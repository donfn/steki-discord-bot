// Package materials backs up the PDFs and JPEGs members post to the steki-backups
// bucket and decides which of them the material page may list.
//
// Discovery lives with the Discord handlers (services/web), which know channels and
// messages. This package takes it from a row in the materials table: the
// Worker copies the file into the bucket, then hands it to a Screener, whose
// verdict decides whether it is listed. The bucket keeps every file that was
// ever posted, whatever the verdict and even after the message is deleted.
package materials

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"unicode"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MaterialPrefix is the bucket folder course material lives in. Stored keys
// (materials.object_key) do not include it: Store, Open and Delete add it.
const MaterialPrefix = "material/"

// partSize is the multipart chunk for large uploads. The client buffers one
// chunk at a time, so this is the upload's memory cost; 5 MiB is the S3
// minimum.
const partSize = 5 << 20

// Bucket is the S3-compatible bucket the materials are copied into.
type Bucket struct {
	client *minio.Client
	name   string
	// codec compresses and decompresses the objects.
	codec *Converter
	// writes keeps a delete from being undone by a store in flight.
	writes sync.Mutex
}

// SetCodec sets the service that compresses and decompresses the objects.
func (b *Bucket) SetCodec(c *Converter) { b.codec = c }

// NewBucket connects to a bucket at endpoint (a URL such as
// https://t3.storageapi.dev). Railway buckets use virtual-hosted-style URLs.
func NewBucket(endpoint, region, accessKey, secretKey, name string) (*Bucket, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("materials: bad bucket endpoint %q", endpoint)
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:       u.Scheme != "http",
		Region:       region,
		BucketLookup: minio.BucketLookupDNS,
	})
	if err != nil {
		return nil, fmt.Errorf("materials: bucket client: %w", err)
	}
	return &Bucket{client: client, name: name}, nil
}

// put streams size bytes from r into the object at the full key as they are.
func (b *Bucket) put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := b.client.PutObject(ctx, b.name, key, r, size, minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    partSize,
		NumThreads:  1,
	})
	return err
}

// Delete removes the material at key. Removing one that is not there succeeds.
func (b *Bucket) Delete(ctx context.Context, key string) error {
	b.writes.Lock()
	defer b.writes.Unlock()
	return b.client.RemoveObject(ctx, b.name, MaterialPrefix+key, minio.RemoveObjectOptions{})
}

// Check confirms the bucket exists and the credentials can reach it.
func (b *Bucket) Check(ctx context.Context) error {
	ok, err := b.client.BucketExists(ctx, b.name)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("materials: bucket " + b.name + " does not exist")
	}
	return nil
}

// ObjectKey is where an attachment is stored: grouped into a folder per
// channel name, with a subfolder per thread, and prefixed with the attachment
// ID so two files of the same name never collide. folders run from the
// outermost channel inwards.
func ObjectKey(folders []string, attachmentID, filename string) string {
	parts := make([]string, 0, len(folders)+1)
	for _, f := range folders {
		parts = append(parts, cleanSegment(f, "channel"))
	}
	parts = append(parts, attachmentID+"-"+cleanSegment(filename, "file"))
	return strings.Join(parts, "/")
}

// cleanSegment makes s safe as one path segment: no separators, control
// characters or relative names, and not overlong. Greek and other letters are
// kept, since S3 keys are UTF-8.
func cleanSegment(s, fallback string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return '-'
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	s = strings.Trim(strings.TrimSpace(s), ".")
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120])
	}
	if s == "" {
		return fallback
	}
	return s
}
