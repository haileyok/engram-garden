// Command engram-mcp gives one agent remember/recall tools over MCP (stdio)
// for an Engram Garden memory space. It embeds memories and queries itself,
// with the model the space declares, through an OpenAI-compatible endpoint
// (Ollama by default).
//
// It reads the settings `engram init` writes (~/.config/engram/config.json,
// or $ENGRAM_CONFIG_DIR), and ENGRAM_* environment variables override them:
// ENGRAM_SPACE, ENGRAM_IDENTIFIER / ENGRAM_PASSWORD, ENGRAM_APPVIEW_URL,
// ENGRAM_APPVIEW_DID, ENGRAM_PDS_HOST and ENGRAM_EMBED_*.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/mcpserver"
	"github.com/haileyok/engram-garden/internal/oauthfile"
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

	dir, err := agent.ConfigDir()
	if err != nil {
		return err
	}
	settings, err := agent.LoadSettings(filepath.Join(dir, "config.json"), os.Getenv)
	if err != nil {
		return err
	}
	a, err := agent.Open(ctx, settings, agent.Options{Store: &oauthfile.FileStore{Dir: filepath.Join(dir, "oauth")}, Log: log})
	if err != nil {
		return fmt.Errorf("signing in: %w", err)
	}
	if exp := settings.Account.Expires(); !exp.IsZero() && time.Until(exp) < 3*24*time.Hour {
		log.Warn("the agent's sign-in ends soon; run `engram login` to renew it", "ends", exp.Format(time.RFC3339))
	}
	if cfg, err := a.Config(ctx, true); err != nil {
		log.Warn("can't read the space's model yet", "err", agent.Explain(err))
	} else if _, err := a.Provider.For(ctx, cfg.ModelInfo); err != nil {
		log.Warn("the local embedding model doesn't match the space's; remember and recall will fail until it does", "err", err)
	}
	// Warm the space and keep this agent's memories embedded with the
	// space's model(s), including during a model change.
	go a.Run(ctx, 15*time.Minute)
	log.Info("engram-mcp ready", "account", a.Client.DID(), "space", settings.Space, "appview", settings.AppviewURL)
	return mcpserver.NewServer(a).Run(ctx, &mcp.StdioTransport{})
}
