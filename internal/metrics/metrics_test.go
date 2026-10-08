package metrics

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestInstrumentLabelsByRoute(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("the wrapped writer isn't a Flusher")
			return
		}
		_, _ = io.WriteString(w, "data: x\n\n")
		fl.Flush()
	})
	srv := httptest.NewServer(Instrument(mux))
	t.Cleanup(srv.Close)

	route := "GET /items/{id}"
	before := testutil.ToFloat64(httpRequests.WithLabelValues(route, "GET", "418"))
	for _, p := range []string{"/items/1", "/items/2", "/stream", "/nowhere/" + strings.Repeat("x", 10)} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if got := testutil.ToFloat64(httpRequests.WithLabelValues(route, "GET", "418")) - before; got != 2 {
		t.Fatalf("requests for %q: %v, want 2 (one label for both IDs)", route, got)
	}
	if got := testutil.ToFloat64(httpRequests.WithLabelValues("GET /stream", "GET", "200")); got < 1 {
		t.Fatalf("streamed request not counted as 200: %v", got)
	}
	if got := testutil.ToFloat64(httpRequests.WithLabelValues("unmatched", "GET", "404")); got < 1 {
		t.Fatalf("unmatched request not counted: %v", got)
	}
}

func TestServe(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := Serve(ctx, addr, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"engram_build_info", "go_goroutines", "process_resident_memory_bytes"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	if err := Serve(ctx, "", nil); err != nil {
		t.Fatalf("empty address: %v", err)
	}
}
