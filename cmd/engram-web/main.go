// Command engram-web serves the Engram Garden web app: people sign in with
// their ATProto account to browse the memory spaces they belong to, delete
// their own memories, and manage the spaces they govern.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/util/ssrf"

	"github.com/haileyok/engram-garden/internal/config"
	"github.com/haileyok/engram-garden/internal/oauthfile"
	"github.com/haileyok/engram-garden/internal/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("engram-web failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	publicURL, err := config.Require("ENGRAM_WEB_PUBLIC_URL")
	if err != nil {
		return err
	}
	publicURL = strings.TrimSuffix(publicURL, "/")
	pu, err := url.Parse(publicURL)
	if err != nil || pu.Host == "" {
		return fmt.Errorf("ENGRAM_WEB_PUBLIC_URL: not a URL: %q", publicURL)
	}
	dev := pu.Scheme == "http" && pu.Hostname() == "127.0.0.1"

	appviewURL := strings.TrimSuffix(config.Get("ENGRAM_APPVIEW_URL", "https://api.engram.garden"), "/")
	au, err := url.Parse(appviewURL)
	if err != nil || au.Host == "" {
		return fmt.Errorf("ENGRAM_APPVIEW_URL: not a URL: %q", appviewURL)
	}
	appviewDID := config.Get("ENGRAM_APPVIEW_DID", "did:web:"+au.Hostname())

	dataDir := config.Get("ENGRAM_WEB_DATA", "engram-web-data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	cookieKey, err := cookieKey(dataDir)
	if err != nil {
		return err
	}

	var key atcrypto.PrivateKey
	if raw := config.Get("ENGRAM_WEB_CLIENT_KEY", ""); raw != "" {
		if key, err = atcrypto.ParsePrivateMultibase(raw); err != nil {
			return fmt.Errorf("ENGRAM_WEB_CLIENT_KEY: %w", err)
		}
	}

	// Requests to PDSes and space authorities go to hosts named in DID
	// documents, so keep them off private networks, except when developing
	// against a local PDS.
	allowPrivate := dev
	switch config.Get("ENGRAM_WEB_ALLOW_PRIVATE", "") {
	case "1", "true":
		allowPrivate = true
	case "0", "false":
		allowPrivate = false
	}
	outbound := &http.Client{Timeout: 60 * time.Second}
	if !allowPrivate {
		outbound.Transport = ssrf.PublicOnlyTransport()
	}

	dir := config.Directory()
	store := &oauthfile.FileStore{Dir: filepath.Join(dataDir, "oauth")}
	o, err := web.NewOAuth(web.OAuthConfig{PublicURL: publicURL, Key: key, Store: store, Dir: dir, HTTP: outbound})
	if err != nil {
		return err
	}
	srv := &web.Server{
		Auth:       o,
		OAuth:      o,
		Dir:        dir,
		HTTP:       outbound,
		AppviewURL: appviewURL,
		AppviewDID: appviewDID,
		Origin:     o.PublicURL,
		CookieKey:  cookieKey,
		Static:     web.Frontend(),
		Log:        log,
	}
	if srv.Static == nil {
		log.Warn("the frontend isn't built into this binary; run make web and rebuild")
	}

	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			store.Sweep()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	addr := config.Get("ENGRAM_WEB_LISTEN", ":8090")
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	log.Info("engram-web listening", "addr", addr, "public", o.PublicURL, "appview", appviewURL, "appviewDid", appviewDID,
		"client", o.App.Config.ClientID, "confidential", o.App.Config.IsConfidential())
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// cookieKey reads ENGRAM_WEB_COOKIE_KEY (hex, at least 32 bytes), or a key
// kept in the data directory, creating it on first run.
func cookieKey(dataDir string) ([]byte, error) {
	if raw := config.Get("ENGRAM_WEB_COOKIE_KEY", ""); raw != "" {
		k, err := hex.DecodeString(raw)
		if err != nil || len(k) < 32 {
			return nil, errors.New("ENGRAM_WEB_COOKIE_KEY must be at least 32 bytes of hex")
		}
		return k, nil
	}
	path := filepath.Join(dataDir, "cookie.key")
	if raw, err := os.ReadFile(path); err == nil {
		k, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(k) < 32 {
			return nil, fmt.Errorf("%s is corrupt; delete it to sign everyone out and start over", path)
		}
		return k, nil
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(k)), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}
