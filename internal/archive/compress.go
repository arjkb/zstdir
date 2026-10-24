package archive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	// smallFile is the size up to which files are read ahead into memory by
	// the reader goroutines; larger files are streamed from disk.
	smallFile = 1 << 20
	// readAhead bounds how many entries the walker may run ahead of the
	// tar writer, and with smallFile the memory held in read-ahead buffers.
	readAhead = 32
	// maxIOWorkers caps the goroutines reading files on compression and
	// writing them on extraction. More than this made extracting a large
	// source tree slower, not faster.
	maxIOWorkers = 4
)

// Compress writes dir as a zstd-compressed tar stream to w. Entries are
// stored under the directory's base name, as `tar -C parent -cf - name`
// would, so extracting with plain tar recreates the directory itself.
// Symlinks are stored as links, never followed; other special files
// (devices, sockets, FIFOs) are skipped with a warning.
//
// Three stages run concurrently: a walker lists the tree, a few reader
// goroutines load small files ahead of time, and the tar writer consumes
// entries in walk order, handing data to the zstd encoder, which
// compresses blocks on its own goroutines.
func Compress(ctx context.Context, dir string, w io.Writer, opt Options) (Stats, error) {
	var st Stats
	info, err := os.Stat(dir)
	if err != nil {
		return st, err
	}
	if !info.IsDir() {
		return st, fmt.Errorf("%s: not a directory", dir)
	}
	prefix, err := baseName(dir)
	if err != nil {
		return st, err
	}
	// WalkDir does not descend into a root that is itself a symlink; a
	// trailing separator makes the OS resolve it first.
	walkRoot := filepath.Clean(dir)
	if li, err := os.Lstat(walkRoot); err == nil && li.Mode()&fs.ModeSymlink != 0 {
		walkRoot += string(filepath.Separator)
	}

	enc, err := newEncoder(w, opt)
	if err != nil {
		return st, err
	}
	tw := tar.NewWriter(enc)

	wctx, cancel := context.WithCancel(ctx)
	entries, wait := walk(wctx, walkRoot, prefix, opt)
	for e := range entries {
		if err = writeEntry(wctx, tw, e, &st, opt); err != nil {
			break
		}
	}
	// On error, stop the walker and readers before returning.
	cancel()
	for range entries {
	}
	wait()

	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = tw.Close()
	}
	if cerr := enc.Close(); err == nil {
		err = cerr
	}
	return st, err
}

// newEncoder returns a zstd encoder writing to w: a parallelEncoder, or with
// a single thread a plain stream encoder, which produces a single frame.
func newEncoder(w io.Writer, opt Options) (io.WriteCloser, error) {
	level := zstd.EncoderLevelFromZstd(opt.level())
	if opt.threads() == 1 {
		return zstd.NewWriter(w, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1))
	}
	return newParallelEncoder(w, level, opt.threads())
}

// baseName returns the name entries are stored under.
func baseName(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	b := filepath.Base(abs)
	if b == string(filepath.Separator) || b == "." {
		return "", fmt.Errorf("%s: cannot archive the filesystem root", dir)
	}
	return b, nil
}

// entry is one item found by the walker. For small regular files, data and
// readErr are filled in by a reader goroutine, which then closes ready.
type entry struct {
	path string
	hdr  *tar.Header
	warn string // if set, the entry is skipped with this warning
	err  error  // if set, archiving stops with this error

	ready   chan struct{}
	data    []byte
	readErr error
}

// walk lists root in lexical order on the returned channel, which is closed
// when the walk ends. wait blocks until all goroutines have exited.
func walk(ctx context.Context, root, prefix string, opt Options) (<-chan *entry, func()) {
	out := make(chan *entry, readAhead)
	toRead := make(chan *entry)
	var wg sync.WaitGroup

	for range min(opt.threads(), maxIOWorkers) {
		wg.Go(func() {
			for e := range toRead {
				if ctx.Err() == nil {
					e.data, e.readErr = os.ReadFile(e.path)
				} else {
					e.readErr = ctx.Err()
				}
				close(e.ready)
			}
		})
	}

	wg.Go(func() {
		defer close(out)
		defer close(toRead)
		send := func(e *entry) error {
			select {
			case out <- e:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil {
				err = ctx.Err()
			}
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if opt.Skip != nil && opt.Skip(info) {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			name := prefix
			if rel != "." {
				name += "/" + filepath.ToSlash(rel)
			}
			e, err := newEntry(path, name, info)
			if err != nil {
				return err
			}
			if err := send(e); err != nil {
				return err
			}
			if e.ready != nil {
				select {
				case toRead <- e:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
		if err != nil {
			send(&entry{err: err})
		}
	})
	return out, wg.Wait
}

func newEntry(path, name string, info fs.FileInfo) (*entry, error) {
	var link string
	mode := info.Mode()
	switch {
	case mode.IsRegular(), mode.IsDir():
	case mode&fs.ModeSymlink != 0:
		var err error
		if link, err = os.Readlink(path); err != nil {
			return nil, err
		}
	default:
		return &entry{warn: fmt.Sprintf("%s: skipping unsupported file type (%s)", path, mode.Type())}, nil
	}

	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return nil, err
	}
	hdr.Name = name
	// tar would round to the nearest second; truncate like GNU tar so an
	// extracted file never looks newer than its source.
	hdr.ModTime = hdr.ModTime.Truncate(time.Second)
	if mode.IsDir() {
		hdr.Name += "/"
	}
	e := &entry{path: path, hdr: hdr}
	if mode.IsRegular() && hdr.Size <= smallFile {
		e.ready = make(chan struct{})
	}
	return e, nil
}

func writeEntry(ctx context.Context, tw *tar.Writer, e *entry, st *Stats, opt Options) error {
	switch {
	case e.err != nil:
		return e.err
	case e.warn != "":
		opt.warn(e.warn)
		return nil
	}
	hdr := e.hdr

	var src io.Reader
	switch {
	case e.ready != nil:
		select {
		case <-e.ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		if e.readErr != nil {
			return e.readErr
		}
		// Archive what was read, even if the file changed size since
		// it was listed.
		hdr.Size = int64(len(e.data))
		src = &byteReader{e.data}
	case hdr.Typeflag == tar.TypeReg:
		// Open before writing the header so an unreadable file is reported
		// without leaving a half-written entry behind.
		f, err := os.Open(e.path)
		if err != nil {
			return err
		}
		defer f.Close()
		src = ctxReader{ctx, f}
	}

	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	opt.list(hdr.Name)

	switch hdr.Typeflag {
	case tar.TypeDir:
		st.Dirs++
	case tar.TypeSymlink:
		st.Symlinks++
	default:
		n, err := io.CopyN(tw, src, hdr.Size)
		st.Bytes += n
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s: file shrank while being archived", e.path)
		}
		if err != nil {
			return err
		}
		st.Files++
	}
	return nil
}

// byteReader is a minimal bytes.Reader that drops its buffer once read, so
// read-ahead memory is released as soon as an entry is written.
type byteReader struct{ b []byte }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		r.b = nil
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
