package coupon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReloadingIndexedValidator_PicksUpIndexChanges(t *testing.T) {
	dir := t.TempDir()
	src1 := filepath.Join(dir, "couponbase1.txt")
	src2 := filepath.Join(dir, "couponbase2.txt")
	src3 := filepath.Join(dir, "couponbase3.txt")
	indexPath := filepath.Join(dir, "coupons.idx")

	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	write(src1, "HAPPYHRS\n")
	write(src2, "HAPPYHRS\n")
	write(src3, "RANDOM123\n")

	if _, err := BuildIndex(context.Background(), indexPath, []string{src1, src2, src3}, BuildOptions{ChunkSize: 2, MergeBatchSize: 2}); err != nil {
		t.Fatalf("initial BuildIndex() error = %v", err)
	}

	validator, err := NewReloadingIndexedValidator(indexPath, ReloaderOptions{Interval: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewReloadingIndexedValidator() error = %v", err)
	}
	defer validator.Close()

	valid, err := validator.IsValid(context.Background(), "NEWCODE99")
	if err != nil {
		t.Fatalf("IsValid() error = %v", err)
	}
	if valid {
		t.Fatalf("NEWCODE99 should not be valid before index update")
	}

	write(src1, "HAPPYHRS\nNEWCODE99\n")
	write(src2, "HAPPYHRS\nNEWCODE99\n")
	write(src3, "RANDOM123\n")
	if _, err := BuildIndex(context.Background(), indexPath, []string{src1, src2, src3}, BuildOptions{ChunkSize: 2, MergeBatchSize: 2}); err != nil {
		t.Fatalf("updated BuildIndex() error = %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		valid, err = validator.IsValid(context.Background(), "NEWCODE99")
		if err != nil {
			t.Fatalf("IsValid() error = %v", err)
		}
		if valid {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("validator did not pick up updated index in time")
		}
		time.Sleep(40 * time.Millisecond)
	}
}
