package materials

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/minio/minio-go/v7"
)

// moveWorkers is how many objects are copied at once.
const moveWorkers = 8

// MoveIntoFolder moves every object outside MaterialPrefix and outside keep (key
// prefixes) under MaterialPrefix, and returns how many it moved. Each object is
// copied and checked before the original goes, so an interrupted move loses
// nothing and the next call finishes it. With nothing to move, it costs one listing
// of the bucket's top level and one of each kept folder's parent.
func (b *Bucket) MoveIntoFolder(ctx context.Context, keep []string, log *slog.Logger) (int64, error) {
	stays := func(key string) bool {
		if strings.HasPrefix(key, MaterialPrefix) {
			return true
		}
		for _, k := range keep {
			if strings.HasPrefix(key, k) {
				return true
			}
		}
		return false
	}

	jobs := make(chan string)
	var (
		moved    atomic.Int64
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for range moveWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for key := range jobs {
				if err := b.moveObject(ctx, key); err != nil {
					errOnce.Do(func() { firstErr = fmt.Errorf("move %s: %w", key, err); cancel() })
					continue
				}
				if n := moved.Add(1); n%500 == 0 {
					log.Info("materials: moving into the folder", "moved", n)
				}
			}
		}()
	}

	listErr := b.eachKey(ctx, stays, func(key string) {
		select {
		case jobs <- key:
		case <-ctx.Done():
		}
	})
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return moved.Load(), firstErr
	}
	return moved.Load(), listErr
}

// eachKey calls fn for each key that does not stay, without walking the folders
// that wholly do: the material folder holds nearly everything, and this runs on
// every start.
func (b *Bucket) eachKey(ctx context.Context, stays func(string) bool, fn func(string)) error {
	for top := range b.client.ListObjects(ctx, b.name, minio.ListObjectsOptions{}) {
		if top.Err != nil {
			return top.Err
		}
		if !strings.HasSuffix(top.Key, "/") {
			if !stays(top.Key) {
				fn(top.Key)
			}
			continue
		}
		if stays(top.Key) {
			continue
		}
		for o := range b.client.ListObjects(ctx, b.name, minio.ListObjectsOptions{Prefix: top.Key, Recursive: true}) {
			if o.Err != nil {
				return o.Err
			}
			if !stays(o.Key) {
				fn(o.Key)
			}
		}
	}
	return nil
}

func (b *Bucket) moveObject(ctx context.Context, key string) error {
	src, err := b.client.StatObject(ctx, b.name, key, minio.StatObjectOptions{})
	if err != nil {
		return err
	}
	dst := MaterialPrefix + key
	if _, err := b.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: b.name, Object: dst},
		minio.CopySrcOptions{Bucket: b.name, Object: key}); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	got, err := b.client.StatObject(ctx, b.name, dst, minio.StatObjectOptions{})
	if err != nil {
		return fmt.Errorf("check the copy: %w", err)
	}
	if got.Size != src.Size {
		return fmt.Errorf("the copy has %d bytes, the original %d", got.Size, src.Size)
	}
	return b.client.RemoveObject(ctx, b.name, key, minio.RemoveObjectOptions{})
}
