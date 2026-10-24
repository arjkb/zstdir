// Package archive packs a directory into a zstd-compressed tar stream and
// unpacks such streams. The format is plain tar inside a zstd frame, so the
// output is interchangeable with `tar --zstd` and `zstd -dc | tar x`.
package archive

import (
	"context"
	"io"
	"io/fs"
	"runtime"
	"sync"
	"syscall"
)

// DefaultLevel is the zstd compression level used when Options.Level is 0.
const DefaultLevel = 3

// Options configures Compress and Extract.
type Options struct {
	// Level is the zstd compression level, 1–22. 0 means DefaultLevel.
	Level int
	// Threads is the number of goroutines used for compression and file
	// I/O. 0 means one per CPU, up to 8.
	Threads int
	// List, if non-nil, receives each entry name, one per line, as it is
	// processed.
	List io.Writer
	// Warn, if non-nil, is called for entries that are skipped.
	Warn func(msg string)
	// Skip, used only by Compress, leaves out any entry for which it
	// returns true.
	Skip func(fs.FileInfo) bool
}

func (o Options) level() int {
	if o.Level == 0 {
		return DefaultLevel
	}
	return o.Level
}

// maxDefaultThreads caps the default thread count. Compression stopped
// getting faster beyond about 8 threads while each one costs ~20 MB.
const maxDefaultThreads = 8

func (o Options) threads() int {
	if o.Threads <= 0 {
		// Unset: one per CPU, but no more than maxDefaultThreads.
		return min(runtime.NumCPU(), maxDefaultThreads)
	}
	return o.Threads // an explicit count is used as given, even above the cap
}

func (o Options) list(name string) {
	if o.List != nil {
		io.WriteString(o.List, name+"\n")
	}
}

func (o Options) warn(msg string) {
	if o.Warn != nil {
		o.Warn(msg)
	}
}

// Stats summarizes the entries processed by Compress or Extract.
type Stats struct {
	Files    int
	Dirs     int
	Symlinks int
	// Bytes is the total size of regular file contents.
	Bytes int64
}

// Magic is the four-byte signature that starts every zstd frame.
var Magic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// Umask returns the process umask. It is read once, since reading it
// requires briefly changing it.
func Umask() fs.FileMode { return umask() }

var umask = sync.OnceValue(func() fs.FileMode {
	m := syscall.Umask(0)
	syscall.Umask(m)
	return fs.FileMode(m)
})

// ctxReader fails reads once ctx is done, so long copies stop promptly on
// interrupt.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
