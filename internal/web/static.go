package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The frontend (web/) builds into dist/app. Only dist/README.md is checked
// in, so a Go build without the frontend serves a placeholder page.
//
//go:embed all:dist
var dist embed.FS

// Frontend is the built frontend, or nil when it wasn't built.
func Frontend() fs.FS {
	sub, err := fs.Sub(dist, "dist/app")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}

const placeholder = `<!doctype html><meta charset="utf-8"><title>Engram Garden</title>
<p>The web app wasn't built into this binary. Run <code>make web</code>, then rebuild.</p>`

// static serves the frontend's files, and index.html for any other path so
// the frontend's routes work on reload.
func (s *Server) static() http.Handler {
	if s.Static == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(placeholder))
		})
	}
	files := http.FileServerFS(s.Static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if st, err := fs.Stat(s.Static, name); err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					// Vite names assets by content hash.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, s.Static, "index.html")
	})
}
