// Command engram-mcp gives one agent remember/recall tools over MCP (stdio)
// for an Engram Garden memory space. It embeds memories and queries itself,
// with the model the space declares, through an OpenAI-compatible endpoint
// (Ollama by default).
//
// It reads the settings the engram CLI writes (~/.config/engram/config.json,
// or $ENGRAM_CONFIG_DIR), and ENGRAM_* environment variables override them:
// ENGRAM_SPACES, ENGRAM_SPACE, ENGRAM_IDENTIFIER / ENGRAM_PASSWORD, ENGRAM_APPVIEW_URL,
// ENGRAM_APPVIEW_DID, ENGRAM_PDS_HOST and ENGRAM_EMBED_*.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
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

const usage = `engram-mcp: Engram Garden memory tools (list_spaces, remember, recall,
get_memory, list_memories, forget) for an MCP client, over stdio.

Set it up once with the engram CLI (engram login, then engram spaces add
<space URI> or engram create <name>), then configure your MCP client to run it:

  {"mcpServers": {"engram": {"command": "engram-mcp"}}}

ENGRAM_* environment variables override the settings; see the README.
`

func main() {
	for _, a := range os.Args[1:] {
		if a == "-h" || a == "--help" || a == "help" || a == "--version" {
			fmt.Print(usage)
			return
		}
	}
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
	store := &oauthfile.FileStore{Dir: filepath.Join(dir, "oauth")}
	store.Sweep() // sign-ins nobody finished
	a, err := agent.Open(ctx, settings, agent.Options{Store: store, LockDir: store.Dir, HTTP: &http.Client{Timeout: time.Minute}, Log: log})
	if err != nil {
		return fmt.Errorf("signing in: %w", err)
	}
	if exp := settings.Account.Expires(); !exp.IsZero() && time.Until(exp) < 3*24*time.Hour {
		log.Warn("the agent's sign-in ends soon; run `engram login` to renew it", "ends", exp.Format(time.RFC3339))
	}
	// A space created through create_space is saved to the settings file
	// (not ENGRAM_* overrides, which stay the environment's).
	path := filepath.Join(dir, "config.json")
	a.Save = func(current agent.Settings) error {
		file, err := agent.LoadSettings(path, nil)
		if err != nil {
			return err
		}
		for _, e := range current.Spaces {
			_, _ = file.AddSpace(e.URI, "")
		}
		return file.Save(path)
	}
	names := make([]string, 0, len(settings.Spaces))
	for _, e := range settings.Spaces {
		names = append(names, e.Name)
		sp, _, err := a.Agent(e.URI)
		if err != nil {
			continue
		}
		if cfg, err := sp.Config(ctx, true); err != nil {
			log.Warn("can't read the space's model yet", "space", e.Name, "err", agent.Explain(err))
		} else if _, err := a.Provider.For(ctx, cfg.ModelInfo); err != nil {
			log.Warn("the local embedding model doesn't match the space's; remember and recall there will fail until it does", "space", e.Name, "err", err)
		}
	}
	// Warm the spaces and keep this agent's memories embedded with each
	// space's model(s), including during a model change.
	go a.Run(ctx, 15*time.Minute)
	def, _ := settings.Default()
	log.Info("engram-mcp ready", "account", a.Client.DID(), "spaces", names, "default", def.Name, "appview", settings.AppviewURL)
	var opts []mcpserver.Option
	if p := os.Getenv("ENGRAM_QUERY_LOG"); p != "" {
		// Opt-in: recall queries and results, kept on this machine for
		// evaluating search (engram-eval gen -log).
		opts = append(opts, mcpserver.WithQueryLog(mcpserver.NewQueryLog(p, log)))
		log.Info("logging recall queries", "file", p)
	}
	return mcpserver.NewServer(a, opts...).Run(ctx, &mcp.StdioTransport{})
}
