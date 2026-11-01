package s3db

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var (
	// ErrNotFound is returned when an object does not exist.
	ErrNotFound = errors.New("s3db: object not found")
	// ErrConflict is returned when a conditional write lost: the object is not
	// in the state the writer expected.
	ErrConflict = errors.New("s3db: conditional write refused")
)

// Objects is what a shard needs from the bucket.
type Objects interface {
	// Get writes the object at key to w and returns its etag.
	Get(ctx context.Context, key string, w io.Writer) (etag string, err error)
	// Put writes the object at key, whatever is there.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// PutIf writes the object only if its etag is still etag, or, with an empty
	// etag, only if it does not exist. It returns the new etag.
	PutIf(ctx context.Context, key string, body []byte, etag string) (newEtag string, err error)
	Delete(ctx context.Context, key string) error
	// List returns the keys under prefix.
	List(ctx context.Context, prefix string) ([]string, error)
}

// Bucket is Objects on an S3-compatible bucket.
type Bucket struct {
	client *minio.Client
	name   string
}

// NewBucket connects to a bucket at endpoint (a URL such as
// https://t3.storageapi.dev). pathStyle is for servers without virtual-hosted
// buckets, such as a local MinIO; Railway's use virtual-hosted URLs.
func NewBucket(endpoint, region, accessKey, secretKey, name string, pathStyle bool) (*Bucket, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("s3db: bad bucket endpoint %q", endpoint)
	}
	lookup := minio.BucketLookupDNS
	if pathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:       u.Scheme != "http",
		Region:       region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("s3db: bucket client: %w", err)
	}
	return &Bucket{client: client, name: name}, nil
}

func (b *Bucket) Get(ctx context.Context, key string, w io.Writer) (string, error) {
	obj, err := b.client.GetObject(ctx, b.name, key, minio.GetObjectOptions{})
	if err != nil {
		return "", classify(err)
	}
	defer obj.Close()
	st, err := obj.Stat()
	if err != nil {
		return "", classify(err)
	}
	if _, err := io.Copy(w, obj); err != nil {
		return "", classify(err)
	}
	return st.ETag, nil
}

func (b *Bucket) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	_, err := b.client.PutObject(ctx, b.name, key, r, size, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
		PartSize:    5 << 20,
		NumThreads:  1,
	})
	return classify(err)
}

func (b *Bucket) PutIf(ctx context.Context, key string, body []byte, etag string) (string, error) {
	opts := minio.PutObjectOptions{
		ContentType:      "application/json",
		DisableMultipart: true,
		SendContentMd5:   true,
	}
	if etag == "" {
		opts.SetMatchETagExcept("*")
	} else {
		opts.SetMatchETag(etag)
	}
	info, err := b.client.PutObject(ctx, b.name, key, bytes.NewReader(body), int64(len(body)), opts)
	if err != nil {
		return "", classify(err)
	}
	return info.ETag, nil
}

func (b *Bucket) Delete(ctx context.Context, key string) error {
	return classify(b.client.RemoveObject(ctx, b.name, key, minio.RemoveObjectOptions{}))
}

func (b *Bucket) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	for o := range b.client.ListObjects(ctx, b.name, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, classify(o.Err)
		}
		keys = append(keys, o.Key)
	}
	return keys, nil
}

// classify maps the server's refusals onto the package's errors.
func classify(err error) error {
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	switch {
	case resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case resp.StatusCode == http.StatusPreconditionFailed || resp.StatusCode == http.StatusConflict ||
		resp.Code == "PreconditionFailed":
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

// Verify proves that the bucket refuses a conditional write it should refuse,
// using a throwaway object. Fencing rests entirely on this, and an S3 server
// that ignores the headers would let two writers overwrite each other without
// a sound, so a service refuses to start on a bucket that fails it.
func Verify(ctx context.Context, o Objects) (err error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	key := "db/.verify-" + hex.EncodeToString(id[:])
	defer func() {
		if derr := o.Delete(context.WithoutCancel(ctx), key); derr != nil && err == nil {
			err = fmt.Errorf("s3db: remove the probe object: %w", derr)
		}
	}()

	first, err := o.PutIf(ctx, key, []byte("1"), "")
	if err != nil {
		return fmt.Errorf("s3db: create with If-None-Match: %w", err)
	}
	if first == "" {
		return errors.New("s3db: the bucket returned no etag for a write")
	}
	if _, err := o.PutIf(ctx, key, []byte("2"), ""); !errors.Is(err, ErrConflict) {
		return fmt.Errorf("s3db: the bucket did not refuse a create over an existing object (got %v)", err)
	}
	second, err := o.PutIf(ctx, key, []byte("3"), first)
	if err != nil {
		return fmt.Errorf("s3db: write with a current If-Match: %w", err)
	}
	if second == first {
		return errors.New("s3db: the object's etag did not change when it was written")
	}
	if _, err := o.PutIf(ctx, key, []byte("4"), first); !errors.Is(err, ErrConflict) {
		return fmt.Errorf("s3db: the bucket did not refuse a stale If-Match (got %v)", err)
	}
	var got bytes.Buffer
	if _, err := o.Get(ctx, key, &got); err != nil {
		return fmt.Errorf("s3db: read the probe back: %w", err)
	}
	if got.String() != "3" {
		return fmt.Errorf("s3db: a refused write changed the object (read %q, want %q)", got.String(), "3")
	}
	return nil
}
