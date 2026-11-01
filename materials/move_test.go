package materials

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// TestMoveIntoFolder runs against a real S3 server, named by STEKI_TEST_S3 as
// "endpoint region accesskey secret bucket" (path-style, such as a local MinIO).
func TestMoveIntoFolder(t *testing.T) {
	v := os.Getenv("STEKI_TEST_S3")
	if v == "" {
		t.Skip("STEKI_TEST_S3 not set")
	}
	var ep, region, key, secret, name string
	if _, err := fmt.Sscanf(v, "%s %s %s %s %s", &ep, &region, &key, &secret, &name); err != nil {
		t.Fatal(err)
	}
	b, err := NewBucket(ep, region, key, secret, name)
	if err != nil {
		t.Fatal(err)
	}
	b.client, err = minio.New(strings.TrimPrefix(ep, "http://"), &minio.Options{
		Creds: credentials.NewStaticV4(key, secret, ""), Region: region, BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	put := func(k, body string) {
		t.Helper()
		if err := b.put(ctx, k, strings.NewReader(body), int64(len(body)), "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	read := func(k string) string {
		t.Helper()
		var sb strings.Builder
		found, err := b.Fetch(ctx, k, &sb)
		if err != nil || !found {
			t.Fatalf("%s: found=%v err=%v", k, found, err)
		}
		return sb.String()
	}
	run := fmt.Sprint(time.Now().UnixNano())
	course, loose, nested := "course"+run+"/1-a.pdf", "loose"+run+".pdf", "course"+run+"/thread/2-b.pdf"
	dbChannel := "db/" + run + "-c.pdf" // a course channel called db
	notes := "user-notes/" + run + ".sqlite"
	shard := "db/registry/manifest" + run
	for _, k := range []string{course, loose, nested, dbChannel, notes, shard} {
		put(k, "body of "+k)
	}
	// An earlier move died between copying this one and deleting the original.
	put(MaterialPrefix+course, "body of "+course)

	keep := []string{"user-notes/", "db/registry/", "db/.verify-"}
	n, err := b.MoveIntoFolder(ctx, keep, log)
	if err != nil {
		t.Fatal(err)
	}
	if n < 4 {
		t.Errorf("moved %d, want at least 4", n)
	}
	for _, k := range []string{course, loose, nested, dbChannel} {
		if got := read(MaterialPrefix + k); got != "body of "+k {
			t.Errorf("%s holds %q", k, got)
		}
		if found, _ := b.Fetch(ctx, k, io.Discard); found {
			t.Errorf("the original %s is still there", k)
		}
		r, _, err := b.Open(ctx, k)
		if err != nil {
			t.Errorf("Open(%s) through the prefix: %v", k, err)
		} else {
			r.Close()
		}
	}
	for _, k := range []string{notes, shard} {
		if got := read(k); got != "body of "+k {
			t.Errorf("%s was disturbed: %q", k, got)
		}
		if found, _ := b.Fetch(ctx, MaterialPrefix+k, io.Discard); found {
			t.Errorf("%s was moved", k)
		}
	}
	if n, err := b.MoveIntoFolder(ctx, keep, log); err != nil || n != 0 {
		t.Errorf("a second move did %d, %v", n, err)
	}
}
