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

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/routing"
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

// Int reads an integer variable.
func Int(key string, def int64) (int64, error) {
	s := Get(key, "")
	if s == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return n, nil
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

// Blob opens the index's object storage.
//
//	ENGRAM_STORAGE            dir (default) or s3
//	ENGRAM_STORAGE_DIR        directory for dir (default ./engram-data)
//	ENGRAM_S3_ENDPOINT        e.g. https://s3.us-east-1.wasabisys.com
//	ENGRAM_S3_REGION, ENGRAM_S3_BUCKET, ENGRAM_S3_PREFIX
//	ENGRAM_S3_ACCESS_KEY, ENGRAM_S3_SECRET_KEY
func Blob() (blob.Store, error) {
	switch kind := Get("ENGRAM_STORAGE", "dir"); kind {
	case "dir":
		return blob.Dir{Root: Get("ENGRAM_STORAGE_DIR", "engram-data")}, nil
	case "s3":
		endpoint, err := Require("ENGRAM_S3_ENDPOINT")
		if err != nil {
			return nil, err
		}
		bucket, err := Require("ENGRAM_S3_BUCKET")
		if err != nil {
			return nil, err
		}
		return blob.NewS3(blob.S3Config{
			Endpoint: endpoint, Region: Get("ENGRAM_S3_REGION", "us-east-1"), Bucket: bucket,
			Prefix: Get("ENGRAM_S3_PREFIX", ""), AccessKey: os.Getenv("ENGRAM_S3_ACCESS_KEY"),
			SecretKey: os.Getenv("ENGRAM_S3_SECRET_KEY"),
		})
	default:
		return nil, fmt.Errorf("unknown ENGRAM_STORAGE %q (dir or s3)", kind)
	}
}

// Ring reads the node list.
//
//	ENGRAM_NODES        id=url,id=url (unset: a single node)
//	ENGRAM_NODE_ID      this node's id
//	ENGRAM_NODES_EPOCH  increases on every change to ENGRAM_NODES (the
//	                    fencing token)
func Ring() (*routing.Ring, error) {
	spec := Get("ENGRAM_NODES", "")
	if spec == "" {
		return routing.Single(), nil
	}
	nodes, err := routing.ParseNodes(spec)
	if err != nil {
		return nil, fmt.Errorf("ENGRAM_NODES: %w", err)
	}
	self, err := Require("ENGRAM_NODE_ID")
	if err != nil {
		return nil, err
	}
	found := false
	for _, n := range nodes {
		found = found || n.ID == self
	}
	if !found {
		return nil, fmt.Errorf("ENGRAM_NODE_ID %q isn't in ENGRAM_NODES", self)
	}
	epoch, err := Int("ENGRAM_NODES_EPOCH", 0)
	if err != nil || epoch <= 0 {
		return nil, fmt.Errorf("ENGRAM_NODES_EPOCH must be a positive integer that increases whenever ENGRAM_NODES changes")
	}
	return &routing.Ring{Self: self, Nodes: nodes, Epoch: uint64(epoch)}, nil
}

// Provider builds the embedding provider from ENGRAM_EMBED_* settings.
//
//	ENGRAM_EMBED_URL           OpenAI-compatible base URL (default Ollama's, http://localhost:11434/v1)
//	ENGRAM_EMBED_API_KEY       API key, if the endpoint needs one
//	ENGRAM_EMBED_MODEL         local model name, when it differs from the space's
//	ENGRAM_EMBED_MODEL_DIGEST  the local model's digest, for endpoints that aren't Ollama
//	ENGRAM_EMBED_PROVIDER      openai (default) or hashing (offline, no meaning; dev only)
func Provider() (embed.Provider, error) {
	switch p := Get("ENGRAM_EMBED_PROVIDER", "openai"); p {
	case "hashing":
		return embed.HashingProvider{}, nil
	case "openai":
		return &embed.OpenAIProvider{
			BaseURL:   Get("ENGRAM_EMBED_URL", "http://localhost:11434/v1"),
			APIKey:    Get("ENGRAM_EMBED_API_KEY", ""),
			ModelName: Get("ENGRAM_EMBED_MODEL", ""),
			Digest:    Get("ENGRAM_EMBED_MODEL_DIGEST", ""),
			HTTP:      &http.Client{Timeout: 60 * time.Second},
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
