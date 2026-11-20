package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/appview"
	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/lounge"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacestore"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

// TestAgentIdentityCommands: the principal sets up, identities are stocked
// and handed out (declared and endorsed), and an agent uses the lounge from
// the shell as its identity.
func TestAgentIdentityCommands(t *testing.T) {
	t.Parallel()
	n := spacetest.NewTyped(t, "principal", lex.LoungeType, "agents")
	n.AutoDeliver = true
	avAcct := n.NewAccount("did:plc:appview")
	n.AddMember(avAcct.DID)
	ac, err := spaceclient.New(n.Session(avAcct), n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := spacestore.New(spacestore.Options{Blob: blob.Dir{Root: t.TempDir()}, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	av := &appview.Server{Store: st, Dir: n.Dir, ServiceDID: serviceDID, Spaces: []string{n.Space},
		Indexer: &indexer.Indexer{Store: st, Client: ac, Dir: n.Dir},
		Lounge:  &lounge.Service{Reader: &indexer.Reader{Client: ac, Dir: n.Dir}, Dir: n.Dir}}
	hs := httptest.NewServer(av.Handler())
	t.Cleanup(hs.Close)
	av.PublicURL = hs.URL
	if _, err := av.Indexer.Register(context.Background(), n.Space, av.Notify(n.Space)); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	env := map[string]string{"ENGRAM_APPVIEW_URL": hs.URL, "ENGRAM_APPVIEW_DID": serviceDID}
	run := func(args ...string) (int, string, string) {
		t.Helper()
		var out, errOut bytes.Buffer
		c := &cli{
			in: strings.NewReader(""), out: &out, err: &errOut,
			getenv:        func(k string) string { return env[k] },
			configPath:    filepath.Join(dir, "config.json"),
			principalPath: filepath.Join(dir, "principal.json"),
			identitiesDir: filepath.Join(dir, "agents"),
			principalSignIn: func(_ context.Context, handle string, _ <-chan string) (agent.Account, []string, error) {
				return agent.Account{Handle: handle, DID: n.Authority.DID, SignIn: agent.SignInOAuth, SessionID: "p1", SignedInAt: time.Now()},
					agent.PrincipalScopes("did:web:pds.test"), nil
			},
			principalSession: func(context.Context, agent.Principal) (*atclient.APIClient, error) {
				return n.Session(n.Authority), nil
			},
			openLounge: func(ctx context.Context, s agent.Settings) (*agent.Lounge, agent.Identity, error) {
				return agent.OpenLounge(ctx, s, agent.Options{Dir: n.Dir})
			},
			verifyAgent: func(ctx context.Context, did string) (map[string]bool, error) {
				return agent.Principals(ctx, n.Dir, nil, did)
			},
			revoke: func(context.Context, agent.Account) error { return nil },
		}
		code := c.run(context.Background(), args)
		return code, out.String(), errOut.String()
	}
	must := func(args ...string) string {
		t.Helper()
		code, out, errOut := run(args...)
		if code != 0 {
			t.Fatalf("engram %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errOut)
		}
		return out
	}

	// Before the principal is set up, agent new says how.
	if code, out, errOut := run("agent", "new"); code == 0 || !strings.Contains(out+errOut, "engram principal login") {
		t.Fatalf("agent new without a principal: %d %s%s", code, out, errOut)
	}
	must("principal", "login", "--handle", "principal.test")
	must("principal", "set", "--recovery-key", "did:key:zQ3shZc2QzApp2oymGvQbzP8eKheVshBHbU4ZYjeXqwSKEn6N", "--lounge", n.Space, "--stock", "1")
	if out := must("principal"); !strings.Contains(out, n.Authority.DID) || !strings.Contains(out, n.Space) {
		t.Fatalf("principal: %s", out)
	}
	if out := must("agent", "stock"); !strings.Contains(out, "Made 1; 1 ready.") {
		t.Fatalf("stock: %s", out)
	}

	var claimed struct {
		DID       string `json:"did"`
		Path      string `json:"path"`
		FromStock bool   `json:"fromStock"`
		Principal string `json:"principal"`
	}
	if err := json.Unmarshal([]byte(must("agent", "new", "--json", "--kind", "session", "--session", "s1", "--no-refill")), &claimed); err != nil {
		t.Fatal(err)
	}
	if !claimed.FromStock || claimed.Principal != n.Authority.DID || claimed.Path == "" {
		t.Fatalf("claimed: %+v", claimed)
	}
	if out := must("agent", "verify", claimed.DID); !strings.Contains(out, "agent of "+n.Authority.DID+": confirmed") {
		t.Fatalf("verify: %s", out)
	}
	if out := must("agent", "list"); !strings.Contains(out, claimed.DID) || !strings.Contains(out, "1 handed out, 0 ready.") {
		t.Fatalf("list: %s", out)
	}

	// As the agent: send to everyone, then read it back.
	env["ENGRAM_IDENTITY"] = claimed.Path
	if out := must("lounge", "send", "hello", "from", "the", "shell"); !strings.Contains(out, "Sent at://") {
		t.Fatalf("send: %s", out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := must("lounge", "messages", "--all")
		if strings.Contains(out, "hello from the shell") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("messages: %s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if out := must("lounge", "agents"); !strings.Contains(out, "No agents online.") {
		t.Fatalf("agents: %s", out)
	}
}
