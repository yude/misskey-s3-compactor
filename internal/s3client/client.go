// Package s3client builds an S3 client from environment configuration.
package s3client

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/yude/misskey-s3-compactor/internal/config"
)

// New constructs an S3 client honoring custom endpoints (MinIO/R2/etc.)
// and the path-style addressing toggle.
func New(ctx context.Context, c config.Config) (*s3.Client, error) {
	cfgOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(c.Region),
	}
	if c.AccessKey != "" || c.SecretKey != "" {
		provider := credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, c.SessionToken)
		cfgOpts = append(cfgOpts, awsconfig.WithCredentialsProvider(provider))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		return nil, fmt.Errorf("loading aws config: %w", err)
	}

	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if c.AccessKey != "" || c.SecretKey != "" {
			o.Credentials = credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, c.SessionToken)
		}
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
		if c.UsePathStyle {
			o.UsePathStyle = true
		}
	}), nil
}
