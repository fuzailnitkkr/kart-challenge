package coupon

import (
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileValidator_IsValid(t *testing.T) {
	t.Parallel()

	validator := NewFileValidator([]string{
		filepath.Join("..", "..", "testdata", "coupons", "couponbase1.txt"),
		filepath.Join("..", "..", "testdata", "coupons", "couponbase2.txt"),
		filepath.Join("..", "..", "testdata", "coupons", "couponbase3.txt"),
	})

	ctx := context.Background()

	tests := []struct {
		name string
		code string
		want bool
	}{
		{name: "valid_in_two_files", code: "HAPPYHRS", want: true},
		{name: "valid_in_two_different_files", code: "FIFTYOFF", want: true},
		{name: "invalid_found_once", code: "ONLYONCE", want: false},
		{name: "invalid_length_short", code: "SHORT", want: false},
		{name: "invalid_chars", code: "BAD-CODE", want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := validator.IsValid(ctx, tt.code)
			if err != nil {
				t.Fatalf("IsValid() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("IsValid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFileValidator_GzipSources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file1 := filepath.Join(dir, "couponbase1.gz")
	file2 := filepath.Join(dir, "couponbase2.gz")

	if err := writeGzip(file1, "SOMECODE\nHAPPYHRS\n"); err != nil {
		t.Fatalf("write gzip 1: %v", err)
	}
	if err := writeGzip(file2, "ABCDEF12\nHAPPYHRS\n"); err != nil {
		t.Fatalf("write gzip 2: %v", err)
	}

	validator := NewFileValidator([]string{file1, file2})
	ok, err := validator.IsValid(context.Background(), "HAPPYHRS")
	if err != nil {
		t.Fatalf("IsValid() error = %v", err)
	}
	if !ok {
		t.Fatalf("expected coupon to be valid")
	}
}

func writeGzip(path, content string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	if _, err := gz.Write([]byte(content)); err != nil {
		_ = gz.Close()
		return err
	}
	return gz.Close()
}
