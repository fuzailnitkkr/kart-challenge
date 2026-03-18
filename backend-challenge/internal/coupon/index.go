package coupon

import (
	"bufio"
	"compress/gzip"
	"container/heap"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	indexMagic      = "CPNIDX1\n"
	indexHeaderSize = int64(len(indexMagic) + 8)
	indexRecordSize = int64(11) // 1 byte length + 10 bytes ascii code
)

// BuildOptions configures index generation for large coupon files.
type BuildOptions struct {
	TempDir        string
	ChunkSize      int
	MergeBatchSize int
	Logf           func(format string, args ...any)
}

// BuildResult contains summary information for a completed index build.
type BuildResult struct {
	Records int64
}

// BuildIndex builds a binary on-disk index that supports fast coupon lookups.
func BuildIndex(ctx context.Context, outputPath string, sourcePaths []string, opts BuildOptions) (BuildResult, error) {
	if len(sourcePaths) < 2 {
		return BuildResult{}, errors.New("at least two source files are required")
	}
	if strings.TrimSpace(outputPath) == "" {
		return BuildResult{}, errors.New("output path is required")
	}

	opts = normalizedBuildOptions(opts)

	tempDir := opts.TempDir
	cleanupTemp := false
	if strings.TrimSpace(tempDir) == "" {
		var err error
		tempDir, err = os.MkdirTemp("", "coupon-index-*")
		if err != nil {
			return BuildResult{}, fmt.Errorf("create temp dir: %w", err)
		}
		cleanupTemp = true
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return BuildResult{}, fmt.Errorf("create temp dir: %w", err)
	}
	if cleanupTemp {
		defer os.RemoveAll(tempDir)
	}

	uniquePaths := make([]string, 0, len(sourcePaths))
	for i, sourcePath := range sourcePaths {
		logf(opts, "building sorted set for source %d/%d: %s", i+1, len(sourcePaths), sourcePath)

		uniquePath, err := buildUniqueSourceFile(ctx, sourcePath, tempDir, i, opts)
		if err != nil {
			return BuildResult{}, err
		}
		uniquePaths = append(uniquePaths, uniquePath)
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return BuildResult{}, fmt.Errorf("create output dir: %w", err)
	}

	tmpOut := outputPath + ".tmp"
	count, err := writeValidIndex(ctx, uniquePaths, tmpOut)
	if err != nil {
		_ = os.Remove(tmpOut)
		return BuildResult{}, err
	}

	if err := os.Rename(tmpOut, outputPath); err != nil {
		_ = os.Remove(tmpOut)
		return BuildResult{}, fmt.Errorf("move index into place: %w", err)
	}

	logf(opts, "index complete: %d valid coupons -> %s", count, outputPath)
	return BuildResult{Records: count}, nil
}

// IndexedValidator validates coupon codes against a prebuilt binary index.
type IndexedValidator struct {
	file  *os.File
	count int64
}

// OpenIndexedValidator loads a prebuilt coupon index from disk.
func OpenIndexedValidator(path string) (*IndexedValidator, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open coupon index: %w", err)
	}

	header := make([]byte, indexHeaderSize)
	if _, err := io.ReadFull(f, header); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("read coupon index header: %w", err)
	}

	if string(header[:len(indexMagic)]) != indexMagic {
		_ = f.Close()
		return nil, errors.New("invalid coupon index magic")
	}

	count := int64(binary.BigEndian.Uint64(header[len(indexMagic):]))

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat coupon index: %w", err)
	}

	expectedSize := indexHeaderSize + (count * indexRecordSize)
	if info.Size() != expectedSize {
		_ = f.Close()
		return nil, fmt.Errorf("coupon index size mismatch: expected %d bytes, got %d bytes", expectedSize, info.Size())
	}

	return &IndexedValidator{
		file:  f,
		count: count,
	}, nil
}

// Close closes the underlying index file.
func (v *IndexedValidator) Close() error {
	if v == nil || v.file == nil {
		return nil
	}
	return v.file.Close()
}

// IsValid performs O(log N) lookup in the coupon index.
func (v *IndexedValidator) IsValid(ctx context.Context, code string) (bool, error) {
	code = NormalizeCode(code)
	if !hasValidFormat(code) {
		return false, nil
	}

	lo := int64(0)
	hi := v.count - 1
	var rec [indexRecordSize]byte

	for lo <= hi {
		if err := ctx.Err(); err != nil {
			return false, err
		}

		mid := lo + ((hi - lo) / 2)
		offset := indexHeaderSize + (mid * indexRecordSize)
		if _, err := v.file.ReadAt(rec[:], offset); err != nil {
			return false, fmt.Errorf("read coupon index: %w", err)
		}

		target := decodeIndexRecord(rec)
		switch strings.Compare(code, target) {
		case 0:
			return true, nil
		case -1:
			hi = mid - 1
		default:
			lo = mid + 1
		}
	}

	return false, nil
}

func normalizedBuildOptions(opts BuildOptions) BuildOptions {
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = 300_000
	}
	if opts.MergeBatchSize < 2 {
		opts.MergeBatchSize = 128
	}
	return opts
}

func logf(opts BuildOptions, format string, args ...any) {
	if opts.Logf != nil {
		opts.Logf(format, args...)
	}
}

func buildUniqueSourceFile(ctx context.Context, sourcePath, tempDir string, sourceIndex int, opts BuildOptions) (string, error) {
	f, err := os.Open(sourcePath)
	if err != nil {
		return "", fmt.Errorf("open source file %q: %w", sourcePath, err)
	}
	defer f.Close()

	reader, closer, err := wrapMaybeGzip(f, sourcePath)
	if err != nil {
		return "", err
	}
	if closer != nil {
		defer closer.Close()
	}

	chunkFiles, err := writeSortedChunks(ctx, reader, tempDir, sourceIndex, opts)
	if err != nil {
		return "", err
	}

	if len(chunkFiles) == 0 {
		emptyPath := filepath.Join(tempDir, fmt.Sprintf("source-%d-empty.txt", sourceIndex))
		if err := os.WriteFile(emptyPath, nil, 0o644); err != nil {
			return "", fmt.Errorf("write empty source file: %w", err)
		}
		return emptyPath, nil
	}

	logf(opts, "source %d generated %d sorted chunks", sourceIndex+1, len(chunkFiles))
	return mergeChunkFiles(tempDir, chunkFiles, opts.MergeBatchSize)
}

func wrapMaybeGzip(f *os.File, sourcePath string) (io.Reader, io.Closer, error) {
	if !strings.EqualFold(filepath.Ext(sourcePath), ".gz") {
		return f, nil, nil
	}

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, fmt.Errorf("open gzip source %q: %w", sourcePath, err)
	}
	return gz, gz, nil
}

func writeSortedChunks(ctx context.Context, reader io.Reader, tempDir string, sourceIndex int, opts BuildOptions) ([]string, error) {
	chunkFiles := make([]string, 0, 16)
	buffer := make([]string, 0, opts.ChunkSize)
	chunkNumber := 0

	flush := func() error {
		if len(buffer) == 0 {
			return nil
		}
		chunkPath := filepath.Join(tempDir, fmt.Sprintf("source-%d-chunk-%05d.txt", sourceIndex, chunkNumber))
		chunkNumber++
		if err := writeSortedUniqueLines(chunkPath, buffer); err != nil {
			return err
		}
		chunkFiles = append(chunkFiles, chunkPath)
		buffer = make([]string, 0, opts.ChunkSize)
		return nil
	}

	err := visitNormalizedCodes(ctx, reader, func(code string) (bool, error) {
		buffer = append(buffer, code)
		if len(buffer) >= opts.ChunkSize {
			return false, flush()
		}
		return false, nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan source file: %w", err)
	}

	if err := flush(); err != nil {
		return nil, err
	}

	return chunkFiles, nil
}

func writeSortedUniqueLines(path string, lines []string) error {
	sort.Strings(lines)

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create chunk file %q: %w", path, err)
	}
	defer file.Close()

	writer := bufio.NewWriterSize(file, 1<<20)
	last := ""
	for i, line := range lines {
		if i == 0 || line != last {
			if _, err := writer.WriteString(line); err != nil {
				return fmt.Errorf("write chunk file: %w", err)
			}
			if err := writer.WriteByte('\n'); err != nil {
				return fmt.Errorf("write chunk file: %w", err)
			}
			last = line
		}
	}

	if err := writer.Flush(); err != nil {
		return fmt.Errorf("flush chunk file: %w", err)
	}
	return nil
}

func mergeChunkFiles(tempDir string, chunkFiles []string, batchSize int) (string, error) {
	current := append([]string(nil), chunkFiles...)
	round := 0

	for len(current) > 1 {
		next := make([]string, 0, (len(current)+batchSize-1)/batchSize)
		toDelete := make([]string, 0, len(current))

		for i := 0; i < len(current); i += batchSize {
			j := i + batchSize
			if j > len(current) {
				j = len(current)
			}
			group := current[i:j]
			if len(group) == 1 {
				next = append(next, group[0])
				continue
			}

			outPath := filepath.Join(tempDir, fmt.Sprintf("merge-%d-%05d.txt", round, i/batchSize))
			if err := mergeSortedLineFiles(group, outPath); err != nil {
				return "", err
			}
			next = append(next, outPath)
			toDelete = append(toDelete, group...)
		}

		for _, p := range toDelete {
			_ = os.Remove(p)
		}

		current = next
		round++
	}

	return current[0], nil
}

func mergeSortedLineFiles(paths []string, outPath string) error {
	readers := make([]*lineReader, 0, len(paths))
	for _, path := range paths {
		r, err := newLineReader(path)
		if err != nil {
			for _, open := range readers {
				open.Close()
			}
			return err
		}
		readers = append(readers, r)
	}
	defer func() {
		for _, r := range readers {
			r.Close()
		}
	}()

	outFile, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create merged file %q: %w", outPath, err)
	}
	defer outFile.Close()

	out := bufio.NewWriterSize(outFile, 1<<20)
	defer out.Flush()

	h := make(minHeap, 0, len(readers))
	for i, r := range readers {
		ok, err := r.Next()
		if err != nil {
			return err
		}
		if ok {
			h = append(h, heapItem{value: r.Value(), index: i})
		}
	}
	heap.Init(&h)

	last := ""
	wroteAny := false

	for len(h) > 0 {
		item := heap.Pop(&h).(heapItem)
		if !wroteAny || item.value != last {
			if _, err := out.WriteString(item.value); err != nil {
				return fmt.Errorf("write merged file: %w", err)
			}
			if err := out.WriteByte('\n'); err != nil {
				return fmt.Errorf("write merged file: %w", err)
			}
			last = item.value
			wroteAny = true
		}

		ok, err := readers[item.index].Next()
		if err != nil {
			return err
		}
		if ok {
			heap.Push(&h, heapItem{value: readers[item.index].Value(), index: item.index})
		}
	}

	return out.Flush()
}

func writeValidIndex(ctx context.Context, uniquePaths []string, outPath string) (int64, error) {
	readers := make([]*lineReader, 0, len(uniquePaths))
	for _, path := range uniquePaths {
		r, err := newLineReader(path)
		if err != nil {
			for _, open := range readers {
				open.Close()
			}
			return 0, err
		}
		readers = append(readers, r)
	}
	defer func() {
		for _, r := range readers {
			r.Close()
		}
	}()

	active := make([]bool, len(readers))
	current := make([]string, len(readers))

	for i, r := range readers {
		ok, err := r.Next()
		if err != nil {
			return 0, err
		}
		if ok {
			active[i] = true
			current[i] = r.Value()
		}
	}

	outFile, err := os.Create(outPath)
	if err != nil {
		return 0, fmt.Errorf("create index file %q: %w", outPath, err)
	}
	defer outFile.Close()

	if _, err := outFile.Write([]byte(indexMagic)); err != nil {
		return 0, fmt.Errorf("write index header: %w", err)
	}
	if _, err := outFile.Write(make([]byte, 8)); err != nil {
		return 0, fmt.Errorf("write index header: %w", err)
	}

	recordCount := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}

		minCode, hasMin := minActive(current, active)
		if !hasMin {
			break
		}

		matches := 0
		for i := range readers {
			if !active[i] || current[i] != minCode {
				continue
			}
			matches++
			ok, err := readers[i].Next()
			if err != nil {
				return 0, err
			}
			if ok {
				current[i] = readers[i].Value()
			} else {
				active[i] = false
			}
		}

		if matches >= 2 {
			rec := encodeIndexRecord(minCode)
			if _, err := outFile.Write(rec[:]); err != nil {
				return 0, fmt.Errorf("write index record: %w", err)
			}
			recordCount++
		}
	}

	countBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(countBuf, uint64(recordCount))
	if _, err := outFile.WriteAt(countBuf, int64(len(indexMagic))); err != nil {
		return 0, fmt.Errorf("write index count: %w", err)
	}

	return recordCount, nil
}

func minActive(values []string, active []bool) (string, bool) {
	minSet := false
	minValue := ""
	for i := range values {
		if !active[i] {
			continue
		}
		if !minSet || values[i] < minValue {
			minValue = values[i]
			minSet = true
		}
	}
	return minValue, minSet
}

func encodeIndexRecord(code string) [indexRecordSize]byte {
	var out [indexRecordSize]byte
	out[0] = byte(len(code))
	copy(out[1:], []byte(code))
	return out
}

func decodeIndexRecord(rec [indexRecordSize]byte) string {
	n := int(rec[0])
	if n < 0 {
		n = 0
	}
	if n > int(indexRecordSize-1) {
		n = int(indexRecordSize - 1)
	}
	return string(rec[1 : 1+n])
}

type lineReader struct {
	file    *os.File
	scanner *bufio.Scanner
	value   string
}

func newLineReader(path string) (*lineReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open sorted file %q: %w", path, err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4*1024), 1024*1024)
	return &lineReader{file: f, scanner: sc}, nil
}

func (r *lineReader) Next() (bool, error) {
	if !r.scanner.Scan() {
		if err := r.scanner.Err(); err != nil {
			return false, fmt.Errorf("scan sorted file: %w", err)
		}
		return false, nil
	}
	r.value = r.scanner.Text()
	return true, nil
}

func (r *lineReader) Value() string {
	return r.value
}

func (r *lineReader) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	return r.file.Close()
}

type heapItem struct {
	value string
	index int
}

type minHeap []heapItem

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i].value < h[j].value }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *minHeap) Push(x any) {
	*h = append(*h, x.(heapItem))
}

func (h *minHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}
