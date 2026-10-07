package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 stores objects in an S3-compatible bucket.
type S3 struct {
	Client *minio.Client
	Bucket string
	// Prefix is prepended to every key, so several appviews can share a
	// bucket.
	Prefix string
}

// S3Config configures an S3 store.
type S3Config struct {
	// Endpoint is the service URL, such as https://s3.us-east-1.wasabisys.com.
	Endpoint  string
	Region    string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
	// Transport overrides the HTTP transport (tests).
	Transport http.RoundTripper
}

// NewS3 connects to a bucket.
func NewS3(cfg S3Config) (*S3, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad S3 endpoint %q", cfg.Endpoint)
	}
	c, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       u.Scheme != "http",
		Region:       cfg.Region,
		BucketLookup: minio.BucketLookupPath,
		Transport:    cfg.Transport,
	})
	if err != nil {
		return nil, err
	}
	return &S3{Client: c, Bucket: cfg.Bucket, Prefix: cfg.Prefix}, nil
}

func (s *S3) key(k string) string { return s.Prefix + k }

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	r := minio.ToErrorResponse(err)
	switch {
	case r.Code == "NoSuchKey" || r.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case r.StatusCode == http.StatusPreconditionFailed || r.StatusCode == http.StatusConflict:
		return fmt.Errorf("%w: %v", ErrExists, err)
	}
	return err
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	_, err := s.Client.PutObject(ctx, s.Bucket, s.key(key), r, size, minio.PutObjectOptions{})
	return mapErr(err)
}

func (s *S3) PutIfAbsent(ctx context.Context, key string, r io.Reader, size int64) error {
	opts := minio.PutObjectOptions{}
	opts.SetMatchETagExcept("*")
	// Single-part uploads only: conditions apply to PutObject.
	opts.DisableMultipart = true
	_, err := s.Client.PutObject(ctx, s.Bucket, s.key(key), r, size, opts)
	return mapErr(err)
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	o, err := s.Client.GetObject(ctx, s.Bucket, s.key(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, mapErr(err)
	}
	// GetObject is lazy; stat to surface a missing key now.
	if _, err := o.Stat(); err != nil {
		o.Close()
		return nil, mapErr(err)
	}
	return o, nil
}

func (s *S3) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	if n == 0 {
		return []byte{}, nil
	}
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(off, off+n-1); err != nil {
		return nil, err
	}
	o, err := s.Client.GetObject(ctx, s.Bucket, s.key(key), opts)
	if err != nil {
		return nil, mapErr(err)
	}
	defer o.Close()
	b := make([]byte, n)
	if _, err := io.ReadFull(o, b); err != nil {
		return nil, mapErr(err)
	}
	return b, nil
}

func (s *S3) Size(ctx context.Context, key string) (int64, error) {
	info, err := s.Client.StatObject(ctx, s.Bucket, s.key(key), minio.StatObjectOptions{})
	if err != nil {
		return 0, mapErr(err)
	}
	return info.Size, nil
}

func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	for o := range s.Client.ListObjects(ctx, s.Bucket, minio.ListObjectsOptions{Prefix: s.key(prefix), Recursive: true}) {
		if o.Err != nil {
			return nil, mapErr(o.Err)
		}
		out = append(out, Object{Key: o.Key[len(s.Prefix):], Size: o.Size, Modified: o.LastModified})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	err := s.Client.RemoveObject(ctx, s.Bucket, s.key(key), minio.RemoveObjectOptions{})
	if err := mapErr(err); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}
