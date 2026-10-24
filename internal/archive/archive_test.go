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
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// makeTree builds a directory exercising the cases the archiver must
// preserve and returns its path.
func makeTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tree")
	long := strings.Repeat("long-name-", 15) // >100 bytes forces PAX
	files := map[string]string{
		"a.txt":                  "hello\n",
		"empty":                  "",
		"sub/b.txt":              strings.Repeat("zstd ", 10000),
		"sub/deeper/c.bin":       "\x00\x01\x02\xff",
		"ünïcødé/文件.txt":         "unicode\n",
		long + "/" + long + ".x": "long\n",
	}
	for name, data := range files {
		p := filepath.Join(root, name)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(data), 0o644))
	}
	must(t, os.MkdirAll(filepath.Join(root, "emptydir"), 0o755))
	must(t, os.Chmod(filepath.Join(root, "a.txt"), 0o600))
	must(t, os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\n"), 0o755))
	must(t, os.Symlink("a.txt", filepath.Join(root, "link")))
	must(t, os.Symlink("../outside", filepath.Join(root, "dangling")))
	must(t, os.MkdirAll(filepath.Join(root, "ro"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "ro", "f"), []byte("x"), 0o444))
	must(t, os.Chmod(filepath.Join(root, "ro"), 0o555))
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "ro"), 0o755) })

	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	must(t, os.Chtimes(filepath.Join(root, "sub", "b.txt"), old, old))
	must(t, os.Chtimes(filepath.Join(root, "sub"), old, old))
	return root
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func roundTrip(t *testing.T, src string, opt Options) string {
	t.Helper()
	var buf bytes.Buffer
	if _, err := Compress(t.Context(), src, &buf, opt); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	got, _, err := Extract(t.Context(), &buf, dst, opt)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got != dst {
		t.Fatalf("Extract returned %q, want %q", got, dst)
	}
	return dst
}

// compareTrees fails if the two trees differ in names, types, contents,
// permissions, symlink targets or file mtimes.
func compareTrees(t *testing.T, want, got string) {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.WalkDir(want, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(want, p)
		seen[rel] = true
		wi, _ := os.Lstat(p)
		gi, err := os.Lstat(filepath.Join(got, rel))
		if err != nil {
			t.Errorf("%s: missing: %v", rel, err)
			return nil
		}
		if wi.Mode() != gi.Mode() {
			t.Errorf("%s: mode %v, want %v", rel, gi.Mode(), wi.Mode())
		}
		switch {
		case wi.Mode()&fs.ModeSymlink != 0:
			wl, _ := os.Readlink(p)
			gl, _ := os.Readlink(filepath.Join(got, rel))
			if wl != gl {
				t.Errorf("%s: link %q, want %q", rel, gl, wl)
			}
		case wi.Mode().IsRegular():
			wb, _ := os.ReadFile(p)
			gb, _ := os.ReadFile(filepath.Join(got, rel))
			if !bytes.Equal(wb, gb) {
				t.Errorf("%s: contents differ", rel)
			}
			fallthrough
		default:
			if !wi.ModTime().Truncate(time.Second).Equal(gi.ModTime().Truncate(time.Second)) {
				t.Errorf("%s: mtime %v, want %v", rel, gi.ModTime(), wi.ModTime())
			}
		}
		return nil
	})
	must(t, err)
	err = filepath.WalkDir(got, func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(got, p)
		// Some tools rewrite Unicode names into another normalization form;
		// on macOS the lookup in want still succeeds for those.
		if _, err := os.Lstat(filepath.Join(want, rel)); !seen[rel] && err != nil {
			t.Errorf("%s: unexpected extra entry", rel)
		}
		return err
	})
	must(t, err)
}

func TestRoundTrip(t *testing.T) {
	src := makeTree(t)
	for _, opt := range []Options{{}, {Level: 1, Threads: 1}, {Level: 19}} {
		dst := roundTrip(t, src, opt)
		compareTrees(t, src, dst)
		os.Chmod(filepath.Join(dst, "ro"), 0o755) // let TempDir cleanup remove it
	}
}

func TestRoundTripSymlinkedRoot(t *testing.T) {
	src := makeTree(t)
	link := filepath.Join(t.TempDir(), "alias")
	must(t, os.Symlink(src, link))
	var buf bytes.Buffer
	if _, err := Compress(t.Context(), link, &buf, Options{}); err != nil {
		t.Fatal(err)
	}
	names := tarNames(t, buf.Bytes())
	if len(names) < 5 || names[0] != "alias/" {
		t.Fatalf("symlinked root not descended into: %v", names)
	}
}

func TestMultiFrame(t *testing.T) {
	src := filepath.Join(t.TempDir(), "big")
	must(t, os.MkdirAll(src, 0o755))
	data := make([]byte, 3*chunkSize+12345)
	for i := range data {
		data[i] = byte(i * i >> 7)
	}
	must(t, os.WriteFile(filepath.Join(src, "f"), data, 0o644))

	f := filepath.Join(t.TempDir(), "big.tar.zst")
	out, err := os.Create(f)
	must(t, err)
	_, err = Compress(t.Context(), src, out, Options{Threads: 4})
	must(t, err)
	must(t, out.Close())

	in, err := os.Open(f)
	must(t, err)
	defer in.Close()
	compareTrees(t, src, mustExtract(t, in))

	// The output is several frames, which the reference zstd accepts.
	if _, err := exec.LookPath("zstd"); err == nil {
		b, err := exec.Command("zstd", "-l", f).CombinedOutput()
		if err != nil {
			t.Fatalf("zstd -l: %v\n%s", err, b)
		}
		if fields := strings.Fields(strings.Split(string(b), "\n")[1]); fields[0] == "1" {
			t.Errorf("expected several frames:\n%s", b)
		}
		if b, err := exec.Command("zstd", "-t", f).CombinedOutput(); err != nil {
			t.Fatalf("zstd -t: %v\n%s", err, b)
		}
	}
}

func mustExtract(t *testing.T, r io.Reader) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "x")
	_, _, err := Extract(t.Context(), r, dst, Options{})
	must(t, err)
	return dst
}

type failWriter struct{ n int }

func (w *failWriter) Write(p []byte) (int, error) {
	if w.n += len(p); w.n > 100 {
		return 0, errors.New("disk full")
	}
	return len(p), nil
}

func TestCompressWriteError(t *testing.T) {
	src := makeTree(t)
	for _, threads := range []int{1, 4} {
		_, err := Compress(t.Context(), src, &failWriter{}, Options{Threads: threads})
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Errorf("threads=%d: err = %v", threads, err)
		}
	}
}

func TestDeterministic(t *testing.T) {
	src := makeTree(t)
	var a, b bytes.Buffer
	must(t, errOnly(Compress(t.Context(), src, &a, Options{})))
	must(t, errOnly(Compress(t.Context(), src, &b, Options{})))
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("two archives of the same tree differ")
	}
}

func errOnly(_ Stats, err error) error { return err }

func TestStats(t *testing.T) {
	src := filepath.Join(t.TempDir(), "s")
	must(t, os.MkdirAll(filepath.Join(src, "d"), 0o755))
	must(t, os.WriteFile(filepath.Join(src, "f"), []byte("12345"), 0o644))
	must(t, os.Symlink("f", filepath.Join(src, "l")))
	var buf bytes.Buffer
	st, err := Compress(t.Context(), src, &buf, Options{})
	must(t, err)
	want := Stats{Files: 1, Dirs: 2, Symlinks: 1, Bytes: 5}
	if st != want {
		t.Fatalf("Compress stats %+v, want %+v", st, want)
	}
	_, st, err = Extract(t.Context(), &buf, filepath.Join(t.TempDir(), "o"), Options{})
	must(t, err)
	if st != want {
		t.Fatalf("Extract stats %+v, want %+v", st, want)
	}
}

func TestListAndSkip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "s")
	must(t, os.MkdirAll(src, 0o755))
	must(t, os.WriteFile(filepath.Join(src, "keep"), nil, 0o644))
	must(t, os.WriteFile(filepath.Join(src, "drop"), nil, 0o644))
	drop, _ := os.Stat(filepath.Join(src, "drop"))
	var list strings.Builder
	var buf bytes.Buffer
	_, err := Compress(t.Context(), src, &buf, Options{
		List: &list,
		Skip: func(fi fs.FileInfo) bool { return os.SameFile(fi, drop) },
	})
	must(t, err)
	if got, want := list.String(), "s/\ns/keep\n"; got != want {
		t.Fatalf("listed %q, want %q", got, want)
	}
}

func TestSkipsSpecialFiles(t *testing.T) {
	src := filepath.Join(t.TempDir(), "s")
	must(t, os.MkdirAll(src, 0o755))
	if err := mkfifo(filepath.Join(src, "fifo")); err != nil {
		t.Skip("mkfifo unavailable:", err)
	}
	var warned []string
	var buf bytes.Buffer
	_, err := Compress(t.Context(), src, &buf, Options{Warn: func(m string) { warned = append(warned, m) }})
	must(t, err)
	if len(warned) != 1 || !strings.Contains(warned[0], "fifo") {
		t.Fatalf("warnings = %q", warned)
	}
	if names := tarNames(t, buf.Bytes()); len(names) != 1 {
		t.Fatalf("fifo archived: %v", names)
	}
}

func TestCompressErrors(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	must(t, os.WriteFile(f, nil, 0o644))
	if _, err := Compress(t.Context(), f, io.Discard, Options{}); err == nil {
		t.Error("compressing a file: want error")
	}
	if _, err := Compress(t.Context(), "/", io.Discard, Options{}); err == nil {
		t.Error("compressing /: want error")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Compress(ctx, makeTree(t), io.Discard, Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v", err)
	}
}

// buildArchive creates a .tar.zst in memory from raw headers, for archives
// this package would never produce itself.
func buildArchive(t *testing.T, hdrs ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	must(t, err)
	tw := tar.NewWriter(enc)
	for _, h := range hdrs {
		if h.Typeflag == tar.TypeReg && h.Mode == 0 {
			h.Mode = 0o644
		}
		if h.Typeflag == tar.TypeDir && h.Mode == 0 {
			h.Mode = 0o755
		}
		must(t, tw.WriteHeader(h))
		if h.Size > 0 {
			_, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
			must(t, err)
		}
	}
	must(t, tw.Close())
	must(t, enc.Close())
	return buf.Bytes()
}

func tarNames(t *testing.T, archive []byte) []string {
	t.Helper()
	dec, err := zstd.NewReader(bytes.NewReader(archive))
	must(t, err)
	defer dec.Close()
	tr := tar.NewReader(dec)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		must(t, err)
		names = append(names, h.Name)
	}
}

func TestExtractRejectsEscapes(t *testing.T) {
	cases := map[string][]*tar.Header{
		"dotdot":   {{Name: "../evil", Typeflag: tar.TypeReg, Size: 1}},
		"nested":   {{Name: "a/../../evil", Typeflag: tar.TypeReg, Size: 1}},
		"absolute": {{Name: "/tmp/evil", Typeflag: tar.TypeReg, Size: 1}},
		"symlink-dir": {
			{Name: "a/", Typeflag: tar.TypeDir},
			{Name: "a/out", Typeflag: tar.TypeSymlink, Linkname: "../../"},
			{Name: "a/out/evil", Typeflag: tar.TypeReg, Size: 1},
		},
		"symlink-abs": {
			{Name: "a/", Typeflag: tar.TypeDir},
			{Name: "a/out", Typeflag: tar.TypeSymlink, Linkname: "/tmp"},
			{Name: "a/out/evil", Typeflag: tar.TypeReg, Size: 1},
		},
		"hardlink": {
			{Name: "a/", Typeflag: tar.TypeDir},
			{Name: "a/h", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd"},
		},
	}
	for name, hdrs := range cases {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			dst := filepath.Join(base, "work", "out")
			must(t, os.MkdirAll(filepath.Dir(dst), 0o755))
			_, _, err := Extract(t.Context(), bytes.NewReader(buildArchive(t, hdrs...)), dst, Options{})
			if err == nil {
				t.Fatal("want error")
			}
			// Nothing may be left anywhere under base apart from the
			// (empty) work directory.
			filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
				if p != base && p != filepath.Dir(dst) {
					t.Errorf("left behind: %s", p)
				}
				return nil
			})
			if _, err := os.Stat("/tmp/evil"); err == nil {
				t.Error("/tmp/evil was written")
			}
		})
	}
}

func TestExtractPlacement(t *testing.T) {
	single := buildArchive(t,
		&tar.Header{Name: "proj/", Typeflag: tar.TypeDir},
		&tar.Header{Name: "proj/f", Typeflag: tar.TypeReg, Size: 3})
	implicit := buildArchive(t, &tar.Header{Name: "./proj/f", Typeflag: tar.TypeReg, Size: 3})
	multi := buildArchive(t,
		&tar.Header{Name: "f1", Typeflag: tar.TypeReg, Size: 1},
		&tar.Header{Name: "f2", Typeflag: tar.TypeReg, Size: 1})
	lone := buildArchive(t, &tar.Header{Name: "only", Typeflag: tar.TypeReg, Size: 1})

	t.Run("single dir renamed to dst", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "renamed")
		_, _, err := Extract(t.Context(), bytes.NewReader(single), dst, Options{})
		must(t, err)
		assertFile(t, filepath.Join(dst, "f"))
	})
	t.Run("implicit top dir", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "x")
		_, _, err := Extract(t.Context(), bytes.NewReader(implicit), dst, Options{})
		must(t, err)
		assertFile(t, filepath.Join(dst, "f"))
	})
	t.Run("multiple entries wrapped in dst", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "x")
		_, _, err := Extract(t.Context(), bytes.NewReader(multi), dst, Options{})
		must(t, err)
		assertFile(t, filepath.Join(dst, "f1"))
		assertFile(t, filepath.Join(dst, "f2"))
		fi, _ := os.Stat(dst)
		if want := 0o777 &^ Umask(); fi.Mode().Perm() != want {
			t.Errorf("wrapper dir mode %v, want %v", fi.Mode().Perm(), want)
		}
	})
	t.Run("single file wrapped in dst", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "x")
		_, _, err := Extract(t.Context(), bytes.NewReader(lone), dst, Options{})
		must(t, err)
		assertFile(t, filepath.Join(dst, "only"))
	})
	t.Run("empty dst uses top dir name", func(t *testing.T) {
		t.Chdir(t.TempDir())
		got, _, err := Extract(t.Context(), bytes.NewReader(single), "", Options{})
		must(t, err)
		if got != "proj" {
			t.Fatalf("got %q", got)
		}
		assertFile(t, filepath.Join("proj", "f"))
	})
	t.Run("empty dst without top dir", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		_, _, err := Extract(t.Context(), bytes.NewReader(multi), "", Options{})
		if !errors.Is(err, ErrNoTopDir) {
			t.Fatalf("err = %v", err)
		}
		assertEmpty(t, dir)
	})
	t.Run("existing dst", func(t *testing.T) {
		dst := t.TempDir()
		_, _, err := Extract(t.Context(), bytes.NewReader(single), dst, Options{})
		if !errors.Is(err, fs.ErrExist) {
			t.Fatalf("err = %v", err)
		}
		assertEmpty(t, dst)
	})
}

func TestExtractLaterEntryReplacesEarlier(t *testing.T) {
	data := buildArchive(t,
		&tar.Header{Name: "d/f", Typeflag: tar.TypeReg, Size: 1},
		&tar.Header{Name: "d/f", Typeflag: tar.TypeReg, Size: 4})
	dst := filepath.Join(t.TempDir(), "x")
	_, _, err := Extract(t.Context(), bytes.NewReader(data), dst, Options{})
	must(t, err)
	if b, _ := os.ReadFile(filepath.Join(dst, "f")); string(b) != "xxxx" {
		t.Fatalf("contents %q", b)
	}
}

func TestExtractThroughSymlinkedDir(t *testing.T) {
	data := buildArchive(t,
		&tar.Header{Name: "d/", Typeflag: tar.TypeDir},
		&tar.Header{Name: "d/real/", Typeflag: tar.TypeDir},
		&tar.Header{Name: "d/link", Typeflag: tar.TypeSymlink, Linkname: "real"},
		&tar.Header{Name: "d/link/f", Typeflag: tar.TypeReg, Size: 1},
		&tar.Header{Name: "d/link/sub/g", Typeflag: tar.TypeReg, Size: 1})
	dst := filepath.Join(t.TempDir(), "x")
	_, _, err := Extract(t.Context(), bytes.NewReader(data), dst, Options{})
	must(t, err)
	assertFile(t, filepath.Join(dst, "real", "f"))
	assertFile(t, filepath.Join(dst, "real", "sub", "g"))
}

func TestExtractSymlinkReplacedByFile(t *testing.T) {
	// d/s/x goes through the symlink; once d/s is replaced by a file,
	// d/s/y must not land in the old target via a cached handle.
	data := buildArchive(t,
		&tar.Header{Name: "d/sub/", Typeflag: tar.TypeDir},
		&tar.Header{Name: "d/s", Typeflag: tar.TypeSymlink, Linkname: "sub"},
		&tar.Header{Name: "d/s/x", Typeflag: tar.TypeReg, Size: 1},
		&tar.Header{Name: "d/s", Typeflag: tar.TypeReg, Size: 2},
		&tar.Header{Name: "d/s/y", Typeflag: tar.TypeReg, Size: 1})
	parent := t.TempDir()
	_, _, err := Extract(t.Context(), bytes.NewReader(data), filepath.Join(parent, "x"), Options{})
	if err == nil {
		t.Fatal("want error writing below a regular file")
	}
	assertEmpty(t, parent)
}

func TestExtractManySmallFiles(t *testing.T) {
	// Enough files across enough directories to exercise the write pool
	// and directory-cache eviction, plus a rewrite of an earlier name.
	var hdrs []*tar.Header
	for d := range 40 {
		for f := range 30 {
			hdrs = append(hdrs, &tar.Header{
				Name: fmt.Sprintf("top/d%02d/f%02d", d, f), Typeflag: tar.TypeReg, Size: int64(f),
			})
		}
	}
	hdrs = append(hdrs, &tar.Header{Name: "top/d00/f05", Typeflag: tar.TypeReg, Size: 99})
	dst := filepath.Join(t.TempDir(), "x")
	_, st, err := Extract(t.Context(), bytes.NewReader(buildArchive(t, hdrs...)), dst, Options{Threads: 4})
	must(t, err)
	if st.Files != len(hdrs) {
		t.Errorf("files = %d, want %d", st.Files, len(hdrs))
	}
	for d := range 40 {
		for f := range 30 {
			want := int64(f)
			if d == 0 && f == 5 {
				want = 99
			}
			fi, err := os.Stat(filepath.Join(dst, fmt.Sprintf("d%02d", d), fmt.Sprintf("f%02d", f)))
			if err != nil || fi.Size() != want {
				t.Fatalf("d%02d/f%02d: %v, size %v, want %d", d, f, err, fi, want)
			}
		}
	}
}

func TestExtractHardlink(t *testing.T) {
	data := buildArchive(t,
		&tar.Header{Name: "d/f", Typeflag: tar.TypeReg, Size: 2},
		&tar.Header{Name: "d/h", Typeflag: tar.TypeLink, Linkname: "d/f"})
	dst := filepath.Join(t.TempDir(), "x")
	_, _, err := Extract(t.Context(), bytes.NewReader(data), dst, Options{})
	must(t, err)
	a, _ := os.Stat(filepath.Join(dst, "f"))
	b, _ := os.Stat(filepath.Join(dst, "h"))
	if !os.SameFile(a, b) {
		t.Fatal("hard link not preserved")
	}
}

func TestExtractSkipsAppleDouble(t *testing.T) {
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	must(t, err)
	tw := tar.NewWriter(enc)
	for _, e := range []struct{ name, body string }{
		{"._proj", "\x00\x05\x16\x07appledouble"},
		{"proj/._f", "\x00\x05\x16\x07appledouble"},
		{"proj/f", "data"},
		{"proj/._real", "not appledouble"},
		{"proj/._x", "ab"}, // shorter than the magic
	} {
		must(t, tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(e.body))}))
		_, err := tw.Write([]byte(e.body))
		must(t, err)
	}
	must(t, tw.Close())
	must(t, enc.Close())

	dst := filepath.Join(t.TempDir(), "x")
	_, st, err := Extract(t.Context(), &buf, dst, Options{})
	must(t, err)
	if st.Files != 3 {
		t.Errorf("extracted %d files, want 3", st.Files)
	}
	for name, want := range map[string]string{"f": "data", "._real": "not appledouble", "._x": "ab"} {
		if b, err := os.ReadFile(filepath.Join(dst, name)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", name, b, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "._f")); err == nil {
		t.Error("AppleDouble file ._f was extracted")
	}
}

func TestExtractCorrupt(t *testing.T) {
	good := buildArchive(t, &tar.Header{Name: "d/f", Typeflag: tar.TypeReg, Size: 100000})
	for name, data := range map[string][]byte{
		"truncated": good[:len(good)/2],
		"garbage":   append(slices.Clone(Magic), bytes.Repeat([]byte{7}, 100)...),
		"flipped":   flipLast(good),
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			_, _, err := Extract(t.Context(), bytes.NewReader(data), filepath.Join(parent, "x"), Options{})
			if err == nil {
				t.Fatal("want error")
			}
			assertEmpty(t, parent)
		})
	}
}

// flipLast corrupts the frame checksum at the end of a zstd stream.
func flipLast(b []byte) []byte {
	c := slices.Clone(b)
	c[len(c)-1] ^= 0xff
	return c
}

func assertFile(t *testing.T, p string) {
	t.Helper()
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("%s: not a regular file (%v)", p, err)
	}
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		t.Errorf("left behind in %s: %s", dir, e.Name())
	}
}

// Interop with the system tar, in both directions.
func TestInteropWithTar(t *testing.T) {
	if exec.Command("tar", "--zstd", "-cf", os.DevNull, "-T", os.DevNull).Run() != nil {
		t.Skip("system tar lacks --zstd")
	}
	src := makeTree(t)
	parent := filepath.Dir(src)

	t.Run("tar extracts ours", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "a.tar.zst")
		out, err := os.Create(f)
		must(t, err)
		_, err = Compress(t.Context(), src, out, Options{})
		must(t, err)
		must(t, out.Close())
		dst := t.TempDir()
		if b, err := exec.Command("tar", "--zstd", "-xf", f, "-C", dst).CombinedOutput(); err != nil {
			t.Fatalf("tar: %v\n%s", err, b)
		}
		compareTrees(t, src, filepath.Join(dst, "tree"))
		os.Chmod(filepath.Join(dst, "tree", "ro"), 0o755)
	})
	t.Run("we extract tar's", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "b.tar.zst")
		if b, err := exec.Command("tar", "--zstd", "-cf", f, "-C", parent, "tree").CombinedOutput(); err != nil {
			t.Fatalf("tar: %v\n%s", err, b)
		}
		in, err := os.Open(f)
		must(t, err)
		defer in.Close()
		dst := filepath.Join(t.TempDir(), "x")
		_, _, err = Extract(t.Context(), in, dst, Options{})
		must(t, err)
		compareTrees(t, src, dst)
		os.Chmod(filepath.Join(dst, "ro"), 0o755)
	})
}
