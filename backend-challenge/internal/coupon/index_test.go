package coupon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildIndexAndLookup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src1 := filepath.Join(dir, "couponbase1.txt")
	src2 := filepath.Join(dir, "couponbase2.txt")
	src3 := filepath.Join(dir, "couponbase3.txt")
	out := filepath.Join(dir, "coupons.idx")

	if err := os.WriteFile(src1, []byte("HAPPYHRS\nONLYONCE\nFIFTYOFF\n"), 0o600); err != nil {
		t.Fatalf("write src1: %v", err)
	}
	if err := os.WriteFile(src2, []byte("abc HAPPYHRS xyz\n"), 0o600); err != nil {
		t.Fatalf("write src2: %v", err)
	}
	if err := os.WriteFile(src3, []byte("FIFTYOFF\nanother\n"), 0o600); err != nil {
		t.Fatalf("write src3: %v", err)
	}

	result, err := BuildIndex(context.Background(), out, []string{src1, src2, src3}, BuildOptions{
		ChunkSize:      2,
		MergeBatchSize: 2,
	})
	if err != nil {
		t.Fatalf("BuildIndex() error = %v", err)
	}
	if result.Records != 2 {
		t.Fatalf("BuildIndex() records = %d, want 2", result.Records)
	}

	validator, err := OpenIndexedValidator(out)
	if err != nil {
		t.Fatalf("OpenIndexedValidator() error = %v", err)
	}
	defer validator.Close()

	tests := []struct {
		name string
		code string
		want bool
	}{
		{name: "valid1", code: "HAPPYHRS", want: true},
		{name: "valid2", code: "FIFTYOFF", want: true},
		{name: "invalid_once", code: "ONLYONCE", want: false},
		{name: "invalid_format", code: "SHORT", want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			got, err := validator.IsValid(context.Background(), tt.code)
			if err != nil {
				t.Fatalf("IsValid() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("IsValid() = %v, want %v", got, tt.want)
			}
		})
	}
}
