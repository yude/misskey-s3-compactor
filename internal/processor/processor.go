// Package processor downloads a single S3 object, recompresses it, and uploads
// the result back to the same key when the savings exceed the configured ratio.
package processor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/yude/misskey-s3-compactor/internal/compress"
	"github.com/yude/misskey-s3-compactor/internal/config"
	"github.com/yude/misskey-s3-compactor/internal/walker"
)

// Processor holds the S3 client and compress engine bound to a single bucket.
type Processor struct {
	cfg    config.Config
	s3     *s3.Client
	bucket string
	comp   *compress.Compressor
	log    *slog.Logger

	stats Stats
}

// Stats tracks aggregate counters across one run.
type Stats struct {
	Scanned       int64
	SkippedMarked int64
	SkippedType   int64
	SkippedSize   int64
	Compressed    int64
	Replaced      int64
	Unchanged     int64
	Errors        int64
	InputBytes    int64
	OutputBytes   int64
	SavedBytes    int64
}

// New builds a Processor.
func New(cfg config.Config, s3c *s3.Client, comp *compress.Compressor, log *slog.Logger) *Processor {
	return &Processor{cfg: cfg, s3: s3c, bucket: cfg.Bucket, comp: comp, log: log}
}

// Stats returns aggregate counters. Safe to read after Process returned.
func (p *Processor) Stats() Stats { return p.stats }

// Handle is the per-object callback used by walker.Walk.
func (p *Processor) Handle(ctx context.Context, o walker.Object) error {
	p.stats.Scanned++

	if p.cfg.MaxObjectBytes > 0 && o.Size > p.cfg.MaxObjectBytes {
		p.stats.SkippedSize++
		p.log.Debug("skipped: too large", "key", o.Key, "size", o.Size)
		return nil
	}

	head, err := p.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(o.Key),
	})
	if err != nil {
		p.stats.Errors++
		p.log.Warn("headobject failed", "key", o.Key, "err", err)
		return nil
	}
	contentType := aws.ToString(head.ContentType)
	metadata := head.Metadata

	if p.cfg.SkipMarked && metadata != nil {
		if v, ok := lookupMeta(metadata, p.cfg.MarkerKey); ok && v == "done" {
			p.stats.SkippedMarked++
			p.log.Debug("skipped: already compressed", "key", o.Key)
			return nil
		}
	}
	if contentType == "" {
		contentType = guessContentType(o.Key)
	}

	if err := p.processOne(ctx, o.Key, contentType, metadata); err != nil {
		p.stats.Errors++
		p.log.Warn("process failed", "key", o.Key, "err", err)
	}
	return nil
}

func (p *Processor) processOne(ctx context.Context, key, contentType string, existing map[string]string) error {
	dlFile, err := os.CreateTemp("", "compactor-dl-*")
	if err != nil {
		return err
	}
	dlPath := dlFile.Name()
	_ = dlFile.Close()
	defer os.Remove(dlPath)

	if err := p.download(ctx, key, dlPath); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	realSize, err := fileSize(dlPath)
	if err != nil {
		return err
	}
	if realSize == 0 {
		return errors.New("downloaded file empty")
	}

	outFile, err := os.CreateTemp("", "compactor-out-*")
	if err != nil {
		return err
	}
	outPath := outFile.Name()
	_ = outFile.Close()
	defer os.Remove(outPath)

	res := p.comp.Compress(ctx, dlPath, outPath, contentType, key)
	p.stats.Compressed++
	p.stats.InputBytes += res.Original

	if res.Err != nil {
		if res.Skipped {
			if res.SkipReason == "already hevc" || res.SkipReason == "unsupported media type" {
				p.stats.SkippedType++
			}
			p.log.Debug("skipped compress", "key", key, "reason", res.SkipReason)
			return nil
		}
		return res.Err
	}
	if res.Output <= 0 {
		p.stats.SkippedType++
		return nil
	}

	p.stats.OutputBytes += res.Output
	saving := 0.0
	if res.Original > 0 {
		saving = float64(res.Original-res.Output) / float64(res.Original)
	}
	if saving < p.cfg.MinSavingRatio {
		p.stats.Unchanged++
		p.log.Debug("not worth replacing", "key", key, "saving", saving)
		return nil
	}

	saved := res.Original - res.Output
	if p.cfg.DryRun {
		p.stats.SavedBytes += saved
		p.log.Info("dry-run: would replace", "key", key,
			"in", res.Original, "out", res.Output, "ratio", saving)
		return nil
	}

	if err := p.upload(ctx, key, outPath, contentType, existing); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	p.stats.Replaced++
	p.stats.SavedBytes += saved
	p.log.Info("replaced", "key", key, "compressor", res.Compressor,
		"in", res.Original, "out", res.Output, "ratio", saving,
		"duration_ms", res.Duration.Milliseconds())
	return nil
}

func (p *Processor) download(ctx context.Context, key, destPath string) error {
	g, err := p.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	defer g.Body.Close()
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, g.Body)
	return err
}

func (p *Processor) upload(ctx context.Context, key, srcPath, contentType string, existing map[string]string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}

	meta := make(map[string]string, len(existing)+1)
	for k, v := range existing {
		if equalFoldASCII(k, p.cfg.MarkerKey) {
			continue
		}
		meta[k] = v
	}
	meta[p.cfg.MarkerKey] = "done"

	in := &s3.PutObjectInput{
		Bucket:        aws.String(p.bucket),
		Key:           aws.String(key),
		Body:          f,
		ContentLength: aws.Int64(st.Size()),
		ContentType:   aws.String(contentType),
		Metadata:      meta,
	}
	if _, err := p.s3.PutObject(ctx, in); err != nil {
		return err
	}
	return nil
}

func guessContentType(key string) string {
	switch lower(filepath.Ext(key)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".mkv":
		return "video/x-matroska"
	}
	return ""
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 32
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func lookupMeta(m map[string]string, key string) (string, bool) {
	for k, v := range m {
		if equalFoldASCII(k, key) {
			return v, true
		}
	}
	return "", false
}

func fileSize(p string) (int64, error) {
	st, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
