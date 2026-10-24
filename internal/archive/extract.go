package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// ErrNoTopDir is returned by Extract when dst is empty and the archive does
// not consist of a single top-level directory to name the output after.
var ErrNoTopDir = errors.New("archive has no single top-level directory to name the output after")

// Extract unpacks the zstd-compressed tar stream r into a new directory
// dst and returns its path.
//
// If the archive holds a single top-level directory, that directory's
// contents become dst; otherwise all entries are placed inside dst. If dst
// is empty, the single top-level directory is extracted under its own name
// in the current directory.
//
// dst must not exist. Entries are first unpacked into a hidden temporary
// directory next to dst, which is renamed into place only on success, so a
// failed or interrupted extraction leaves nothing behind. All writes go
// through an os.Root, so entries cannot escape it via "..", absolute paths
// or symlinks.
func Extract(ctx context.Context, r io.Reader, dst string, opt Options) (string, Stats, error) {
	var st Stats
	parent := "."
	if dst != "" {
		parent = filepath.Dir(dst)
		if err := checkFree(dst); err != nil {
			return "", st, err
		}
	}
	umask := Umask()

	tmp, err := os.MkdirTemp(parent, ".zstdir-*")
	if err != nil {
		return "", st, err
	}
	defer os.RemoveAll(tmp)

	if st, err = unpack(ctx, r, tmp, umask, opt); err != nil {
		return "", st, err
	}

	// Decide what becomes dst: the single top-level directory, or tmp
	// itself.
	ents, err := os.ReadDir(tmp)
	if err != nil {
		return "", st, err
	}
	src := tmp
	if len(ents) == 1 && ents[0].IsDir() {
		src = filepath.Join(tmp, ents[0].Name())
		if dst == "" {
			dst = ents[0].Name()
		}
	} else {
		if dst == "" {
			return "", st, ErrNoTopDir
		}
		if err := os.Chmod(tmp, 0o777&^umask); err != nil {
			return "", st, err
		}
	}
	// Re-check: dst may have appeared while extracting, and rename would
	// silently replace an empty directory.
	if err := checkFree(dst); err != nil {
		return "", st, err
	}
	if err := os.Rename(src, dst); err != nil {
		return "", st, err
	}
	return dst, st, nil
}

// checkFree returns an error unless nothing exists at p.
func checkFree(p string) error {
	_, err := os.Lstat(p)
	switch {
	case err == nil:
		return &fs.PathError{Op: "extract", Path: p, Err: fs.ErrExist}
	case errors.Is(err, fs.ErrNotExist):
		return nil
	}
	return err
}

type dirMeta struct {
	name  string
	mode  fs.FileMode
	mtime time.Time
}

// unpack extracts every entry of r into the directory dir.
//
// Small files are handed to a writePool so several are written at once;
// everything else happens in archive order on this goroutine. Before an
// entry that could depend on a pending write (one reusing its name, or a
// hard link) the pool is drained, which keeps tar's sequential semantics.
func unpack(ctx context.Context, r io.Reader, dir string, umask fs.FileMode, opt Options) (st Stats, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return st, err
	}
	dc := newDirCache(root)
	defer dc.close()
	pool := newWritePool(ctx, min(opt.threads(), maxIOWorkers))
	defer func() {
		if werr := pool.close(); err == nil {
			err = werr
		}
	}()

	dec, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(opt.threads()))
	if err != nil {
		return st, err
	}
	defer dec.Close()
	tr := tar.NewReader(dec)

	// Directory modes and mtimes are applied last: writing entries inside a
	// directory changes its mtime, and a read-only mode would block them.
	var dirs []dirMeta
	// Names of the symlinks extracted so far.
	symlinks := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		if err := pool.failed(); err != nil {
			return st, err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, err
		}
		name, err := cleanName(hdr.Name)
		if err != nil {
			return st, err
		}
		if name == "" {
			continue // "./" entry
		}
		if pool.pending(name) {
			if err := pool.wait(); err != nil {
				return st, err
			}
		}
		perm := hdr.FileInfo().Mode().Perm() &^ umask

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := dc.mkdir(name, 0o700); err != nil {
				return st, err
			}
			dirs = append(dirs, dirMeta{name, perm, hdr.ModTime})
			st.Dirs++
		case tar.TypeReg:
			body, isAppleDouble, err := sniffAppleDouble(name, tr)
			if err != nil {
				return st, err
			}
			if isAppleDouble {
				continue
			}
			// Only an entry this extraction created can be in the way. A
			// regular file there is replaced by writeFile; a symlink needs
			// the cache invalidation in prepare.
			var sub *sharedRoot
			base := path.Base(name)
			if symlinks[name] {
				sub, base, err = dc.prepare(name)
				delete(symlinks, name)
			} else {
				sub, err = dc.get(path.Dir(name))
			}
			if err != nil {
				return st, err
			}
			if hdr.Size <= smallFile {
				data, err := io.ReadAll(body)
				if err != nil {
					return st, err
				}
				pool.add(writeJob{name, sub, base, data, perm, hdr.ModTime})
				st.Bytes += int64(len(data))
			} else {
				n, err := writeFile(sub.Root, base, ctxReader{ctx, body}, perm, hdr.ModTime)
				st.Bytes += n
				if err != nil {
					return st, err
				}
			}
			st.Files++
		case tar.TypeSymlink:
			sub, base, err := dc.prepare(name)
			if err != nil {
				return st, err
			}
			if err := sub.Symlink(hdr.Linkname, base); err != nil {
				return st, err
			}
			symlinks[name] = true
			st.Symlinks++
		case tar.TypeLink:
			target, err := cleanName(hdr.Linkname)
			if err != nil {
				return st, err
			}
			if err := pool.wait(); err != nil {
				return st, err
			}
			if _, _, err := dc.prepare(name); err != nil {
				return st, err
			}
			delete(symlinks, name)
			if err := root.Link(target, name); err != nil {
				return st, err
			}
			st.Files++
		default:
			opt.warn(fmt.Sprintf("%s: skipping unsupported entry type %q", hdr.Name, hdr.Typeflag))
			continue
		}
		opt.list(hdr.Name)
	}

	// Read to the end of the zstd stream so its checksum is verified; tar
	// stops at its end-of-archive marker.
	if _, err := io.Copy(io.Discard, dec); err != nil {
		return st, err
	}
	if err := pool.wait(); err != nil {
		return st, err
	}

	// Deepest first, so a read-only parent does not block its children.
	slices.Reverse(dirs)
	for _, d := range dirs {
		if err := root.Chmod(d.name, d.mode); err != nil {
			return st, err
		}
		if err := root.Chtimes(d.name, d.mtime, d.mtime); err != nil {
			return st, err
		}
	}
	return st, nil
}

// writeFile creates base in dir with the contents of r, then sets its mode
// and mtime. An existing file at base is replaced, as a later tar entry
// replaces an earlier one; an existing directory is an error.
func writeFile(dir *os.Root, base string, r io.Reader, perm fs.FileMode, mtime time.Time) (int64, error) {
	const flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	f, err := dir.OpenFile(base, flags, 0o600)
	if errors.Is(err, fs.ErrExist) {
		if fi, lerr := dir.Lstat(base); lerr == nil && fi.IsDir() {
			return 0, fmt.Errorf("%s: archive replaces a directory with a file", base)
		}
		if err = dir.Remove(base); err == nil {
			f, err = dir.OpenFile(base, flags, 0o600)
		}
	}
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = dir.Chmod(base, perm)
	}
	if err == nil {
		err = dir.Chtimes(base, mtime, mtime)
	}
	return n, err
}

// cleanName validates an archive path and returns it in clean, relative
// form. It returns "" for the archive root ("./").
func cleanName(name string) (string, error) {
	n := path.Clean(name)
	switch {
	case strings.HasPrefix(n, "/"):
		return "", fmt.Errorf("%s: refusing to extract absolute path", name)
	case n == ".." || strings.HasPrefix(n, "../"):
		return "", fmt.Errorf("%s: refusing to extract path outside the target directory", name)
	case n == ".":
		return "", nil
	}
	return n, nil
}

var appleDoubleMagic = []byte{0x00, 0x05, 0x16, 0x07}

// sniffAppleDouble detects the "._name" AppleDouble files that macOS tar
// adds to carry extended attributes. They are metadata, not content, and
// would otherwise litter the output (and defeat the single-top-level-dir
// detection), so they are skipped. A file is only treated as AppleDouble if
// both its name and its magic number match. It returns a reader for the
// full entry body.
func sniffAppleDouble(name string, r io.Reader) (io.Reader, bool, error) {
	if !strings.HasPrefix(path.Base(name), "._") {
		return r, false, nil
	}
	head := make([]byte, len(appleDoubleMagic))
	n, err := io.ReadFull(r, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, false, err
	}
	head = head[:n]
	if bytes.Equal(head, appleDoubleMagic) {
		return nil, true, nil
	}
	return io.MultiReader(bytes.NewReader(head), r), false, nil
}
