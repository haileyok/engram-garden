package blob

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/minio/minio-go/v7"
)

// newFakeS3 starts an in-process S3. With ignoreConditions, it strips
// If-None-Match before handling, like providers that silently overwrite.
//
// It serves TLS, as real providers do: over plain HTTP the client signs
// uploads in chunks, which gofakes3 doesn't decode for multipart parts.
func newFakeS3(t *testing.T, ignoreConditions bool) *S3 {
	t.Helper()
	be := s3mem.New()
	if err := be.CreateBucket("engram"); err != nil {
		t.Fatal(err)
	}
	h := gofakes3.New(be).Server()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreConditions {
			r.Header.Del("If-None-Match")
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	s, err := NewS3(S3Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "engram", Prefix: "test/", AccessKey: "k", SecretKey: "s",
		Transport: srv.Client().Transport,
	})
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

// runStoreContract exercises every Store operation under spaces/a/ and
// spaces/b/. conditional says whether the store honors conditional writes;
// when it doesn't (a real provider that ignores If-None-Match), the checks
// that need a rejected overwrite are skipped.
func runStoreContract(t *testing.T, s Store, conditional bool) {
	t.Helper()
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
	if conditional {
		if err := PutBytes(ctx, s, "spaces/a/seg-1", []byte("again"), true); !errors.Is(err, ErrExists) {
			t.Fatalf("conditional overwrite: %v", err)
		}
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
}

func TestStoreContract(t *testing.T) {
	t.Parallel()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runStoreContract(t, s, true)
		})
	}
}

// runLargeObject round-trips an object big enough that the S3 client
// uploads it in parts, then reads ranges across the part boundary. Segment
// files for big spaces are this large.
func runLargeObject(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	const size = 17<<20 + 123 // just over one 16 MiB part
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 ^ i>>11)
	}
	if err := PutBytes(ctx, s, "spaces/big/seg-1", data, false); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Size(ctx, "spaces/big/seg-1"); err != nil || n != size {
		t.Fatalf("size %d %v", n, err)
	}
	for _, r := range []struct{ off, n int64 }{{16<<20 - 100, 300}, {0, 64}, {size - 50, 50}} {
		got, err := s.GetRange(ctx, "spaces/big/seg-1", r.off, r.n)
		if err != nil || !bytes.Equal(got, data[r.off:r.off+r.n]) {
			t.Fatalf("range %d+%d: %v", r.off, r.n, err)
		}
	}
	all, err := GetBytes(ctx, s, "spaces/big/seg-1")
	if err != nil || !bytes.Equal(all, data) {
		t.Fatalf("whole object: len %d, %v", len(all), err)
	}
}

func TestLargeObject(t *testing.T) {
	t.Parallel()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runLargeObject(t, s)
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

// realBucket opens the bucket named by ENGRAM_TEST_S3_* (a real provider,
// such as Wasabi) under a prefix unique to this call, and deletes everything
// under that prefix when the test ends. It skips when
// ENGRAM_TEST_S3_ENDPOINT is unset.
//
// Providers like Wasabi bill each object for a minimum storage period even
// after it's deleted, so use a dedicated test bucket.
func realBucket(t *testing.T) *S3 {
	t.Helper()
	endpoint := os.Getenv("ENGRAM_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("ENGRAM_TEST_S3_ENDPOINT unset")
	}
	cfg := S3Config{
		Endpoint: endpoint, Region: os.Getenv("ENGRAM_TEST_S3_REGION"), Bucket: os.Getenv("ENGRAM_TEST_S3_BUCKET"),
		Prefix:    "conformance/" + time.Now().UTC().Format("20060102T150405") + "-" + randomHex(4) + "/",
		AccessKey: os.Getenv("ENGRAM_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("ENGRAM_TEST_S3_SECRET_KEY"),
	}
	if cfg.Region == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		t.Fatal("ENGRAM_TEST_S3_ENDPOINT is set, so ENGRAM_TEST_S3_REGION, _BUCKET, _ACCESS_KEY and _SECRET_KEY must be too")
	}
	s, err := NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		objs, err := s.List(ctx, "")
		if err != nil {
			t.Errorf("cleanup: listing %s: %v", cfg.Prefix, err)
			return
		}
		for _, o := range objs {
			if err := s.Delete(ctx, o.Key); err != nil {
				t.Errorf("cleanup: deleting %s: %v", o.Key, err)
			}
		}
	})
	return s
}

// TestRealBucketConformance runs the store contract, a multipart upload and
// the conditional-write probe against a real bucket, such as Wasabi, when
// ENGRAM_TEST_S3_* is set. The contract and large object must pass whether
// or not the provider honors conditional writes; whether it does is
// reported in the log.
func TestRealBucketConformance(t *testing.T) {
	t.Parallel()
	s := realBucket(t)
	ctx := context.Background()
	conditional, err := Probe(ctx, s)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("%s honors conditional writes: %v", os.Getenv("ENGRAM_TEST_S3_ENDPOINT"), conditional)

	t.Run("contract", func(t *testing.T) { runStoreContract(t, s, conditional) })
	t.Run("large object", func(t *testing.T) { runLargeObject(t, s) })

	// Log exactly what the provider answers to a conditional overwrite, so
	// the docs can say what it does rather than what we assumed.
	t.Run("conditional overwrite response", func(t *testing.T) {
		if err := PutBytes(ctx, s, "diag/conditional", []byte("first"), false); err != nil {
			t.Fatal(err)
		}
		opts := minio.PutObjectOptions{DisableMultipart: true}
		opts.SetMatchETagExcept("*")
		_, err := s.Client.PutObject(ctx, s.Bucket, s.key("diag/conditional"), strings.NewReader("second"), 6, opts)
		if err == nil {
			t.Log("the provider accepted a PutObject with If-None-Match: * on an existing key")
		} else {
			r := minio.ToErrorResponse(err)
			t.Logf("PutObject with If-None-Match: * on an existing key: HTTP %d, code %q, message %q", r.StatusCode, r.Code, r.Message)
		}
		got, err := GetBytes(ctx, s, "diag/conditional")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("object after the conditional overwrite: %q", got)
	})
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
