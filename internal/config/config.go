package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for the compactor.
type Config struct {
	// S3 connection
	Endpoint     string // optional, empty => AWS S3
	Region       string
	Bucket       string
	Prefix       string // object key prefix to scan (e.g. "misskey/")
	UsePathStyle bool   // required for MinIO/R2 and most S3-compatible services
	AccessKey    string
	SecretKey    string
	SessionToken string

	// Compression knobs
	JPEGQuality      int // 1-100, default 80
	PNGStripMetadata bool
	WebPQuality      int    // 1-100, default 80
	GIFFlags         string // extra flags for gifsicle
	VideoCRF         int    // x265 CRF, default 26
	VideoPreset      string // x265 preset, default "medium"
	VideoPixFmt      string // default "yuv420p"
	VideoCodec       string // default "libx265"

	// Behavior
	DryRun         bool
	SkipMarked     bool    // skip objects already tagged with the marker metadata
	MarkerKey      string  // metadata key used to mark processed objects
	MinSavingRatio float64 // only replace when output_size < input_size * (1 - MinSavingRatio)
	MaxObjectBytes int64   // skip objects larger than this; 0 = no limit
}

// Load reads configuration from environment variables, applying defaults.
func Load() (Config, error) {
	c := Config{
		Endpoint:         strings.TrimRight(os.Getenv("S3_ENDPOINT"), "/"),
		Region:           env("S3_REGION", "us-east-1"),
		Bucket:           os.Getenv("S3_BUCKET"),
		Prefix:           env("S3_PREFIX", ""),
		UsePathStyle:     envBool("S3_USE_PATH_STYLE", false),
		AccessKey:        os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey:        os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:     os.Getenv("AWS_SESSION_TOKEN"),
		JPEGQuality:      envInt("JPEG_QUALITY", 80),
		PNGStripMetadata: envBool("PNG_STRIP_METADATA", true),
		WebPQuality:      envInt("WEBP_QUALITY", 80),
		GIFFlags:         env("GIF_FLAGS", "--optimize=3"),
		VideoCRF:         envInt("VIDEO_CRF", 26),
		VideoPreset:      env("VIDEO_PRESET", "medium"),
		VideoPixFmt:      env("VIDEO_PIXFMT", "yuv420p"),
		VideoCodec:       env("VIDEO_CODEC", "libx265"),
		DryRun:           envBool("DRY_RUN", false),
		SkipMarked:       envBool("SKIP_MARKED", true),
		MarkerKey:        env("MARKER_KEY", "x-amz-meta-compactor"),
		MinSavingRatio:   envFloat("MIN_SAVING_RATIO", 0.05),
		MaxObjectBytes:   int64(envInt("MAX_OBJECT_BYTES", 0)),
	}

	if c.Bucket == "" {
		return c, errors.New("S3_BUCKET must be set")
	}
	if c.JPEGQuality < 1 || c.JPEGQuality > 100 {
		return c, errors.New("JPEG_QUALITY must be between 1 and 100")
	}
	if c.WebPQuality < 1 || c.WebPQuality > 100 {
		return c, errors.New("WEBP_QUALITY must be between 1 and 100")
	}
	if c.VideoCRF < 0 || c.VideoCRF > 51 {
		return c, errors.New("VIDEO_CRF must be between 0 and 51")
	}
	if c.MinSavingRatio < 0 || c.MinSavingRatio > 1 {
		return c, errors.New("MIN_SAVING_RATIO must be between 0 and 1")
	}
	return c, nil
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v, ok := os.LookupEnv(k); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v, ok := os.LookupEnv(k); ok {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, ok := os.LookupEnv(k); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
