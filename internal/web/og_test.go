package web

import (
	"image/png"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// metaContent returns the content of the <meta> tag whose property or name is
// `name` in the page's HTML.
func metaContent(t *testing.T, html, name string) string {
	t.Helper()
	re := regexp.MustCompile(`<meta\s+(?:property|name)="` + regexp.QuoteMeta(name) + `"\s+content="([^"]*)"`)
	m := re.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("web/index.html has no %q meta tag", name)
	}
	return m[1]
}

// TestLinkPreview checks what a link preview is built from: the tags in the
// page's HTML, and the image they name, which has to exist and be the size the
// tags say.
func TestLinkPreview(t *testing.T) {
	t.Parallel()
	page, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)

	if img := metaContent(t, html, "og:image"); !regexp.MustCompile(`^https://[^/]+/og\.png$`).MatchString(img) {
		t.Fatalf("og:image should be an absolute https URL ending in /og.png, got %q", img)
	}
	f, err := os.Open("../../web/public/og.png")
	if err != nil {
		t.Fatalf("the image og:image names must be in web/public: %v", err)
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatalf("web/public/og.png is not a PNG: %v", err)
	}
	if cfg.Width != 1200 || cfg.Height != 630 {
		t.Fatalf("og.png is %dx%d, want 1200x630", cfg.Width, cfg.Height)
	}
	if got := metaContent(t, html, "og:image:width"); got != strconv.Itoa(cfg.Width) {
		t.Fatalf("og:image:width is %s, the image is %d wide", got, cfg.Width)
	}
	if got := metaContent(t, html, "og:image:height"); got != strconv.Itoa(cfg.Height) {
		t.Fatalf("og:image:height is %s, the image is %d high", got, cfg.Height)
	}
	if got := metaContent(t, html, "twitter:card"); got != "summary_large_image" {
		t.Fatalf("twitter:card is %q, want summary_large_image", got)
	}
	if alt := metaContent(t, html, "og:image:alt"); len(alt) < 20 {
		t.Fatalf("og:image:alt is too short to describe the image: %q", alt)
	}
	for _, name := range []string{"og:title", "og:description"} {
		if metaContent(t, html, name) == "" {
			t.Fatalf("%s is empty", name)
		}
	}
}
