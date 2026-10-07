package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

// TestDemo serves the built frontend against an in-memory network, for
// working on the UI without a PDS:
//
//	make web && ENGRAM_WEB_DEMO=1 go test -run TestDemo -timeout 0 ./internal/web/
//
// It prints a URL that signs you in as the space's authority. Set
// ENGRAM_WEB_DEMO_SECONDS to stop after a while.
func TestDemo(t *testing.T) {
	t.Parallel()
	if os.Getenv("ENGRAM_WEB_DEMO") == "" {
		t.Skip("set ENGRAM_WEB_DEMO=1 to serve the web app against an in-memory network")
	}
	static := Frontend()
	if static == nil {
		t.Fatal("build the frontend first: make web")
	}
	f := setup(t)
	n := f.net
	for _, h := range []struct {
		a      *spacetest.Account
		handle string
	}{{n.Authority, "hailey.test"}, {f.alice, "scout.agents.test"}, {f.bob, "archivist.agents.test"}, {f.appview, "appview.engram.test"}} {
		ident, _ := n.Dir.LookupDID(context.Background(), syntax.DID(h.a.DID))
		n.Dir.Insert(identity.Identity{DID: ident.DID, Handle: syntax.Handle(h.handle), Keys: ident.Keys, Services: ident.Services})
	}
	seed := []struct {
		a    *spacetest.Account
		text string
		tags []string
	}{
		{f.alice, "pop1 deploys go through the deploy repo's Deploy Attie workflow, one SHA for every service.", []string{"infra", "deploys"}},
		{f.bob, "Hailey prefers short answers that lead with the recommendation.", []string{"prefs"}},
		{f.alice, "The appview's notification receiver only accepts service auth from the space authority.", []string{"engram", "auth"}},
		{f.bob, "Wasabi bills a 90-day minimum per object, so merges leave old segments for garbage collection.", []string{"storage"}},
		{n.Authority, "Remember to rotate the appview's account password before launch.", []string{"todo"}},
	}
	for i, m := range seed {
		n.Put(m.a, lex.MemoryCollection, "seed"+strconv.Itoa(i), memory(m.text, m.tags...))
	}
	f.sync(t)

	ln, err := net.Listen("tcp", "127.0.0.1:"+envOr("ENGRAM_WEB_DEMO_PORT", "8091"))
	if err != nil {
		t.Fatal(err)
	}
	demo := &Server{Auth: f.web.Auth, Dir: n.Dir, AppviewURL: f.web.AppviewURL, AppviewDID: appviewDID,
		Origin: "http://" + ln.Addr().String(), CookieKey: f.web.CookieKey, Static: static, LiveEvery: 2 * time.Second}
	mux := http.NewServeMux()
	mux.Handle("/", demo.Handler())
	// Sign in by visiting /demo-signin?as=authority|alice|bob|mallory.
	accounts := map[string]*spacetest.Account{"authority": n.Authority, "alice": f.alice, "bob": f.bob, "mallory": f.mallory}
	mux.HandleFunc("/demo-signin", func(w http.ResponseWriter, r *http.Request) {
		a := accounts[r.URL.Query().Get("as")]
		if a == nil {
			a = n.Authority
		}
		http.SetCookie(w, demo.sessionCookie(syntax.DID(a.DID), "s-"+a.DID))
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	hs := &http.Server{Handler: mux}
	go func() { _ = hs.Serve(ln) }()
	defer hs.Close()
	fmt.Printf("demo: %s/demo-signin?as=authority\nspace: %s\n", demo.Origin, n.Space)

	// An agent keeps remembering things, for the live view.
	stop := time.After(24 * time.Hour)
	if s := os.Getenv("ENGRAM_WEB_DEMO_SECONDS"); s != "" {
		secs, _ := strconv.Atoi(s)
		stop = time.After(time.Duration(secs) * time.Second)
	}
	tick := time.NewTicker(6 * time.Second)
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		case <-tick.C:
			n.Put(f.alice, lex.MemoryCollection, "live"+strconv.Itoa(i), memory(fmt.Sprintf("Live memory #%d from the scout agent.", i+1), "live"))
			f.sync(t)
		}
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
