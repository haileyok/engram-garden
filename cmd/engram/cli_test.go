package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/appview"
	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacestore"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

const serviceDID = "did:web:engram.test"

var model = lex.ModelInfo{Model: "hashing-256", ModelDigest: embed.HashingDigest, Dims: 256}

// world is a memory space with two member agents and an appview indexing
// it. Signing in is faked: an account's session is its spacetest session.
type world struct {
	net *spacetest.Net
	av  *appview.Server
	url string
	// signInErr makes the next opened session fail, as an expired one does.
	signInErr error
}

func newWorld(t *testing.T) *world {
	t.Helper()
	n := spacetest.New(t)
	indexerAcct := n.NewAccount("did:plc:indexer")
	for _, did := range []string{"did:plc:alice", "did:plc:bob"} {
		n.AddMember(n.NewAccount(did).DID)
	}
	n.NewAccount("did:plc:outsider")
	n.AddMember(indexerAcct.DID)
	ic, err := spaceclient.New(n.Session(indexerAcct), n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := spacestore.New(spacestore.Options{Blob: blob.Dir{Root: t.TempDir()}, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	av := &appview.Server{Store: st, Dir: n.Dir, ServiceDID: serviceDID, Spaces: []string{n.Space},
		Indexer: &indexer.Indexer{Store: st, Client: ic, Dir: n.Dir}}
	hs := httptest.NewServer(av.Handler())
	t.Cleanup(hs.Close)
	n.RegisterService(serviceDID, appview.SyncerFragment, hs.URL)
	n.Put(n.Authority, lex.ConfigCollection, lex.ConfigRkey, lex.Config{ModelInfo: model}.Record(time.Now()))
	w := &world{net: n, av: av, url: hs.URL}
	if _, err := av.Indexer.Register(context.Background(), n.Space, av.ServiceID()); err != nil {
		t.Fatal(err)
	}
	if err := av.Indexer.SyncSpace(context.Background(), n.Space); err != nil {
		t.Fatal(err)
	}
	return w
}

// deliver tells the appview about an account's writes and waits for them.
func (w *world) deliver(did string) {
	w.net.DeliverWrite(w.net.AccountByDID(did), "")
	w.av.Jobs.Wait()
}

// agentCLI is one agent's engram command, with its own settings file.
type agentCLI struct {
	t     *testing.T
	w     *world
	path  string
	env   map[string]string
	stdin string
}

func (w *world) agent(t *testing.T) *agentCLI {
	return &agentCLI{t: t, w: w, path: filepath.Join(t.TempDir(), "engram", "config.json"),
		env: map[string]string{"ENGRAM_APPVIEW_URL": w.url, "ENGRAM_APPVIEW_DID": serviceDID, "ENGRAM_EMBED_PROVIDER": "hashing"}}
}

// run runs a command, returning its exit code, stdout and stderr.
func (a *agentCLI) run(args ...string) (int, string, string) {
	a.t.Helper()
	var out, errOut bytes.Buffer
	c := &cli{
		in: strings.NewReader(a.stdin), out: &out, err: &errOut,
		getenv:     func(k string) string { return a.env[k] },
		configPath: a.path,
		open: func(ctx context.Context, s agent.Settings) (*agent.Agent, error) {
			if a.w.signInErr != nil {
				return nil, a.w.signInErr
			}
			p, err := s.Provider()
			if err != nil {
				return nil, err
			}
			acct := a.w.net.AccountByDID(s.Account.DID)
			sc, err := spaceclient.New(a.w.net.Session(acct), a.w.net.Dir, nil)
			if err != nil {
				return nil, err
			}
			return &agent.Agent{Client: sc, Space: s.Space, AppviewURL: s.AppviewURL, AppviewDID: s.AppviewDID, Provider: p}, nil
		},
		signIn: func(_ context.Context, handle, password string) (agent.Account, error) {
			did := "did:plc:" + strings.TrimSuffix(handle, ".test")
			if password != "" {
				return agent.Account{Handle: handle, DID: did, SignIn: agent.SignInPassword, Password: password}, nil
			}
			return agent.Account{Handle: handle, DID: did, SignIn: agent.SignInOAuth, SessionID: "s", Callback: "http://127.0.0.1:1/callback", SignedInAt: time.Now()}, nil
		},
		readSecret: func(string) (string, error) { return "typed-password", nil },
	}
	code := c.run(context.Background(), args)
	a.stdin = ""
	return code, out.String(), errOut.String()
}

func (a *agentCLI) mustRun(args ...string) string {
	a.t.Helper()
	code, out, errOut := a.run(args...)
	if code != 0 {
		a.t.Fatalf("engram %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

func TestAgentsRememberAndRecall(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	alice, bob := w.agent(t), w.agent(t)
	if out := alice.mustRun("init", "--space", w.net.Space, "--handle", "alice.test"); !strings.Contains(out, "Everything works") {
		t.Fatalf("init: %s", out)
	}
	bob.mustRun("init", "--space", w.net.Space, "--handle", "bob.test")

	// Flags may come after the text.
	out := alice.mustRun("remember", "attie", "deploys", "go", "through", "the", "deploy", "workflow", "-t", "ops", "--source", "runbook")
	if !strings.Contains(out, "/did:plc:alice/garden.engram.memory/") {
		t.Fatalf("remember: %s", out)
	}
	alice.stdin = "hailey prefers short answers\n"
	alice.mustRun("remember", "-t", "prefs")
	w.deliver("did:plc:alice")

	var found agent.MemoriesOut
	if err := json.Unmarshal([]byte(bob.mustRun("recall", "how do attie deploys work", "-n", "1", "--json")), &found); err != nil {
		t.Fatal(err)
	}
	if len(found.Memories) != 1 || found.Memories[0].Author != "did:plc:alice" || found.Memories[0].Source != "runbook" ||
		len(found.Memories[0].Tags) != 1 || found.Memories[0].Similarity == nil {
		t.Fatalf("recall: %+v", found)
	}
	uri := found.Memories[0].URI
	if out := bob.mustRun("recall", "deploy", "workflow"); !strings.Contains(out, "attie deploys") || !strings.Contains(out, uri) {
		t.Fatalf("recall text: %s", out)
	}
	if out := bob.mustRun("list", "-t", "prefs"); !strings.Contains(out, "short answers") || strings.Contains(out, "attie") {
		t.Fatalf("list by tag: %s", out)
	}
	if out := bob.mustRun("list", "--mine"); !strings.Contains(out, "No memories found") {
		t.Fatalf("bob's own memories: %s", out)
	}
	if out := bob.mustRun("get", uri); !strings.Contains(out, "attie deploys") {
		t.Fatalf("get: %s", out)
	}

	// Only the author can forget a memory.
	if code, _, errOut := bob.run("forget", uri); code != 1 || !strings.Contains(errOut, "only forget your own") {
		t.Fatalf("bob forgetting alice's memory: %d %s", code, errOut)
	}
	alice.mustRun("forget", uri)
	w.deliver("did:plc:alice")
	if code, out, _ := bob.run("get", uri, "--json"); code != 1 || !strings.Contains(out, `"error"`) {
		t.Fatalf("forgotten memory: %d %s", code, out)
	}
}

func TestInitSavesSettings(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a := w.agent(t)
	a.stdin = w.net.Space + "\nalice.test\n" // answered at the prompts
	var r checkResult
	if err := json.Unmarshal([]byte(a.mustRun("init", "--json")), &r); err != nil {
		t.Fatal(err)
	}
	if !r.OK || r.DID != "did:plc:alice" || r.Memories == nil || r.Expires == nil {
		t.Fatalf("init: %+v", r)
	}
	s, err := agent.LoadSettings(a.path, nil)
	if err != nil || s.Space != w.net.Space || s.Account.SignIn != agent.SignInOAuth || s.Account.Password != "" {
		t.Fatalf("saved settings: %+v %v", s, err)
	}

	// A machine without a browser signs in with the password instead.
	a.mustRun("login", "--password")
	if s, _ := agent.LoadSettings(a.path, nil); s.Account.SignIn != agent.SignInPassword || s.Account.Password != "typed-password" {
		t.Fatalf("password login: %+v", s.Account)
	}
}

// TestInitNotAMember: an account the authority hasn't added gets told what
// to ask for.
func TestInitNotAMember(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a := w.agent(t)
	code, out, _ := a.run("init", "--space", w.net.Space, "--handle", "outsider.test")
	if code != 1 || !strings.Contains(out, "add did:plc:outsider as a member") {
		t.Fatalf("init as an outsider: %d %s", code, out)
	}
}

func TestNotSetUp(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a := w.agent(t)
	if code, _, errOut := a.run("recall", "anything"); code != 1 || !strings.Contains(errOut, "engram init") {
		t.Fatalf("recall before init: %d %s", code, errOut)
	}
	if code, _, errOut := a.run("remember"); code != 2 || !strings.Contains(errOut, "nothing to remember") {
		t.Fatalf("empty remember: %d %s", code, errOut)
	}
	if code, out, _ := a.run("help"); code != 0 || !strings.Contains(out, "engram init") {
		t.Fatalf("help: %d %s", code, out)
	}
}

// TestExpiredSignIn: an OAuth sign-in the server no longer accepts says
// how to fix it.
func TestExpiredSignIn(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a := w.agent(t)
	a.mustRun("init", "--space", w.net.Space, "--handle", "alice.test")
	w.signInErr = errors.New("token refresh failed (HTTP 400): invalid_grant")
	if code, _, errOut := a.run("recall", "anything"); code != 1 || !strings.Contains(errOut, "run `engram login`") {
		t.Fatalf("expired sign-in: %d %s", code, errOut)
	}
}

// TestEnvOverridesSettings: ENGRAM_* variables work without a settings file.
func TestEnvOverridesSettings(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a := w.agent(t)
	a.env["ENGRAM_SPACE"] = w.net.Space
	a.env["ENGRAM_IDENTIFIER"] = "alice.test"
	a.env["ENGRAM_PASSWORD"] = "pw"
	// The fake open finds the account by DID, which a password settings
	// file would have from signing in; ENGRAM_IDENTIFIER alone doesn't.
	s, err := agent.LoadSettings(a.path, func(k string) string { return a.env[k] })
	if err != nil || s.Account.SignIn != agent.SignInPassword || s.Space != w.net.Space {
		t.Fatalf("env settings: %+v %v", s, err)
	}
}
