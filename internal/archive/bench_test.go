package archive

import (
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkExtract extracts $ZSTDIR_BENCH_ARCHIVE (skipped if unset).
func BenchmarkExtract(b *testing.B) {
	src := os.Getenv("ZSTDIR_BENCH_ARCHIVE")
	if src == "" {
		b.Skip("ZSTDIR_BENCH_ARCHIVE not set")
	}
	for b.Loop() {
		f, err := os.Open(src)
		if err != nil {
			b.Fatal(err)
		}
		dst := filepath.Join(b.TempDir(), "x")
		if _, _, err := Extract(b.Context(), f, dst, Options{}); err != nil {
			b.Fatal(err)
		}
		f.Close()
	}
}

// BenchmarkCompress compresses $ZSTDIR_BENCH_DIR (skipped if unset).
func BenchmarkCompress(b *testing.B) {
	src := os.Getenv("ZSTDIR_BENCH_DIR")
	if src == "" {
		b.Skip("ZSTDIR_BENCH_DIR not set")
	}
	for b.Loop() {
		f, err := os.Create(filepath.Join(b.TempDir(), "a.tar.zst"))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := Compress(b.Context(), src, f, Options{}); err != nil {
			b.Fatal(err)
		}
		f.Close()
	}
}
