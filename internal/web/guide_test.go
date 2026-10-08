package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestServesAgentsGuide checks that web/public/AGENTS.md, which the frontend
// build copies to the site's root, comes out of the server at /AGENTS.md as
// text: not as the app's index page, which is what a path without a file gets,
// and not too long to be worth an agent's context.
func TestServesAgentsGuide(t *testing.T) {
	t.Parallel()
	s := &Server{Static: os.DirFS("../../web/public")}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /AGENTS.md: %d, want 200 (is web/public/AGENTS.md there?)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/") || strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type is %q, want plain text", ct)
	}
	if !strings.HasPrefix(body, "# ") {
		t.Fatalf("the guide should start with a heading, got %.40q", body)
	}
	if len(body) > 8000 {
		t.Fatalf("the guide is %d bytes; keep it under 8000 so agents read all of it", len(body))
	}
}
