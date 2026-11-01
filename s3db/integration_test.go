package s3db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestRealBucket runs against a real S3 server, named by STEKI_TEST_S3 as
// "endpoint region accesskey secret bucket" (path-style, such as a local MinIO).
func TestRealBucket(t *testing.T) {
	v := os.Getenv("STEKI_TEST_S3")
	if v == "" {
		t.Skip("STEKI_TEST_S3 not set")
	}
	var ep, region, key, secret, name string
	if _, err := fmt.Sscanf(v, "%s %s %s %s %s", &ep, &region, &key, &secret, &name); err != nil {
		t.Fatal(err)
	}
	b, err := NewBucket(ep, region, key, secret, name, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := Verify(ctx, b); err != nil {
		t.Fatal(err)
	}

	shard := fmt.Sprintf("it-%d", time.Now().UnixNano())
	open := func(owner string) *Shard {
		s, err := Open(ctx, Options{Name: shard, Dir: t.TempDir(), Objects: b, Owner: owner, Create: true})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a := open("a")
	if _, err := a.DB().Exec(`create table kv (k integer primary key)`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := range maxSegments + 5 { // crosses a compaction
		if _, err := a.DB().Exec(`insert into kv values (?1)`, i); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%v per acknowledged write", time.Since(start)/(maxSegments+5))

	c := open("c") // takes over while a is still running
	defer c.Close()
	if _, err := a.DB().Exec(`insert into kv values (100000)`); !errors.Is(err, ErrFenced) {
		t.Fatalf("old writer got %v, want ErrFenced", err)
	}
	crash(a)
	var n int
	if err := c.DB().QueryRow(`select count(*) from kv`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != maxSegments+5 {
		t.Fatalf("%d rows, want %d", n, maxSegments+5)
	}
}
