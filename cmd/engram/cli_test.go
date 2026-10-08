package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// grants is the appview's grants, when it keeps them.
	grants *appview.Grants
	// signInErr makes the next opened session fail, as an expired one does.
	signInErr error
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return newSpaceWorld(t, "authority", "memory")
}

// newSpaceWorld is a world for a space with the given authority and key.
// Agents in several spaces use one world per space.
func newSpaceWorld(t *testing.T, authority, skey string) *world {
	t.Helper()
	return buildWorld(t, authority, skey, false)
}

// newGrantedWorld is a world whose appview keeps grants, with the authority
// having granted access (w.grant). Tests take it away with w.grants.Delete.
func newGrantedWorld(t *testing.T) *world {
	t.Helper()
	return buildWorld(t, "authority", "memory", true)
}

// grant records the authority's grant to the appview.
func (w *world) grant(t *testing.T) {
	t.Helper()
	g := appview.Grant{Space: w.net.Space, DID: w.net.Authority.DID, SessionID: "session", GrantedAt: time.Now()}
	if err := w.grants.Put(context.Background(), g); err != nil {
		t.Fatal(err)
	}
}

func buildWorld(t *testing.T, authority, skey string, withGrants bool) *world {
	t.Helper()
	n := spacetest.NewSpace(t, authority, skey)
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
	var grants *appview.Grants
	if withGrants {
		grants = &appview.Grants{Blob: blob.Dir{Root: t.TempDir()}}
		av.Grants = grants
	}
	hs := httptest.NewServer(av.Handler())
	t.Cleanup(hs.Close)
	n.RegisterService(serviceDID, appview.SyncerFragment, hs.URL)
	n.Put(n.Authority, lex.ConfigCollection, lex.ConfigRkey, lex.Config{ModelInfo: model}.Record(time.Now()))
	w := &world{net: n, av: av, url: hs.URL, grants: grants}
	if withGrants {
		w.grant(t)
	}
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
	// noTerminal reads passwords as lines of stdin, as without a terminal.
	noTerminal bool
	// revoked are the OAuth sessions ended after signing in again.
	revoked []string
	n       int
	// pastes makes OAuth sign-ins take a pasted address from input, as
	// when the browser is on another machine; pasted records them.
	pastes bool
	pasted []string
	// others are more spaces' worlds, by space URI.
	others map[string]*world
}

// join puts the agent in another world's space too (it still has to add the
// space to its settings).
func (a *agentCLI) join(w *world) {
	if a.others == nil {
		a.others = map[string]*world{}
	}
	a.others[w.net.Space] = w
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
		open: func(ctx context.Context, s agent.Settings) (*agent.Spaces, error) {
			if a.w.signInErr != nil {
				return nil, a.w.signInErr
			}
			p, err := s.Provider()
			if err != nil {
				return nil, err
			}
			client := func(w *world) *spaceclient.Client {
				sc, err := spaceclient.New(w.net.Session(w.net.AccountByDID(s.Account.DID)), w.net.Dir, nil)
				if err != nil {
					a.t.Fatal(err)
				}
				return sc
			}
			sp := &agent.Spaces{Client: client(a.w), AppviewURL: s.AppviewURL, AppviewDID: s.AppviewDID, Provider: p, Settings: s}
			// Each space is on its own fake network, with its own appview.
			sp.NewAgent = func(e agent.SpaceEntry) *agent.Agent {
				if w := a.others[e.URI]; w != nil {
					return &agent.Agent{Client: client(w), Space: e.URI, AppviewURL: w.url, AppviewDID: serviceDID, Provider: p}
				}
				return &agent.Agent{Client: sp.Client, Space: e.URI, AppviewURL: s.AppviewURL, AppviewDID: s.AppviewDID, Provider: p}
			}
			return sp, nil
		},
		signIn: func(_ context.Context, handle, password string, lines <-chan string) (agent.Account, error) {
			did := "did:plc:" + strings.TrimSuffix(handle, ".test")
			if password != "" {
				return agent.Account{Handle: handle, DID: did, SignIn: agent.SignInPassword, Password: password}, nil
			}
			if a.pastes {
				line, ok := <-lines
				if !ok {
					return agent.Account{}, errors.New("nothing pasted")
				}
				a.pasted = append(a.pasted, line)
			}
			a.n++
			return agent.Account{Handle: handle, DID: did, SignIn: agent.SignInOAuth, SessionID: fmt.Sprintf("s%d", a.n), Callback: "http://127.0.0.1:1/callback", SignedInAt: time.Now()}, nil
		},
		readSecret: func(string) (string, error) { return "typed-password", nil },
		revoke: func(_ context.Context, acct agent.Account) error {
			a.revoked = append(a.revoked, acct.SessionID)
			return nil
		},
	}
	if a.noTerminal {
		c.readSecret = nil
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
	a.stdin = "alice.test\n" // answered at the prompt
	var r checkResult
	// init still works, as login.
	if err := json.Unmarshal([]byte(a.mustRun("init", "--space", w.net.Space, "--json")), &r); err != nil {
		t.Fatal(err)
	}
	if !r.OK || r.DID != "did:plc:alice" || len(r.Spaces) != 1 || r.Spaces[0].Memories == nil || !r.Spaces[0].Default || r.Expires == nil {
		t.Fatalf("init: %+v", r)
	}
	s, err := agent.LoadSettings(a.path, nil)
	if def, _ := s.Default(); err != nil || def.URI != w.net.Space || s.Account.SignIn != agent.SignInOAuth || s.Account.Password != "" {
		t.Fatalf("saved settings: %+v %v", s, err)
	}

	// A machine without a browser signs in with the password instead.
	a.mustRun("login", "--password")
	if s, _ := agent.LoadSettings(a.path, nil); s.Account.SignIn != agent.SignInPassword || s.Account.Password != "typed-password" {
		t.Fatalf("password login: %+v", s.Account)
	}
}

// TestInitPastedAddress: with every answer piped in, the address pasted
// from the browser reaches the sign-in after the prompts take theirs.
func TestInitPastedAddress(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a := w.agent(t)
	a.pastes = true
	const pasted = "http://127.0.0.1:1234/callback?state=s&code=c"
	a.stdin = "alice.test\n" + pasted + "\n"
	a.mustRun("login", "--space", w.net.Space)
	if len(a.pasted) != 1 || a.pasted[0] != pasted {
		t.Fatalf("pasted: %q", a.pasted)
	}
}

// TestLinesAfterSignIn: once the sign-in reads input line by line, prompts
// read the lines it left, in order, and nothing is lost.
func TestLinesAfterSignIn(t *testing.T) {
	t.Parallel()
	c := &cli{in: strings.NewReader("first\nsecond\nthird")}
	if l, err := c.readLine(); l != "first" || err != nil {
		t.Fatalf("before: %q %v", l, err)
	}
	if l := <-c.lineInput(); l != "second" {
		t.Fatalf("sign-in: %q", l)
	}
	if l, err := c.readLine(); l != "third" || err != nil {
		t.Fatalf("after: %q %v", l, err)
	}
	if _, err := c.readLine(); !errors.Is(err, io.EOF) {
		t.Fatalf("end: %v", err)
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
	if code, _, errOut := a.run("recall", "anything"); code != 1 || !strings.Contains(errOut, "engram spaces add") {
		t.Fatalf("recall before init: %d %s", code, errOut)
	}
	if code, _, errOut := a.run("remember"); code != 2 || !strings.Contains(errOut, "nothing to remember") {
		t.Fatalf("empty remember: %d %s", code, errOut)
	}
	if code, out, _ := a.run("help"); code != 0 || !strings.Contains(out, "engram login") || strings.Contains(out, "engram init") {
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

// TestJSONFailureIsOneDocument: a failed check prints one JSON document,
// so programs can parse it.
func TestJSONFailureIsOneDocument(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a := w.agent(t)
	code, out, _ := a.run("init", "--space", w.net.Space, "--handle", "outsider.test", "--json")
	var r checkResult
	// The problem is the space's: the account isn't a member.
	if err := json.Unmarshal([]byte(out), &r); code != 1 || err != nil || r.OK || len(r.Spaces) != 1 || len(r.Spaces[0].Problems) == 0 {
		t.Fatalf("init --json as an outsider: %d %v %s", code, err, out)
	}
	code, out, _ = a.run("status", "--json")
	if err := json.Unmarshal([]byte(out), &r); code != 1 || err != nil || r.OK {
		t.Fatalf("status --json: %d %v %s", code, err, out)
	}
}

// TestSigningInAgain: login ends the sign-in it replaces, keeps password
// sign-in unless told otherwise, and reads a whole password line.
func TestSigningInAgain(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a := w.agent(t)
	a.mustRun("init", "--space", w.net.Space, "--handle", "alice.test")
	a.mustRun("login")
	if len(a.revoked) != 1 || a.revoked[0] != "s1" {
		t.Fatalf("revoked after login: %v", a.revoked)
	}
	a.noTerminal = true
	a.stdin = "correct horse battery\n"
	if _, _, errOut := a.run("login", "--password"); !strings.Contains(errOut, "password is saved") {
		t.Fatalf("no notice that the password was saved: %s", errOut)
	}
	if len(a.revoked) != 2 || a.revoked[1] != "s2" {
		t.Fatalf("revoked after password login: %v", a.revoked)
	}
	a.stdin = "another one\n"
	a.mustRun("login") // stays with the password
	if s, _ := agent.LoadSettings(a.path, nil); s.Account.SignIn != agent.SignInPassword || s.Account.Password != "another one" {
		t.Fatalf("login after a password sign-in: %+v", s.Account)
	}
}

// TestDoubleDash: text after -- is the memory's, even if it looks like a flag.
func TestDoubleDash(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a := w.agent(t)
	a.mustRun("init", "--space", w.net.Space, "--handle", "alice.test")
	var out agent.RememberOut
	if err := json.Unmarshal([]byte(a.mustRun("remember", "--json", "--", "--json", "-t", "is", "text")), &out); err != nil {
		t.Fatal(err)
	}
	w.deliver("did:plc:alice")
	code, text, _ := a.run("get", out.URI)
	if code != 0 || !strings.Contains(text, "--json -t is text") {
		t.Fatalf("remembered: %s", text)
	}
	if code, _, errOut := a.run("status", "--config"); code != 2 || !strings.Contains(errOut, "needs a path") {
		t.Fatalf("--config without a path: %d %s", code, errOut)
	}
}

// TestSignInEndingSoon: a sign-in about to end is a warning; status still
// succeeds.
func TestSignInEndingSoon(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a := w.agent(t)
	a.mustRun("init", "--space", w.net.Space, "--handle", "alice.test")
	s, _ := agent.LoadSettings(a.path, nil)
	s.Account.SignedInAt = time.Now().Add(-13 * 24 * time.Hour)
	if err := s.Save(a.path); err != nil {
		t.Fatal(err)
	}
	code, out, _ := a.run("status")
	if code != 0 || !strings.Contains(out, "engram login") || !strings.Contains(out, "Everything works") {
		t.Fatalf("status near the end of a sign-in: %d %s", code, out)
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
	if def, _ := s.Default(); err != nil || s.Account.SignIn != agent.SignInPassword || def.URI != w.net.Space {
		t.Fatalf("env settings: %+v %v", s, err)
	}
}

// TestRunningASpaceFromCLI: sign in with no spaces, create one, manage its
// members and model, then sign out.
func TestRunningASpaceFromCLI(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a := w.agent(t)
	// The authority of the world's space signs in, with nothing set up yet.
	if out := a.mustRun("login", "--handle", "authority.test"); !strings.Contains(out, "engram create <name>") {
		t.Fatalf("login with no spaces: %s", out)
	}
	if out := a.mustRun("create", "runbooks"); !strings.Contains(out, "Created at://did:plc:authority/space/garden.engram.space/runbooks") || !strings.Contains(out, "your default space") {
		t.Fatalf("create: %s", out)
	}
	if s, _ := agent.LoadSettings(a.path, nil); len(s.Spaces) != 1 || s.Spaces[0].Name != "runbooks" {
		t.Fatalf("created space not saved: %+v", s.Spaces)
	}
	if out := a.mustRun("members", "add", "did:plc:alice", "--read-only"); !strings.Contains(out, "can now recall (not remember)") {
		t.Fatalf("members add: %s", out)
	}
	var members agent.MembersOut
	// (The test network lists every member as read and write.)
	if err := json.Unmarshal([]byte(a.mustRun("members", "--json")), &members); err != nil || len(members.Members) != 1 || members.Members[0].DID != "did:plc:alice" {
		t.Fatalf("members: %+v %v", members, err)
	}
	a.mustRun("members", "remove", "did:plc:alice")

	// The model of the world's own space, where the test network keeps
	// records.
	a.mustRun("spaces", "add", w.net.Space)
	if out := a.mustRun("model", "--space", "memory"); !strings.Contains(out, "hashing-256") {
		t.Fatalf("model: %s", out)
	}
	if out := a.mustRun("model", "--space", "memory", "--set", "hashing-64", "--dims", "64"); !strings.Contains(out, "hashing-64") {
		t.Fatalf("model --set: %s", out)
	}
	if code, _, errOut := a.run("model", "--promote", "--cancel"); code != 2 || !strings.Contains(errOut, "choose one") {
		t.Fatalf("two actions: %d %s", code, errOut)
	}
	// This appview takes no grants.
	if code, _, errOut := a.run("index", "--space", "memory"); code != 1 || !strings.Contains(errOut, "grants") {
		t.Fatalf("index: %d %s", code, errOut)
	}

	// Someone else can't run it.
	b := w.agent(t)
	b.mustRun("login", "--space", w.net.Space, "--handle", "alice.test")
	if code, _, errOut := b.run("members", "add", "did:plc:bob"); code != 1 || !strings.Contains(errOut, "authority") {
		t.Fatalf("members add as a member: %d %s", code, errOut)
	}

	// logout ends the sign-in and forgets the account; spaces stay.
	before := len(a.revoked)
	a.mustRun("logout")
	s, _ := agent.LoadSettings(a.path, nil)
	if len(a.revoked) != before+1 || s.Account.SignIn != "" || len(s.Spaces) != 2 {
		t.Fatalf("after logout: revoked %v, settings %+v", a.revoked, s)
	}
	if code, _, errOut := a.run("recall", "x"); code != 1 || !strings.Contains(errOut, "engram login") {
		t.Fatalf("recall signed out: %d %s", code, errOut)
	}
}

// TestSeveralSpaces: an agent adds a second space, sees both with their
// models, writes to either, recalls from both or one, and switches the
// default.
func TestSeveralSpaces(t *testing.T) {
	t.Parallel()
	team := newWorld(t)
	notes := newSpaceWorld(t, "noteskeeper", "notes")
	a := team.agent(t)
	a.join(notes)
	a.mustRun("init", "--space", team.net.Space, "--handle", "alice.test")

	// A new space by URI, named after its key; the default stays.
	if out := a.mustRun("spaces", "add", notes.net.Space); !strings.Contains(out, `"notes"`) || !strings.Contains(out, "Default space: memory") {
		t.Fatalf("spaces add: %s", out)
	}
	var listed agent.ListSpacesOut
	if err := json.Unmarshal([]byte(a.mustRun("spaces", "--json")), &listed); err != nil {
		t.Fatal(err)
	}
	set := map[string]agent.SpaceInfo{}
	for _, s := range listed.Spaces {
		if s.SetUp {
			set[s.Name] = s
		}
	}
	if len(set) != 2 || !set["memory"].Default || set["notes"].Model == nil || set["notes"].Model.Dims != model.Dims || set["notes"].LocalModel != "ready" {
		t.Fatalf("spaces: %+v", listed)
	}
	if out := a.mustRun("spaces"); !strings.Contains(out, "* memory") || !strings.Contains(out, "model: hashing-256, 256 dimensions") {
		t.Fatalf("spaces, for people: %s", out)
	}

	a.mustRun("remember", "the", "friday", "deploy", "needs", "two", "approvals")
	team.deliver("did:plc:alice")
	a.mustRun("remember", "--space", "notes", "my", "friday", "deploy", "checklist")
	notes.deliver("did:plc:alice")

	var found agent.MemoriesOut
	if err := json.Unmarshal([]byte(a.mustRun("recall", "friday deploy", "--json")), &found); err != nil {
		t.Fatal(err)
	}
	spaces := map[string]bool{}
	for _, m := range found.Memories {
		spaces[m.Space] = true
	}
	if len(found.Memories) != 2 || !spaces["memory"] || !spaces["notes"] {
		t.Fatalf("recall across spaces: %+v", found)
	}
	if out := a.mustRun("recall", "friday deploy", "--space", "notes"); !strings.Contains(out, "checklist") || strings.Contains(out, "approvals") || !strings.Contains(out, "in notes") {
		t.Fatalf("recall in one space: %s", out)
	}

	// The default space decides where list and remember go.
	a.mustRun("use", "notes")
	if out := a.mustRun("list"); !strings.Contains(out, "checklist") || strings.Contains(out, "approvals") {
		t.Fatalf("list after use: %s", out)
	}
	if code, _, errOut := a.run("use", "nowhere"); code != 2 || !strings.Contains(errOut, "nowhere") {
		t.Fatalf("use an unknown name: %d %s", code, errOut)
	}

	// status checks each space.
	var st checkResult
	if err := json.Unmarshal([]byte(a.mustRun("status", "--json")), &st); err != nil {
		t.Fatal(err)
	}
	if !st.OK || len(st.Spaces) != 2 {
		t.Fatalf("status: %+v", st)
	}

	a.mustRun("spaces", "remove", "notes")
	s, err := agent.LoadSettings(a.path, nil)
	if def, _ := s.Default(); err != nil || len(s.Spaces) != 1 || def.Name != "memory" {
		t.Fatalf("after removing: %+v %v", s, err)
	}
}

// TestSpaceTheAppviewCantRead: the CLI says, where it matters, that memories
// stored in a space the appview isn't allowed to read won't be searchable,
// and what to do about it; the warnings go once the authority approves.
func TestSpaceTheAppviewCantRead(t *testing.T) {
	t.Parallel()
	w := newGrantedWorld(t)
	a := w.agent(t)
	a.mustRun("login", "--space", w.net.Space, "--handle", "alice.test")
	if out := a.mustRun("spaces"); !strings.Contains(out, "indexing: the appview can read it") {
		t.Fatalf("spaces with a grant: %s", out)
	}
	if out := a.mustRun("remember", "the deploy key rotates every friday"); strings.Contains(out, "can't read") {
		t.Fatalf("remember with a grant: %s", out)
	}

	if err := w.grants.Delete(context.Background(), w.net.Space); err != nil {
		t.Fatal(err)
	}
	out := a.mustRun("remember", "staging deploys need a vpn")
	for _, want := range []string{"Remembered: at://", "Stored, but the appview can't read this space", "did:plc:authority", "engram index --space memory"} {
		if !strings.Contains(out, want) {
			t.Errorf("remember lacks %q: %s", want, out)
		}
	}
	if out := a.mustRun("spaces"); !strings.Contains(out, "indexing: The appview can't read this space") {
		t.Fatalf("spaces without a grant: %s", out)
	}
	code, out, _ := a.run("status")
	if code != 1 || !strings.Contains(out, "engram index --space memory") {
		t.Fatalf("status without a grant: %d %s", code, out)
	}

	w.grant(t)
	if out := a.mustRun("remember", "prod deploys need a vpn too"); strings.Contains(out, "can't read") {
		t.Fatalf("remember after the grant: %s", out)
	}
	if code, out, _ := a.run("status"); code != 0 {
		t.Fatalf("status after the grant: %d %s", code, out)
	}
}
