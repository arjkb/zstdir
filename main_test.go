package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var bin string // path to the zstdir binary built for the tests

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "zstdir-test-bin")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "zstdir")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	if err := os.Symlink("zstdir", filepath.Join(dir, "unzstdir")); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type result struct {
	code           int
	stdout, stderr string
}

// zstdir runs the binary in dir with the given stdin.
func zstdir(t *testing.T, dir string, stdin []byte, args ...string) result {
	t.Helper()
	return runBin(t, bin, dir, stdin, args...)
}

func runBin(t *testing.T, path, dir string, stdin []byte, args ...string) result {
	t.Helper()
	cmd := exec.Command(path, args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{code, out.String(), errb.String()}
}

func (r result) want(t *testing.T, code int) result {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", r.code, code, r.stdout, r.stderr)
	}
	return r
}

// workdir returns a temp dir containing proj/{a.txt,sub/b.txt}.
func workdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "proj", "a.txt"), "alpha\n")
	write(t, filepath.Join(dir, "proj", "sub", "b.txt"), strings.Repeat("beta ", 1000))
	return dir
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestCompressExtractAutoDetect(t *testing.T) {
	dir := workdir(t)
	r := zstdir(t, dir, nil, "proj").want(t, 0)
	if r.stdout != "" || r.stderr != "" {
		t.Fatalf("not quiet: stdout %q stderr %q", r.stdout, r.stderr)
	}
	if !exists(filepath.Join(dir, "proj.tar.zst")) {
		t.Fatal("proj.tar.zst not created")
	}
	os.RemoveAll(filepath.Join(dir, "proj"))
	zstdir(t, dir, nil, "proj.tar.zst").want(t, 0)
	if got := read(t, filepath.Join(dir, "proj", "a.txt")); got != "alpha\n" {
		t.Fatalf("a.txt = %q", got)
	}
}

func TestTrailingSlashAndPaths(t *testing.T) {
	dir := workdir(t)
	zstdir(t, dir, nil, "proj/").want(t, 0)
	if !exists(filepath.Join(dir, "proj.tar.zst")) {
		t.Fatal("proj/ did not produce proj.tar.zst")
	}
	// Outputs go next to the input, not into the working directory.
	other := t.TempDir()
	zstdir(t, other, nil, "-f", filepath.Join(dir, "proj")).want(t, 0)
	if exists(filepath.Join(other, "proj.tar.zst")) {
		t.Fatal("archive written to cwd instead of next to input")
	}
	// "." archives the current directory next to it.
	zstdir(t, filepath.Join(dir, "proj", "sub"), nil, ".").want(t, 0)
	if !exists(filepath.Join(dir, "proj", "sub.tar.zst")) {
		t.Fatal(". did not produce ../sub.tar.zst")
	}
}

func TestOutputFlag(t *testing.T) {
	dir := workdir(t)
	zstdir(t, dir, nil, "-o", "backup.tar.zst", "proj").want(t, 0)
	zstdir(t, dir, nil, "-o", "restored", "backup.tar.zst").want(t, 0)
	if got := read(t, filepath.Join(dir, "restored", "a.txt")); got != "alpha\n" {
		t.Fatalf("a.txt = %q", got)
	}
	// The output may live inside the directory being archived.
	zstdir(t, dir, nil, "-o", "proj/self.tar.zst", "proj").want(t, 0)
	zstdir(t, dir, nil, "-o", "self", "proj/self.tar.zst").want(t, 0)
	if exists(filepath.Join(dir, "self", "self.tar.zst")) {
		t.Fatal("archive contains itself")
	}
}

func TestStdoutAndStdin(t *testing.T) {
	dir := workdir(t)
	r := zstdir(t, dir, nil, "-c", "proj").want(t, 0)
	if !strings.HasPrefix(r.stdout, "\x28\xb5\x2f\xfd") {
		t.Fatal("stdout is not a zstd stream")
	}
	out := t.TempDir()
	zstdir(t, out, []byte(r.stdout), "-d").want(t, 0)
	if got := read(t, filepath.Join(out, "proj", "a.txt")); got != "alpha\n" {
		t.Fatalf("a.txt = %q", got)
	}
	// "-" also reads stdin, and -o names the result.
	zstdir(t, out, []byte(r.stdout), "-o", "named", "-").want(t, 0)
	if !exists(filepath.Join(out, "named", "a.txt")) {
		t.Fatal("-o with stdin ignored")
	}
}

func TestUnzstdirAlias(t *testing.T) {
	dir := workdir(t)
	zstdir(t, dir, nil, "proj").want(t, 0)
	os.RemoveAll(filepath.Join(dir, "proj"))
	runBin(t, filepath.Join(filepath.Dir(bin), "unzstdir"), dir, nil, "proj.tar.zst").want(t, 0)
	if !exists(filepath.Join(dir, "proj", "a.txt")) {
		t.Fatal("unzstdir did not extract")
	}
	r := runBin(t, filepath.Join(filepath.Dir(bin), "unzstdir"), dir, nil, "proj").want(t, 1)
	if !strings.Contains(r.stderr, "is a directory") {
		t.Fatalf("stderr = %q", r.stderr)
	}
}

func TestNoClobber(t *testing.T) {
	dir := workdir(t)
	zstdir(t, dir, nil, "proj").want(t, 0)
	r := zstdir(t, dir, nil, "proj").want(t, 1)
	if !strings.Contains(r.stderr, "already exists") {
		t.Fatalf("stderr = %q", r.stderr)
	}
	write(t, filepath.Join(dir, "proj", "new.txt"), "new")
	zstdir(t, dir, nil, "-f", "proj").want(t, 0)

	// Extraction never replaces an existing directory, even with -f.
	r = zstdir(t, dir, nil, "-f", "proj.tar.zst").want(t, 1)
	if !strings.Contains(r.stderr, "exists") {
		t.Fatalf("stderr = %q", r.stderr)
	}
	if got := read(t, filepath.Join(dir, "proj", "a.txt")); got != "alpha\n" {
		t.Fatal("existing directory modified")
	}
	// No temporary files are left behind.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}

func TestVerbose(t *testing.T) {
	dir := workdir(t)
	r := zstdir(t, dir, nil, "-v", "proj").want(t, 0)
	if want := "proj/\nproj/a.txt\nproj/sub/\nproj/sub/b.txt\n"; r.stderr != want {
		t.Fatalf("-v stderr = %q, want %q", r.stderr, want)
	}
	r = zstdir(t, dir, nil, "-vv", "-o", "x", "proj.tar.zst").want(t, 0)
	if !strings.Contains(r.stderr, "\n✔ Extracted proj.tar.zst -> x/\n  Files: 2 files, 2 dirs (0 symlinks)\n") ||
		!strings.Contains(r.stderr, "  Size:  ") || !strings.Contains(r.stderr, "% saved)\n  Time:  ") {
		t.Fatalf("-vv stderr = %q", r.stderr)
	}
	if r.stdout != "" {
		t.Fatalf("verbose output on stdout: %q", r.stdout)
	}
}

func TestMultipleInputs(t *testing.T) {
	dir := workdir(t)
	write(t, filepath.Join(dir, "two", "f"), "2")
	r := zstdir(t, dir, nil, "proj", "missing", "two").want(t, 1)
	if !strings.Contains(r.stderr, "missing") {
		t.Fatalf("stderr = %q", r.stderr)
	}
	// The failure in the middle does not stop the other inputs.
	if !exists(filepath.Join(dir, "proj.tar.zst")) || !exists(filepath.Join(dir, "two.tar.zst")) {
		t.Fatal("not all valid inputs were processed")
	}
}

func TestErrors(t *testing.T) {
	dir := workdir(t)
	write(t, filepath.Join(dir, "plain.txt"), "not an archive")
	write(t, filepath.Join(dir, "noext"), "\x28\xb5\x2f\xfdjunk")
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{nil, 2, "no input"},
		{[]string{"--bogus"}, 2, "unknown flag"},
		{[]string{"-L", "0", "proj"}, 2, "level"},
		{[]string{"-L", "23", "proj"}, 2, "level"},
		{[]string{"-T", "-1", "proj"}, 2, "threads"},
		{[]string{"-v", "-q", "proj"}, 2, "mutually exclusive"},
		{[]string{"-o", "x", "proj", "proj"}, 2, "multiple inputs"},
		{[]string{"-c", "proj", "proj"}, 2, "multiple inputs"},
		{[]string{"-c", "-o", "x", "proj"}, 2, "mutually exclusive"},
		{[]string{"plain.txt"}, 1, "not a directory or a zstd archive"},
		{[]string{"-d", "plain.txt"}, 1, "not a zstd archive"},
		{[]string{"noext"}, 1, "use -o"},
		{[]string{"-o", "y", "noext"}, 1, "noext"},
		{[]string{"nope"}, 1, "no such file"},
	}
	for _, c := range cases {
		r := zstdir(t, dir, nil, c.args...)
		if r.code != c.code || !strings.Contains(r.stderr, c.msg) || !strings.HasPrefix(r.stderr, "zstdir: ") {
			t.Errorf("zstdir %q: exit %d stderr %q; want exit %d containing %q", c.args, r.code, r.stderr, c.code, c.msg)
		}
	}
	if exists(filepath.Join(dir, "y")) {
		t.Error("corrupt archive left output behind")
	}
}

func TestHelpAndVersion(t *testing.T) {
	r := zstdir(t, t.TempDir(), nil, "--help").want(t, 0)
	if !strings.Contains(r.stdout, "Usage:") || r.stderr != "" {
		t.Fatalf("help: stdout %q stderr %q", r.stdout, r.stderr)
	}
	r = zstdir(t, t.TempDir(), nil, "-V").want(t, 0)
	if !strings.HasPrefix(r.stdout, "zstdir ") {
		t.Fatalf("version: %q", r.stdout)
	}
}

func TestInterrupt(t *testing.T) {
	dir := t.TempDir()
	// Enough incompressible data that compression is still running when the
	// signal arrives.
	big := make([]byte, 64<<20)
	for i := range big {
		big[i] = byte(i*7919 + i>>8)
	}
	if err := os.MkdirAll(filepath.Join(dir, "big"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		if err := os.WriteFile(filepath.Join(dir, "big", string(rune('a'+i))), big, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bin, "-L", "19", "big")
	cmd.Dir = dir
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGINT)
	err := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Skipf("finished before the signal arrived (%v)", err)
	}
	if ee.ExitCode() != 130 {
		t.Fatalf("exit %d, want 130; stderr %q", ee.ExitCode(), errb.String())
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("left behind: %v", ents)
	}
}

func TestStripArchiveExt(t *testing.T) {
	for in, want := range map[string]string{
		"a.tar.zst":       "a",
		"dir/a.tar.zstd":  "dir/a",
		"a.tzst":          "a",
		"a.zst":           "a",
		"a.tar":           "",
		".tar.zst":        "",
		"dir/.zst":        "",
		"weird.tar.zst.x": "",
	} {
		if got := stripArchiveExt(in); got != want {
			t.Errorf("stripArchiveExt(%q) = %q, want %q", in, got, want)
		}
	}
}
