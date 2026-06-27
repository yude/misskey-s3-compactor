// Package walker paginates through S3 object keys under a prefix.
package walker

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Object is a minimal S3 object record relevant to the compactor.
type Object struct {
	Key          string
	Size         int64
	ContentType  string
	StorageClass string
}

// Lister is the subset of s3.Client that Walker needs, extracted for tests.
type Lister interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// Walker iterates S3 list pages, yielding flat Object values.
type Walker struct {
	client Lister
	bucket string
	prefix string
}

// New builds a Walker bound to (bucket, prefix).
func New(client Lister, bucket, prefix string) *Walker {
	return &Walker{client: client, bucket: bucket, prefix: prefix}
}

// Walk lists every object key under the configured prefix. Each key is passed
// to fn along with the context; if fn returns an error walking stops
// immediately. The context is checked between objects and passed to fn so
// long-running callbacks (downloads, compressions) can observe cancellation.
func (w *Walker) Walk(ctx context.Context, fn func(context.Context, Object) error) error {
	var token *string
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		out, err := w.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(w.bucket),
			Prefix:            aws.String(w.prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("listobjects: %w", err)
		}

		for _, o := range out.Contents {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if err := fn(ctx, Object{
				Key:          aws.ToString(o.Key),
				Size:         aws.ToInt64(o.Size),
				ContentType:  "",
				StorageClass: string(o.StorageClass),
			}); err != nil {
				return err
			}
		}

		if !aws.ToBool(out.IsTruncated) {
			return nil
		}
		token = out.NextContinuationToken
	}
}
