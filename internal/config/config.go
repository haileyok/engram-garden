// Package config reads settings shared by the Engram Garden commands from
// the environment.
package config

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/embed"
)

// Get returns an environment variable or a default.
func Get(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// Require returns an environment variable or an error naming it.
func Require(key string) (string, error) {
	v := Get(key, "")
	if v == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return v, nil
}

// Directory returns a caching identity directory.
func Directory() identity.Directory {
	return identity.DefaultDirectory()
}

// Login opens a password session for ENGRAM_IDENTIFIER / ENGRAM_PASSWORD.
// ENGRAM_PDS_HOST skips resolving the account's PDS.
func Login(ctx context.Context, dir identity.Directory) (*atclient.APIClient, error) {
	ident, err := Require("ENGRAM_IDENTIFIER")
	if err != nil {
		return nil, err
	}
	pass, err := Require("ENGRAM_PASSWORD")
	if err != nil {
		return nil, err
	}
	if host := Get("ENGRAM_PDS_HOST", ""); host != "" {
		return atclient.LoginWithPasswordHost(ctx, host, ident, pass, "", nil)
	}
	atid, err := syntax.ParseAtIdentifier(ident)
	if err != nil {
		return nil, fmt.Errorf("ENGRAM_IDENTIFIER: %w", err)
	}
	return atclient.LoginWithPassword(ctx, dir, atid, pass, "", nil)
}

// Embedder builds the embedder from ENGRAM_EMBED_* settings.
//
//	ENGRAM_EMBED_PROVIDER  openai (default) or hashing (offline, no meaning; dev only)
//	ENGRAM_EMBED_URL       OpenAI-compatible base URL (default https://api.openai.com/v1)
//	ENGRAM_EMBED_API_KEY   API key (falls back to OPENAI_API_KEY)
//	ENGRAM_EMBED_MODEL     model (default text-embedding-3-small)
//	ENGRAM_EMBED_DIMS      vector size (default 1536)
//	ENGRAM_EMBED_SEND_DIMS send the size to the model (default true for text-embedding-3 models)
func Embedder() (embed.Embedder, error) {
	dims, err := strconv.Atoi(Get("ENGRAM_EMBED_DIMS", "1536"))
	if err != nil || dims <= 0 {
		return nil, fmt.Errorf("ENGRAM_EMBED_DIMS must be a positive integer")
	}
	switch p := Get("ENGRAM_EMBED_PROVIDER", "openai"); p {
	case "hashing":
		return embed.Hashing{Dims: dims}, nil
	case "openai":
		model := Get("ENGRAM_EMBED_MODEL", "text-embedding-3-small")
		send, err := strconv.ParseBool(Get("ENGRAM_EMBED_SEND_DIMS", strconv.FormatBool(strings.HasPrefix(model, "text-embedding-3"))))
		if err != nil {
			return nil, fmt.Errorf("ENGRAM_EMBED_SEND_DIMS must be true or false")
		}
		return &embed.OpenAI{
			BaseURL:        Get("ENGRAM_EMBED_URL", "https://api.openai.com/v1"),
			APIKey:         Get("ENGRAM_EMBED_API_KEY", os.Getenv("OPENAI_API_KEY")),
			Name:           model,
			Dims:           dims,
			SendDimensions: send,
			HTTP:           &http.Client{Timeout: 60 * time.Second},
		}, nil
	default:
		return nil, fmt.Errorf("unknown ENGRAM_EMBED_PROVIDER %q", p)
	}
}

// List splits a comma-separated variable.
func List(key string) []string {
	var out []string
	for _, s := range strings.Split(Get(key, ""), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
