// Command engram-mcp gives one agent remember/recall tools over MCP (stdio)
// for an Engram Garden memory space. It embeds memories and queries itself,
// with the model the space declares, through an OpenAI-compatible endpoint
// (Ollama by default).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/haileyok/cocoon/space"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/haileyok/engram-garden/internal/config"
	"github.com/haileyok/engram-garden/internal/mcpserver"
	"github.com/haileyok/engram-garden/internal/spaceclient"
)

func main() {
	// stdout carries the protocol; logs go to stderr.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	spaceURI, err := config.Require("ENGRAM_SPACE")
	if err != nil {
		return err
	}
	if _, err := space.ParseRef(spaceURI); err != nil {
		return fmt.Errorf("ENGRAM_SPACE: %w", err)
	}
	appviewURL := config.Get("ENGRAM_APPVIEW_URL", "https://api.engram.garden")
	u, err := url.Parse(appviewURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("ENGRAM_APPVIEW_URL: not a URL: %q", appviewURL)
	}
	// A did:web appview's DID follows from its host.
	appviewDID := config.Get("ENGRAM_APPVIEW_DID", "did:web:"+u.Hostname())
	provider, err := config.Provider()
	if err != nil {
		return err
	}

	dir := config.Directory()
	session, err := config.Login(ctx, dir)
	if err != nil {
		return fmt.Errorf("logging in: %w", err)
	}
	client, err := spaceclient.New(session, dir, nil)
	if err != nil {
		return err
	}
	tools := &mcpserver.Tools{Client: client, Space: spaceURI, AppviewURL: appviewURL, AppviewDID: appviewDID, Provider: provider, Log: log}
	if cfg, err := tools.Config(ctx, true); err != nil {
		log.Warn("can't read the space's model yet", "err", err)
	} else if _, err := provider.For(ctx, cfg.ModelInfo); err != nil {
		log.Warn("the local embedding model doesn't match the space's; remember and recall will fail until it does", "err", err)
	}
	// Warm the space and keep this agent's memories embedded with the
	// space's model(s), including during a model change.
	go tools.Run(ctx, 15*time.Minute)
	log.Info("engram-mcp ready", "account", client.DID(), "space", spaceURI, "appview", appviewURL)
	return tools.NewServer().Run(ctx, &mcp.StdioTransport{})
}
