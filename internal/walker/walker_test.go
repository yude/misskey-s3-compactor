package walker

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeLister records calls and returns canned output for one page.
type fakeLister struct {
	called    int
	contents  []types.Object
	truncated bool
}

func (f *fakeLister) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.called++
	return &s3.ListObjectsV2Output{
		Contents:    f.contents,
		IsTruncated: aws.Bool(f.truncated),
	}, nil
}

func TestWalk_SinglePage(t *testing.T) {
	l := &fakeLister{contents: []types.Object{
		{Key: aws.String("a"), Size: aws.Int64(10)},
		{Key: aws.String("b"), Size: aws.Int64(20)},
	}, truncated: false}
	w := New(l, "b", "p")
	var got []string
	if err := w.Walk(context.Background(), func(_ context.Context, o Object) error {
		got = append(got, o.Key)
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if l.called != 1 {
		t.Fatalf("expected 1 list call, got %d", l.called)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("unexpected keys %v", got)
	}
}

func TestWalk_StopOnCallback(t *testing.T) {
	l := &fakeLister{contents: []types.Object{
		{Key: aws.String("a"), Size: aws.Int64(1)},
	}, truncated: false}
	w := New(l, "b", "p")
	sentinel := errors.New("stop")
	got := 0
	err := w.Walk(context.Background(), func(_ context.Context, o Object) error {
		got++
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("expected sentinel, got %v", err)
	}
	if got != 1 {
		t.Fatalf("expected callback once, got %d", got)
	}
}

func TestWalk_MultiPage(t *testing.T) {
	l := &statefulLister{
		pages: [][]types.Object{
			{{Key: aws.String("a")}, {Key: aws.String("b")}},
			{{Key: aws.String("c")}},
		},
	}
	w := New(l, "b", "p")
	var got []string
	if err := w.Walk(context.Background(), func(_ context.Context, o Object) error {
		if o.Key != "" {
			got = append(got, o.Key)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if l.calls != 2 {
		t.Fatalf("expected 2 list calls, got %d", l.calls)
	}
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("unexpected keys %v", got)
	}
}

func TestWalk_CancelBetweenObjects(t *testing.T) {
	l := &statefulLister{
		pages: [][]types.Object{
			{{Key: aws.String("a")}, {Key: aws.String("b")}},
			{{Key: aws.String("c")}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := New(l, "b", "p")
	var got []string
	err := w.Walk(ctx, func(_ context.Context, o Object) error {
		got = append(got, o.Key)
		if len(got) == 1 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 object before cancel, got %d", len(got))
	}
}

type statefulLister struct {
	pages [][]types.Object
	calls int
}

func (s *statefulLister) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	idx := s.calls
	s.calls++
	if idx >= len(s.pages) {
		return nil, errors.New("too many pages")
	}
	truncated := idx < len(s.pages)-1
	var next *string
	if truncated {
		next = aws.String("token")
	}
	return &s3.ListObjectsV2Output{
		Contents:              s.pages[idx],
		IsTruncated:           aws.Bool(truncated),
		NextContinuationToken: next,
	}, nil
}
