// Command engram-appview indexes Engram Garden memory spaces and serves
// vector search over them.
//
//	engram-appview                 run the service
//	engram-appview import <file>   load a space export (from garden.engram.exportSpace)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/appview"
	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/config"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/routing"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacestore"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)
	var err error
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "import":
			if len(os.Args) != 3 {
				err = errors.New("usage: engram-appview import <export.tar>")
				break
			}
			err = runImport(log, os.Args[2])
		default:
			err = fmt.Errorf("unknown command %q", os.Args[1])
		}
	} else {
		err = run(log)
	}
	if err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

// openStore builds the index from ENGRAM_* settings.
//
//	ENGRAM_CACHE_DIR       local disk cache for segment files (default engram-cache)
//	ENGRAM_CACHE_BYTES     disk cache budget (default 100 GiB)
//	ENGRAM_RAM_BYTES       RAM budget for loaded spaces (default 8 GiB)
//	ENGRAM_LIMIT_MEMORIES, ENGRAM_LIMIT_BYTES, ENGRAM_LIMIT_SEARCHES_PER_SECOND,
//	ENGRAM_LIMIT_WRITES_PER_DAY   per-space limits (default unlimited)
func openStore(ctx context.Context, log *slog.Logger, ring *routing.Ring) (*spacestore.Node, blob.Store, error) {
	bs, err := config.Blob()
	if err != nil {
		return nil, nil, err
	}
	// Trust conditional writes only when the bucket proves it honors them.
	cond, err := blob.Probe(ctx, bs)
	if err != nil {
		return nil, nil, fmt.Errorf("probing storage: %w", err)
	}
	log.Info("storage probed", "conditional_writes", cond)
	ints := map[string]int64{}
	for _, k := range []string{"ENGRAM_CACHE_BYTES", "ENGRAM_RAM_BYTES", "ENGRAM_LIMIT_MEMORIES", "ENGRAM_LIMIT_BYTES", "ENGRAM_LIMIT_WRITES_PER_DAY"} {
		if ints[k], err = config.Int(k, 0); err != nil {
			return nil, nil, err
		}
	}
	sps, err := strconv.ParseFloat(config.Get("ENGRAM_LIMIT_SEARCHES_PER_SECOND", "0"), 64)
	if err != nil {
		return nil, nil, errors.New("ENGRAM_LIMIT_SEARCHES_PER_SECOND must be a number")
	}
	n, err := spacestore.New(spacestore.Options{
		Blob:              bs,
		CacheDir:          config.Get("ENGRAM_CACHE_DIR", "engram-cache"),
		CacheBytes:        ints["ENGRAM_CACHE_BYTES"],
		RAMBytes:          ints["ENGRAM_RAM_BYTES"],
		Lease:             ring.Lease,
		ConditionalWrites: cond,
		Limits: spacestore.Limits{
			MaxMemories: int(ints["ENGRAM_LIMIT_MEMORIES"]), MaxBytes: ints["ENGRAM_LIMIT_BYTES"],
			SearchesPerSecond: sps, WritesPerDay: int(ints["ENGRAM_LIMIT_WRITES_PER_DAY"]),
		},
		Log: log,
	})
	return n, bs, err
}

func runImport(log *slog.Logger, path string) error {
	ctx := context.Background()
	ring, err := config.Ring()
	if err != nil {
		return err
	}
	st, _, err := openStore(ctx, log, ring)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sp, err := st.Import(ctx, f)
	if err != nil {
		return err
	}
	log.Info("imported space; add it to ENGRAM_SPACES to serve it", "space", sp)
	return nil
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serviceDID, err := config.Require("ENGRAM_SERVICE_DID")
	if err != nil {
		return err
	}
	spaces := config.List("ENGRAM_SPACES")
	var openRegistration bool
	switch reg := config.Get("ENGRAM_REGISTRATION", "open"); reg {
	case "open":
		openRegistration = true
	case "closed":
		if len(spaces) == 0 {
			return errors.New("ENGRAM_SPACES is required when ENGRAM_REGISTRATION=closed: comma-separated space URIs to index")
		}
	default:
		return fmt.Errorf("ENGRAM_REGISTRATION must be open or closed, not %q", reg)
	}
	for _, sp := range spaces {
		if _, err := space.ParseRef(sp); err != nil {
			return fmt.Errorf("ENGRAM_SPACES: %s: %w", sp, err)
		}
	}
	publicURL := config.Get("ENGRAM_PUBLIC_URL", "")
	poll, err := time.ParseDuration(config.Get("ENGRAM_POLL_INTERVAL", "5m"))
	if err != nil || poll <= 0 {
		return fmt.Errorf("ENGRAM_POLL_INTERVAL must be a positive duration, like 5m")
	}
	ring, err := config.Ring()
	if err != nil {
		return err
	}
	st, bs, err := openStore(ctx, log, ring)
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
	srv := &appview.Server{
		Store:      st,
		Indexer:    &indexer.Indexer{Store: st, Client: client, Dir: dir, Log: log},
		Dir:        dir,
		Log:        log,
		ServiceDID: serviceDID,
		PublicURL:  publicURL,
		Spaces:     spaces,
		Ring:       ring,
		Blob:       bs,

		OpenRegistration: openRegistration,
	}
	if err := srv.LoadRegistrations(ctx); err != nil {
		return fmt.Errorf("reading registered spaces: %w", err)
	}

	// Notifications need a public endpoint; without one, polling alone keeps
	// the index current.
	register := publicURL != ""
	if !register {
		log.Warn("ENGRAM_PUBLIC_URL unset: not registering for notifications, relying on polling", "interval", poll)
	}
	var bg sync.WaitGroup
	bg.Add(2)
	go func() {
		defer bg.Done()
		srv.Run(ctx, poll, register)
	}()
	go func() {
		defer bg.Done()
		st.Run(ctx, time.Minute) // flush full or hour-old buffers
	}()

	addr := config.Get("ENGRAM_LISTEN", ":8080")
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	log.Info("engram-appview listening", "addr", addr, "service", srv.ServiceID(), "account", client.DID(),
		"spaces", srv.Spaces, "indexed", len(srv.IndexedSpaces()), "registration", openRegistration, "node", ring.Self, "nodes", len(ring.Nodes), "epoch", ring.Epoch)
	err = hs.ListenAndServe()
	// Stop the background loops (if the server failed on its own), wait
	// for notification syncs, then publish every buffer.
	stop()
	srv.Jobs.Wait()
	bg.Wait()
	flushCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if cerr := st.Close(flushCtx); cerr != nil {
		log.Error("flushing on shutdown failed", "err", cerr)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
