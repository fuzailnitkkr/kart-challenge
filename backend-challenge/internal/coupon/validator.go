package coupon

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	minCouponLength = 8
	maxCouponLength = 10
)

// Validator validates coupon codes.
type Validator interface {
	IsValid(ctx context.Context, code string) (bool, error)
}

// FileValidator validates coupons by checking that a code exists
// in at least two source files.
type FileValidator struct {
	sources []source

	mu    sync.RWMutex
	cache map[string]bool
}

type source struct {
	path string
}

// NewFileValidator creates a coupon validator backed by local files.
func NewFileValidator(paths []string) *FileValidator {
	sources := make([]source, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		sources = append(sources, source{path: p})
	}

	return &FileValidator{
		sources: sources,
		cache:   make(map[string]bool),
	}
}

// NormalizeCode converts a coupon to canonical comparison format.
func NormalizeCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// IsValid returns true if code matches format and appears in >= 2 files.
func (v *FileValidator) IsValid(ctx context.Context, code string) (bool, error) {
	code = NormalizeCode(code)
	if !hasValidFormat(code) {
		return false, nil
	}

	v.mu.RLock()
	cached, ok := v.cache[code]
	v.mu.RUnlock()
	if ok {
		return cached, nil
	}

	count := 0
	for _, src := range v.sources {
		found, err := src.contains(ctx, code)
		if err != nil {
			return false, err
		}
		if found {
			count++
			if count >= 2 {
				break
			}
		}
	}

	valid := count >= 2
	v.mu.Lock()
	v.cache[code] = valid
	v.mu.Unlock()

	return valid, nil
}

func hasValidFormat(code string) bool {
	if len(code) < minCouponLength || len(code) > maxCouponLength {
		return false
	}

	for i := 0; i < len(code); i++ {
		c := code[i]
		if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}

	return true
}

func (s source) contains(ctx context.Context, code string) (bool, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return false, fmt.Errorf("open coupon source %q: %w", s.path, err)
	}
	defer f.Close()

	var reader io.Reader = f
	var gzipReader *gzip.Reader
	if strings.EqualFold(filepath.Ext(s.path), ".gz") {
		gzipReader, err = gzip.NewReader(f)
		if err != nil {
			return false, fmt.Errorf("open gzip coupon source %q: %w", s.path, err)
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	found := false
	err = visitNormalizedCodes(ctx, reader, func(token string) (bool, error) {
		if token == code {
			found = true
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return false, fmt.Errorf("scan coupon source %q: %w", s.path, err)
	}

	return found, nil
}

func isAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}
