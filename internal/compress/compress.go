// Package compress drives external binaries (jpegoptim/oxipng/cwebp/gifsicle/ffmpeg)
// to lossily or losslessly recompress a file in place, preserving the original
// format as required by Misskey's stored metadata.
package compress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yude/misskey-s3-compactor/internal/config"
)

// Result reports one compression attempt.
type Result struct {
	Kind       string // "image/jpeg", "image/png", "video/webm", etc.
	Compressor string // binary used
	Original   int64
	Output     int64
	Duration   time.Duration
	Skipped    bool // true when we judged it unprofitable or unsupported
	SkipReason string
	Err        error
}

// Ratio returns 0 if Original is 0, otherwise the relative reduction (0.0..1.0).
func (r Result) Ratio() float64 {
	if r.Original <= 0 {
		return 0
	}
	return float64(r.Original-r.Output) / float64(r.Original)
}

// Compressor is the entry point. It accepts a source path and the media type
// (MIME), and recompresses the file into destPath. Returns metadata about the
// attempt. The source file is never modified; destPath holds recompressed data
// if and only if Compressor produced one (Skipped=false, Err=nil, Output>0).
type Compressor struct {
	cfg config.Config
	log *slog.Logger
}

// New builds a Compressor with the supplied config.
func New(cfg config.Config, log *slog.Logger) *Compressor {
	return &Compressor{cfg: cfg, log: log}
}

// Compress decides the strategy from mimeType and dispatches to the matching
// worker. On any failure or skip the returned Result has Skipped=true (when it
// was deliberate) or Err set (when incidental).
func (c *Compressor) Compress(ctx context.Context, srcPath, destPath, mimeType, key string) Result {
	switch {
	case strings.HasPrefix(mimeType, "image/jpeg"):
		return c.runJPEG(ctx, srcPath, destPath, key)
	case strings.HasPrefix(mimeType, "image/png"):
		return c.runPNG(ctx, srcPath, destPath, key)
	case strings.HasPrefix(mimeType, "image/webp"):
		return c.runWebP(ctx, srcPath, destPath, key)
	case strings.HasPrefix(mimeType, "image/gif"):
		return c.runGIF(ctx, srcPath, destPath, key)
	case strings.HasPrefix(mimeType, "video/") || isVideoExt(key):
		return c.runVideo(ctx, srcPath, destPath, key)
	default:
		return Result{Kind: mimeType, Skipped: true, SkipReason: "unsupported media type"}
	}
}

// commandContext creates an exec.Cmd that is killed with SIGINT (not SIGKILL)
// when the context is cancelled, then force-killed after a 5-second grace
// period. This lets ffmpeg and other tools clean up gracefully on Ctrl+C.
func commandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGINT)
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// runJPEG: copy file as-is to destPath then jpegoptim in-place for re-encoding.
func (c *Compressor) runJPEG(ctx context.Context, src, dest, key string) Result {
	r := Result{Kind: "image/jpeg", Compressor: "jpegoptim"}
	srcSize, err := sizeOf(src)
	if err != nil {
		r.Err = err
		return r
	}
	r.Original = srcSize
	if err := copyFile(src, dest); err != nil {
		r.Err = fmt.Errorf("copy: %w", err)
		return r
	}

	start := time.Now()
	args := []string{"-s", fmt.Sprintf("--max=%d", c.cfg.JPEGQuality), "--all-progressive", "--strip-com", dest}
	if out, err := commandContext(ctx, "jpegoptim", args...).CombinedOutput(); err != nil {
		c.log.Debug("jpegoptim failed", "key", key, "output", string(out), "err", err)
		r.Err = fmt.Errorf("jpegoptim: %w (%s)", err, strings.TrimSpace(string(out)))
		return r
	}
	r.Duration = time.Since(start)

	outSize, err := sizeOf(dest)
	if err != nil {
		r.Err = err
		return r
	}
	r.Output = outSize
	return r
}

// runPNG: lossless re-compress with oxipng.
func (c *Compressor) runPNG(ctx context.Context, src, dest, key string) Result {
	r := Result{Kind: "image/png", Compressor: "oxipng"}
	srcSize, err := sizeOf(src)
	if err != nil {
		r.Err = err
		return r
	}
	r.Original = srcSize
	if err := copyFile(src, dest); err != nil {
		r.Err = err
		return r
	}
	start := time.Now()
	stripMode := "safe"
	if c.cfg.PNGStripMetadata {
		stripMode = "all"
	}
	args := []string{"-o", "3", "--strip", stripMode, dest}
	if out, err := commandContext(ctx, "oxipng", args...).CombinedOutput(); err != nil {
		c.log.Debug("oxipng failed", "key", key, "output", string(out), "err", err)
		r.Err = fmt.Errorf("oxipng: %w (%s)", err, strings.TrimSpace(string(out)))
		return r
	}
	r.Duration = time.Since(start)
	outSize, _ := sizeOf(dest)
	r.Output = outSize
	return r
}

// runWebP: re-encode WebP at the configured quality via cwebp.
func (c *Compressor) runWebP(ctx context.Context, src, dest, key string) Result {
	r := Result{Kind: "image/webp", Compressor: "cwebp"}
	srcSize, err := sizeOf(src)
	if err != nil {
		r.Err = err
		return r
	}
	r.Original = srcSize
	start := time.Now()
	out := filepath.Join(filepath.Dir(dest), "cwebp_out.webp")
	args := []string{"-q", fmt.Sprintf("%d", c.cfg.WebPQuality), src, "-o", out}
	if o, err := commandContext(ctx, "cwebp", args...).CombinedOutput(); err != nil {
		c.log.Debug("cwebp failed", "key", key, "output", string(o), "err", err)
		r.Err = fmt.Errorf("cwebp: %w (%s)", err, strings.TrimSpace(string(o)))
		return r
	}
	r.Duration = time.Since(start)
	if err := os.Rename(out, dest); err != nil {
		r.Err = err
		return r
	}
	outSize, _ := sizeOf(dest)
	r.Output = outSize
	return r
}

// runGIF: losslessly shrink with gifsicle.
func (c *Compressor) runGIF(ctx context.Context, src, dest, key string) Result {
	r := Result{Kind: "image/gif", Compressor: "gifsicle"}
	srcSize, err := sizeOf(src)
	if err != nil {
		r.Err = err
		return r
	}
	r.Original = srcSize
	start := time.Now()
	args := []string{"--no-warnings", "--output", dest}
	args = append(args, strings.Fields(c.cfg.GIFFlags)...)
	args = append(args, src)
	if out, err := commandContext(ctx, "gifsicle", args...).CombinedOutput(); err != nil {
		c.log.Debug("gifsicle failed", "key", key, "output", string(out), "err", err)
		r.Err = fmt.Errorf("gifsicle: %w (%s)", err, strings.TrimSpace(string(out)))
		return r
	}
	r.Duration = time.Since(start)
	outSize, _ := sizeOf(dest)
	r.Output = outSize
	return r
}

// runVideo: re-encode with ffmpeg to HEVC.
func (c *Compressor) runVideo(ctx context.Context, src, dest, key string) Result {
	r := Result{Kind: "video/hevc", Compressor: "ffmpeg"}
	srcSize, err := sizeOf(src)
	if err != nil {
		r.Err = err
		return r
	}
	r.Original = srcSize

	// Probe codec: skip if the source is already HEVC/H.265-encoded.
	if codec, _ := probeVideoCodec(ctx, src); codec == "hevc" {
		r.Skipped = true
		r.SkipReason = "already hevc"
		return r
	}

	start := time.Now()
	args := []string{
		"-y",
		"-i", src,
		"-c:v", c.cfg.VideoCodec,
		"-preset", c.cfg.VideoPreset,
		"-crf", fmt.Sprintf("%d", c.cfg.VideoCRF),
		"-pix_fmt", c.cfg.VideoPixFmt,
		"-c:a", "aac",
		"-b:a", "128k",
		"-movflags", "+faststart",
		dest,
	}
	if out, err := commandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		c.log.Debug("ffmpeg failed", "key", key, "output", lastLine(out), "err", err)
		r.Err = fmt.Errorf("ffmpeg: %w (%s)", err, lastLine(out))
		return r
	}
	r.Duration = time.Since(start)
	if s, err := sizeOf(dest); err == nil {
		r.Output = s
	} else {
		r.Err = err
		return r
	}
	if r.Output == 0 {
		r.Err = errors.New("ffmpeg produced empty output")
	}
	return r
}

// --- helpers ---

func isVideoExt(key string) bool {
	switch strings.ToLower(filepath.Ext(key)) {
	case ".mp4", ".mov", ".webm", ".mkv", ".avi", ".m4v", ".wmv", ".flv", ".ts", ".mpg", ".mpeg":
		return true
	}
	return false
}

func sizeOf(p string) (int64, error) {
	st, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func probeVideoCodec(ctx context.Context, path string) (string, error) {
	out, err := commandContext(ctx, "ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name",
		"-of", "default=nk=1:nw=1",
		path,
	).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func lastLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}
