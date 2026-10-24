// Command zstdir compresses a directory into a .tar.zst archive, or
// extracts one, in a single step.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/arjkb/zstdir/internal/archive"
	flag "github.com/spf13/pflag"
	"golang.org/x/term"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

const (
	exitOK        = 0
	exitError     = 1
	exitUsage     = 2
	exitInterrupt = 130
)

const usageText = `Usage:
  zstdir [options] DIR...          compress each DIR into DIR.tar.zst
  zstdir [options] ARCHIVE...      extract each archive into a directory
  zstdir -d [options] [ARCHIVE|-]  extract; "-" or no argument reads stdin

The mode is picked automatically: directories are compressed, zstd files
are extracted. Outputs are written next to their inputs and existing
outputs are never overwritten unless -f is given (directories never are).

Options:
`

// config holds the parsed command line.
type config struct {
	decompress bool
	output     string
	stdout     bool
	force      bool
	verbose    int
	quiet      bool
	level      int
	threads    int
}

// app carries the I/O streams so main can be exercised in-process.
type app struct {
	cfg    config
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	// stdoutTTY and stdinTTY report whether the streams are terminals.
	stdoutTTY, stdinTTY bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		// After the first signal, restore default handling so a second one
		// kills the process even if it is blocked reading a pipe.
		<-ctx.Done()
		stop()
	}()
	os.Exit(run(ctx, os.Args, os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin, stdout, stderr *os.File) int {
	a := &app{
		stdin:     stdin,
		stdout:    stdout,
		stderr:    stderr,
		stdoutTTY: term.IsTerminal(int(stdout.Fd())),
		stdinTTY:  term.IsTerminal(int(stdin.Fd())),
	}
	if filepath.Base(args[0]) == "unzstdir" {
		a.cfg.decompress = true
	}

	fset := flag.NewFlagSet("zstdir", flag.ContinueOnError)
	fset.SortFlags = false
	fset.SetOutput(io.Discard)
	fset.BoolVarP(&a.cfg.decompress, "decompress", "d", a.cfg.decompress, "extract archives (also: unzstdir)")
	fset.StringVarP(&a.cfg.output, "output", "o", "", "write to `PATH`: the archive when compressing,\nthe directory when extracting")
	fset.BoolVarP(&a.cfg.stdout, "stdout", "c", false, "write the archive to stdout (compression only)")
	fset.BoolVarP(&a.cfg.force, "force", "f", false, "overwrite an existing archive; allow terminal I/O")
	fset.CountVarP(&a.cfg.verbose, "verbose", "v", "list entries on stderr; -vv adds a summary``")
	fset.BoolVarP(&a.cfg.quiet, "quiet", "q", false, "suppress warnings")
	fset.IntVarP(&a.cfg.level, "level", "L", archive.DefaultLevel, "compression level `N`, 1-22")
	fset.IntVarP(&a.cfg.threads, "threads", "T", 0, "use `N` threads; 0 = one per CPU, up to 8")
	help := fset.BoolP("help", "h", false, "show this help")
	showVersion := fset.BoolP("version", "V", false, "show version")

	if err := fset.Parse(args[1:]); err != nil {
		return a.usageError(err.Error())
	}
	switch {
	case *help:
		fmt.Fprint(a.stdout, usageText+fset.FlagUsages())
		return exitOK
	case *showVersion:
		fmt.Fprintln(a.stdout, "zstdir", getVersion())
		return exitOK
	}

	inputs := fset.Args()
	if len(inputs) == 0 {
		if !a.cfg.decompress {
			return a.usageError("no input given")
		}
		inputs = []string{"-"}
	}
	switch {
	case a.cfg.level < 1 || a.cfg.level > 22:
		return a.usageError("level must be between 1 and 22")
	case a.cfg.threads < 0:
		return a.usageError("threads must not be negative")
	case a.cfg.verbose > 0 && a.cfg.quiet:
		return a.usageError("-v and -q are mutually exclusive")
	case len(inputs) > 1 && a.cfg.output != "":
		return a.usageError("-o cannot be used with multiple inputs")
	case len(inputs) > 1 && a.cfg.stdout:
		return a.usageError("-c cannot be used with multiple inputs")
	case a.cfg.stdout && a.cfg.output != "":
		return a.usageError("-c and -o are mutually exclusive")
	}

	code := exitOK
	for _, in := range inputs {
		err := a.process(ctx, in)
		if ctx.Err() != nil {
			a.errorf("interrupted")
			return exitInterrupt
		}
		if err != nil {
			a.errorf("%v", err)
			code = exitError
		}
	}
	return code
}

// process compresses or extracts a single input, choosing the mode from
// what the input is.
func (a *app) process(ctx context.Context, in string) error {
	if in == "-" {
		if a.cfg.stdout {
			return errors.New("-c is only valid when compressing")
		}
		if a.stdinTTY && !a.cfg.force {
			return errors.New("refusing to read an archive from a terminal; use -f to force")
		}
		return a.extract(ctx, a.stdin, "(stdin)", a.cfg.output)
	}

	info, err := os.Stat(in)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if a.cfg.decompress {
			return fmt.Errorf("%s: is a directory; cannot extract it", in)
		}
		return a.compress(ctx, in)
	}

	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()
	if !isZstd(f) {
		if a.cfg.decompress {
			return fmt.Errorf("%s: not a zstd archive", in)
		}
		return fmt.Errorf("%s: not a directory or a zstd archive", in)
	}
	if a.cfg.stdout {
		return errors.New("-c is only valid when compressing")
	}
	dst := a.cfg.output
	if dst == "" {
		if dst = stripArchiveExt(in); dst == "" {
			return fmt.Errorf("%s: cannot derive an output name; use -o", in)
		}
	}
	return a.extract(ctx, f, in, dst)
}

func (a *app) compress(ctx context.Context, dir string) error {
	start := time.Now()
	opt := a.options()

	if a.cfg.stdout {
		if a.stdoutTTY && !a.cfg.force {
			return errors.New("refusing to write an archive to a terminal; use -f to force")
		}
		cw := &countingWriter{w: a.stdout}
		st, err := archive.Compress(ctx, dir, cw, opt)
		if err != nil {
			return err
		}
		a.summary("Compressed", dir, "(stdout)", st, st.Bytes, cw.n, start)
		return nil
	}

	out := a.cfg.output
	if out == "" {
		base := filepath.Clean(dir)
		if b := filepath.Base(base); b == "." || b == ".." {
			abs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			base = abs
		}
		out = base + ".tar.zst"
	}
	if fi, err := os.Lstat(out); err == nil {
		if fi.IsDir() {
			return fmt.Errorf("%s: output is a directory", out)
		}
		if !a.cfg.force {
			return fmt.Errorf("%s: already exists; use -f to overwrite", out)
		}
	}

	// Write to a temporary file beside the output and rename it into place
	// only once the archive is complete.
	tmp, err := os.CreateTemp(filepath.Dir(out), "."+filepath.Base(out)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	defer tmp.Close()
	tmpInfo, err := tmp.Stat()
	if err != nil {
		return err
	}
	// The temporary file may sit inside the directory being archived.
	opt.Skip = func(fi fs.FileInfo) bool { return os.SameFile(fi, tmpInfo) }

	cw := &countingWriter{w: tmp}
	st, err := archive.Compress(ctx, dir, cw, opt)
	if err != nil {
		return err
	}
	if err := tmp.Chmod(0o666 &^ archive.Umask()); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), out); err != nil {
		return err
	}
	a.summary("Compressed", dir, out, st, st.Bytes, cw.n, start)
	return nil
}

func (a *app) extract(ctx context.Context, r io.Reader, name, dst string) error {
	start := time.Now()
	cr := &countingReader{r: r}
	out, st, err := archive.Extract(ctx, cr, dst, a.options())
	if errors.Is(err, archive.ErrNoTopDir) {
		return fmt.Errorf("%s: %v; use -o", name, err)
	}
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%v; use -o to choose another name", err)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	a.summary("Extracted", name, out+"/", st, cr.n, st.Bytes, start)
	return nil
}

func (a *app) options() archive.Options {
	opt := archive.Options{Level: a.cfg.level, Threads: a.cfg.threads}
	if a.cfg.verbose > 0 {
		opt.List = a.stderr
	}
	if !a.cfg.quiet {
		opt.Warn = func(msg string) { a.errorf("warning: %s", msg) }
	}
	return opt
}

// summary prints a report at -vv, such as
//
//	✔ Compressed photos -> photos.tar.zst
//	  Files: 3 files, 1 dir (0 symlinks)
//	  Size:  2.4 MiB -> 905.0 KiB (37.1% ratio, 62.9% saved)
//	  Time:  27ms (89.1 MiB/s)
//
// src and out name the input and what was written; in and written are the
// sizes before and after.
func (a *app) summary(verb, src, out string, st archive.Stats, in, written int64, start time.Time) {
	if a.cfg.verbose < 2 {
		return
	}
	elapsed := time.Since(start)
	compressed, raw := min(in, written), max(in, written)
	ratio := 0.0
	if raw > 0 {
		ratio = float64(compressed) / float64(raw) * 100
	}
	speed := ""
	if elapsed > 0 {
		speed = fmt.Sprintf(" (%s/s)", humanBytes(int64(float64(raw)/elapsed.Seconds())))
	}
	fmt.Fprintf(a.stderr, "✔ %s %s -> %s\n", verb, src, out)
	fmt.Fprintf(a.stderr, "  Files: %s, %s (%s)\n",
		plural(st.Files, "file"), plural(st.Dirs, "dir"), plural(st.Symlinks, "symlink"))
	fmt.Fprintf(a.stderr, "  Size:  %s -> %s (%.1f%% ratio, %.1f%% saved)\n",
		humanBytes(in), humanBytes(written), ratio, 100-ratio)
	fmt.Fprintf(a.stderr, "  Time:  %s%s\n", elapsed.Round(time.Millisecond), speed)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (a *app) errorf(format string, args ...any) {
	fmt.Fprintf(a.stderr, "zstdir: "+format+"\n", args...)
}

func (a *app) usageError(msg string) int {
	a.errorf("%s", msg)
	fmt.Fprintln(a.stderr, "Try 'zstdir --help' for more information.")
	return exitUsage
}

// isZstd reports whether f starts with the zstd magic number, then rewinds
// it.
func isZstd(f *os.File) bool {
	buf := make([]byte, len(archive.Magic))
	_, err := io.ReadFull(f, buf)
	if _, serr := f.Seek(0, io.SeekStart); serr != nil {
		return false
	}
	return err == nil && bytes.Equal(buf, archive.Magic)
}

// stripArchiveExt maps "x.tar.zst" (and similar) to "x". It returns "" when
// the name has no known suffix.
func stripArchiveExt(p string) string {
	for _, ext := range []string{".tar.zst", ".tar.zstd", ".tzst", ".zst", ".zstd"} {
		if strings.HasSuffix(p, ext) {
			if len(filepath.Base(p)) == len(ext) {
				return ""
			}
			return strings.TrimSuffix(p, ext)
		}
	}
	return ""
}

func getVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
