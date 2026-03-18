package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/oolio-group/kart-challenge/backend-challenge/internal/app"
)

func main() {
	cfg := app.ConfigFromEnv()

	runtime, err := app.BuildRuntime(cfg)
	if err != nil {
		log.Fatalf("failed to build app: %v", err)
	}
	defer func() {
		if err := runtime.Close(); err != nil {
			log.Printf("failed to close runtime: %v", err)
		}
	}()

	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           runtime.Handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("api server listening on %s", cfg.Address)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server failed: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}
