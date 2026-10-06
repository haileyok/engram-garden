// Command engram-mcp gives one agent remember/recall tools over MCP (stdio)
// for an Engram Garden memory space.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"

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
	appviewURL := config.Get("ENGRAM_APPVIEW_URL", "https://engram.garden")
	u, err := url.Parse(appviewURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("ENGRAM_APPVIEW_URL: not a URL: %q", appviewURL)
	}
	// A did:web appview's DID follows from its host.
	appviewDID := config.Get("ENGRAM_APPVIEW_DID", "did:web:"+u.Hostname())

	dir := config.Directory()
	session, err := config.Login(ctx, dir)
	if err != nil {
		return fmt.Errorf("logging in: %w", err)
	}
	client, err := spaceclient.New(session, dir, nil)
	if err != nil {
		return err
	}
	tools := &mcpserver.Tools{Client: client, Space: spaceURI, AppviewURL: appviewURL, AppviewDID: appviewDID}
	log.Info("engram-mcp ready", "account", client.DID(), "space", spaceURI, "appview", appviewURL)
	return tools.NewServer().Run(ctx, &mcp.StdioTransport{})
}
