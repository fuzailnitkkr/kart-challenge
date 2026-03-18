package coupon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const defaultReloadInterval = 30 * time.Second

// ReloaderOptions configures index hot-reload behavior.
type ReloaderOptions struct {
	Interval time.Duration
	Logf     func(format string, args ...any)
}

// ReloadingIndexedValidator validates against a binary index and
// reloads it automatically when the index file changes.
type ReloadingIndexedValidator struct {
	path     string
	interval time.Duration
	logf     func(format string, args ...any)

	mu      sync.RWMutex
	current *IndexedValidator
	modTime time.Time
	size    int64

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewReloadingIndexedValidator constructs a validator with periodic reload checks.
func NewReloadingIndexedValidator(path string, opts ReloaderOptions) (*ReloadingIndexedValidator, error) {
	if path == "" {
		return nil, errors.New("index path is required")
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultReloadInterval
	}

	v := &ReloadingIndexedValidator{
		path:     path,
		interval: opts.Interval,
		logf:     opts.Logf,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}

	if err := v.reloadFromDisk(true); err != nil {
		return nil, err
	}

	go v.watch()
	return v, nil
}

// IsValid validates a coupon using currently loaded index data.
func (v *ReloadingIndexedValidator) IsValid(ctx context.Context, code string) (bool, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if v.current == nil {
		return false, errors.New("coupon index is not loaded")
	}

	return v.current.IsValid(ctx, code)
}

// Close stops reload checks and releases file resources.
func (v *ReloadingIndexedValidator) Close() error {
	var closeErr error
	v.stopOnce.Do(func() {
		close(v.stopCh)
		<-v.doneCh

		v.mu.Lock()
		current := v.current
		v.current = nil
		v.mu.Unlock()

		if current != nil {
			closeErr = current.Close()
		}
	})

	return closeErr
}

func (v *ReloadingIndexedValidator) watch() {
	defer close(v.doneCh)

	ticker := time.NewTicker(v.interval)
	defer ticker.Stop()

	for {
		select {
		case <-v.stopCh:
			return
		case <-ticker.C:
			if err := v.reloadFromDisk(false); err != nil {
				v.log("coupon index reload failed: %v", err)
			}
		}
	}
}

func (v *ReloadingIndexedValidator) reloadFromDisk(initial bool) error {
	info, err := os.Stat(v.path)
	if err != nil {
		return fmt.Errorf("stat coupon index %q: %w", v.path, err)
	}

	modTime := info.ModTime()
	size := info.Size()

	v.mu.RLock()
	unchanged := !initial &&
		v.current != nil &&
		v.modTime.Equal(modTime) &&
		v.size == size
	v.mu.RUnlock()

	if unchanged {
		return nil
	}

	next, err := OpenIndexedValidator(v.path)
	if err != nil {
		return fmt.Errorf("open coupon index %q: %w", v.path, err)
	}

	v.mu.Lock()
	current := v.current
	v.current = next
	v.modTime = modTime
	v.size = size
	v.mu.Unlock()

	if current != nil {
		_ = current.Close()
	}

	if initial {
		v.log("loaded coupon index: %s", v.path)
	} else {
		v.log("reloaded coupon index: %s", v.path)
	}

	return nil
}

func (v *ReloadingIndexedValidator) log(format string, args ...any) {
	if v.logf != nil {
		v.logf(format, args...)
	}
}
