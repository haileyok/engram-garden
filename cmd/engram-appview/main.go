// Command engram-appview indexes Engram Garden memory spaces and serves
// semantic search over them.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/appview"
	"github.com/haileyok/engram-garden/internal/config"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn, err := config.Require("ENGRAM_DATABASE_URL")
	if err != nil {
		return err
	}
	serviceDID, err := config.Require("ENGRAM_SERVICE_DID")
	if err != nil {
		return err
	}
	spaces := config.List("ENGRAM_SPACES")
	if len(spaces) == 0 {
		return errors.New("ENGRAM_SPACES is required: comma-separated space URIs to index")
	}
	for _, sp := range spaces {
		if _, err := space.ParseRef(sp); err != nil {
			return fmt.Errorf("ENGRAM_SPACES: %s: %w", sp, err)
		}
	}
	publicURL := config.Get("ENGRAM_PUBLIC_URL", "")
	poll, err := time.ParseDuration(config.Get("ENGRAM_POLL_INTERVAL", "5m"))
	if err != nil {
		return fmt.Errorf("ENGRAM_POLL_INTERVAL: %w", err)
	}

	emb, err := config.Embedder()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, dsn, emb.Dimensions())
	if err != nil {
		return err
	}
	defer st.Close()

	dir := config.Directory()
	session, err := config.Login(ctx, dir)
	if err != nil {
		return fmt.Errorf("logging in: %w", err)
	}
	client, err := spaceclient.New(session, dir, nil)
	if err != nil {
		return err
	}
	srv := &appview.Server{
		Store:      st,
		Embedder:   emb,
		Indexer:    &indexer.Indexer{Store: st, Embedder: emb, Client: client, Dir: dir, Log: log},
		Dir:        dir,
		Log:        log,
		ServiceDID: serviceDID,
		PublicURL:  publicURL,
		Spaces:     spaces,
	}

	// Notifications need a public endpoint; without one, polling alone keeps
	// the index current.
	register := publicURL != ""
	if !register {
		log.Warn("ENGRAM_PUBLIC_URL unset: not registering for notifications, relying on polling", "interval", poll)
	}
	go srv.Run(ctx, poll, register)

	addr := config.Get("ENGRAM_LISTEN", ":8080")
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	log.Info("engram-appview listening", "addr", addr, "service", srv.ServiceID(), "account", client.DID(), "spaces", spaces, "model", emb.Model())
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	srv.Jobs.Wait()
	return nil
}
