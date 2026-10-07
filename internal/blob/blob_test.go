package blob

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// newFakeS3 starts an in-process S3. With ignoreConditions, it strips
// If-None-Match before handling, like providers that silently overwrite.
func newFakeS3(t *testing.T, ignoreConditions bool) *S3 {
	t.Helper()
	be := s3mem.New()
	if err := be.CreateBucket("engram"); err != nil {
		t.Fatal(err)
	}
	h := gofakes3.New(be).Server()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreConditions {
			r.Header.Del("If-None-Match")
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	s, err := NewS3(S3Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "engram", Prefix: "test/", AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func stores(t *testing.T) map[string]Store {
	return map[string]Store{
		"dir": Dir{Root: t.TempDir()},
		"s3":  newFakeS3(t, false),
	}
}

func TestStoreContract(t *testing.T) {
	t.Parallel()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if _, err := GetBytes(ctx, s, "spaces/a/missing"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing: %v", err)
			}
			if err := PutBytes(ctx, s, "spaces/a/seg-1", []byte("0123456789"), false); err != nil {
				t.Fatal(err)
			}
			if err := PutBytes(ctx, s, "spaces/b/seg-1", []byte("x"), false); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetRange(ctx, "spaces/a/seg-1", 3, 4)
			if err != nil || string(got) != "3456" {
				t.Fatalf("range: %q %v", got, err)
			}
			if n, err := s.Size(ctx, "spaces/a/seg-1"); err != nil || n != 10 {
				t.Fatalf("size %d %v", n, err)
			}
			if err := PutBytes(ctx, s, "spaces/a/seg-1", []byte("again"), true); !errors.Is(err, ErrExists) {
				t.Fatalf("conditional overwrite: %v", err)
			}
			if err := PutBytes(ctx, s, "spaces/a/manifest-1", []byte("{}"), true); err != nil {
				t.Fatal(err)
			}
			objs, err := s.List(ctx, "spaces/a/")
			if err != nil || len(objs) != 2 || objs[0].Key != "spaces/a/manifest-1" || objs[1].Size != 10 {
				t.Fatalf("list: %+v %v", objs, err)
			}
			if err := s.Delete(ctx, "spaces/a/seg-1"); err != nil {
				t.Fatal(err)
			}
			if err := s.Delete(ctx, "spaces/a/seg-1"); err != nil {
				t.Fatalf("deleting a missing key: %v", err)
			}
			if _, err := s.Size(ctx, "spaces/a/seg-1"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("after delete: %v", err)
			}
		})
	}
}

func TestProbe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, s := range stores(t) {
		ok, err := Probe(ctx, s)
		if err != nil || !ok {
			t.Fatalf("%s: probe = %v, %v; want conditional writes", name, ok, err)
		}
		if objs, _ := s.List(ctx, "probe/"); len(objs) != 0 {
			t.Fatalf("%s: probe left %v behind", name, objs)
		}
	}
	// A provider that ignores the condition and overwrites must not be
	// trusted.
	ok, err := Probe(ctx, newFakeS3(t, true))
	if err != nil || ok {
		t.Fatalf("ignoring provider: probe = %v, %v", ok, err)
	}
}

// TestRealBucketConformance runs the probe against a real bucket, such as
// Wasabi, when ENGRAM_TEST_S3_ENDPOINT and its credentials are set. It
// reports whether the provider honors conditional writes.
func TestRealBucketConformance(t *testing.T) {
	t.Parallel()
	endpoint := os.Getenv("ENGRAM_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("ENGRAM_TEST_S3_ENDPOINT unset")
	}
	s, err := NewS3(S3Config{
		Endpoint: endpoint, Region: os.Getenv("ENGRAM_TEST_S3_REGION"), Bucket: os.Getenv("ENGRAM_TEST_S3_BUCKET"),
		Prefix: "conformance/", AccessKey: os.Getenv("ENGRAM_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("ENGRAM_TEST_S3_SECRET_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := Probe(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s honors conditional writes: %v", endpoint, ok)
}

func TestDirRejectsEscapingKeys(t *testing.T) {
	t.Parallel()
	d := Dir{Root: t.TempDir()}
	for _, k := range []string{"../x", "/etc/passwd", "a/../../b", ""} {
		if err := PutBytes(context.Background(), d, k, []byte("x"), false); err == nil || !strings.Contains(err.Error(), "bad object key") {
			t.Fatalf("%q: %v", k, err)
		}
	}
}
