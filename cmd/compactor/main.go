// Command compactor scans a bucket under a prefix, recompresses images and
// videos with the best available encoder for each format, and overwrites the
// object in place when savings exceed a configurable threshold. Designed to run
// as a Kubernetes CronJob.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yude/misskey-s3-compactor/internal/compress"
	"github.com/yude/misskey-s3-compactor/internal/config"
	"github.com/yude/misskey-s3-compactor/internal/processor"
	"github.com/yude/misskey-s3-compactor/internal/s3client"
	"github.com/yude/misskey-s3-compactor/internal/tools"
	"github.com/yude/misskey-s3-compactor/internal/walker"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel()}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config invalid", "err", err)
		os.Exit(2)
	}

	if err := tools.Check(); err != nil {
		log.Error("runtime missing binaries", "err", err)
		os.Exit(3)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	s3c, err := s3client.New(ctx, cfg)
	if err != nil {
		log.Error("s3 client init failed", "err", err)
		os.Exit(4)
	}

	comp := compress.New(cfg, log)
	proc := processor.New(cfg, s3c, comp, log)

	log.Info("starting scan",
		"bucket", cfg.Bucket, "prefix", cfg.Prefix,
		"endpoint", cfg.Endpoint, "path_style", cfg.UsePathStyle,
		"dry_run", cfg.DryRun)

	start := time.Now()
	err = walker.New(s3c, cfg.Bucket, cfg.Prefix).Walk(ctx, proc.Handle)
	elapsed := time.Since(start)

	stats := proc.Stats()
	log.Info("scan complete",
		"scanned", stats.Scanned,
		"skipped_marked", stats.SkippedMarked,
		"skipped_type", stats.SkippedType,
		"skipped_size", stats.SkippedSize,
		"compressed_attempts", stats.Compressed,
		"replaced", stats.Replaced,
		"unchanged", stats.Unchanged,
		"errors", stats.Errors,
		"input_bytes", stats.InputBytes,
		"output_bytes", stats.OutputBytes,
		"saved_bytes", stats.SavedBytes,
		"elapsed_ms", elapsed.Milliseconds(),
	)

	if errors.Is(err, context.Canceled) {
		log.Warn("interrupted by signal")
		os.Exit(130)
	}
	if err != nil {
		log.Error("scan stopped with error", "err", err)
		os.Exit(1)
	}
	if stats.Errors > 0 {
		os.Exit(1)
	}
}

func parseLevel() slog.Level {
	switch v := os.Getenv("LOG_LEVEL"); v {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
