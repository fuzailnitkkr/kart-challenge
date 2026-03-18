package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/oolio-group/kart-challenge/backend-challenge/internal/coupon"
)

func main() {
	var (
		outPath   = flag.String("out", "data/coupons.idx", "output index file path")
		tempDir   = flag.String("temp-dir", "", "temp directory for sort chunks (optional)")
		chunkSize = flag.Int("chunk-size", 300000, "number of tokens per in-memory sort chunk")
		batchSize = flag.Int("batch-size", 128, "max files merged at once")
	)
	flag.Parse()

	sourcePaths := flag.Args()
	if len(sourcePaths) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <coupon-file-1> <coupon-file-2> [coupon-file-3...]\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	start := time.Now()
	result, err := coupon.BuildIndex(ctx, *outPath, sourcePaths, coupon.BuildOptions{
		TempDir:        *tempDir,
		ChunkSize:      *chunkSize,
		MergeBatchSize: *batchSize,
		Logf:           log.Printf,
	})
	if err != nil {
		log.Fatalf("build index failed: %v", err)
	}

	log.Printf("index build complete: %d records written to %s (elapsed: %s)", result.Records, *outPath, time.Since(start).Round(time.Second))
}
