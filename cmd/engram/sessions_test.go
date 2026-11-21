package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

// TestArchiveCommands: the principal makes an archive, identities join it
// (one stocked before it existed too), an identity keeps its session there,
// and the principal lists, pulls and deletes it from the shell.
func TestArchiveCommands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := spacetest.NewTyped(t, "principal", lex.ArchiveType, "sessions")
	n.AddMember(n.Authority.DID)
	dir := t.TempDir()
	scopes := []string{"atproto"} // a sign-in from before archives
	opts := agent.Options{Dir: n.Dir}
	run := func(args ...string) (int, string, string) {
		t.Helper()
		var out, errOut bytes.Buffer
		c := &cli{
			in: strings.NewReader(""), out: &out, err: &errOut,
			getenv:        func(k string) string { return map[string]string{"ENGRAM_EMBED_PROVIDER": "hashing"}[k] },
			configPath:    filepath.Join(dir, "config.json"),
			principalPath: filepath.Join(dir, "principal.json"),
			identitiesDir: filepath.Join(dir, "agents"),
			principalSignIn: func(_ context.Context, handle string, _ <-chan string) (agent.Account, []string, error) {
				return agent.Account{Handle: handle, DID: n.Authority.DID, SignIn: agent.SignInOAuth, SessionID: "p1", SignedInAt: time.Now()}, scopes, nil
			},
			principalSession: func(context.Context, agent.Principal) (*atclient.APIClient, error) {
				return n.Session(n.Authority), nil
			},
			identityClient: func(ctx context.Context, s agent.Settings) (*spaceclient.Client, agent.Identity, error) {
				return agent.IdentityClient(ctx, s, opts)
			},
			spaceClient: func(sess *atclient.APIClient) (*spaceclient.Client, error) {
				return spaceclient.New(sess, n.Dir, nil)
			},
			revoke: func(context.Context, agent.Account) error { return nil },
		}
		code := c.run(ctx, args)
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

	must("principal", "login", "--handle", "principal.test")
	must("principal", "set", "--stock", "1")
	must("agent", "stock") // before there's an archive

	// A sign-in from before archives says to sign in again.
	if code, out, errOut := run("archive", "create", "elsewhere"); code == 0 || !strings.Contains(out+errOut, "engram principal login") {
		t.Fatalf("archive create with an old sign-in: %d %s%s", code, out, errOut)
	}
	scopes = agent.PrincipalScopes("did:web:pds.test")
	must("principal", "login")
	if out := must("archive", "create", "elsewhere"); !strings.Contains(out, "at://"+n.Authority.DID+"/space/"+lex.ArchiveType+"/elsewhere") {
		t.Fatalf("archive create: %s", out)
	}
	// The test network serves one space: use that one.
	must("principal", "set", "--archive", n.Space)
	if out := must("archive", "model", "hashing-64", "--dims", "64"); !strings.Contains(out, "embedded with hashing-64") {
		t.Fatalf("archive model: %s", out)
	}
	var cfgOut struct {
		Value map[string]any `json:"value"`
	}
	if err := n.Session(n.Authority).Get(ctx, "com.atproto.space.getRecord", map[string]any{"space": n.Space, "repo": n.Authority.DID, "collection": lex.ConfigCollection, "rkey": lex.ConfigRkey}, &cfgOut); err != nil || cfgOut.Value["model"] != "hashing-64" {
		t.Fatalf("the archive's config: %+v %v", cfgOut, err)
	}
	if out := must("principal"); !strings.Contains(out, "archive:        "+n.Space) {
		t.Fatalf("principal: %s", out)
	}

	// The stocked identity names an archive the principal no longer uses
	// (as if it switched archives): it joins the current one when claimed.
	stocked, _ := filepath.Glob(filepath.Join(dir, "agents", "stock", "*.json"))
	if len(stocked) != 1 {
		t.Fatalf("stock: %v", stocked)
	}
	raw, err := os.ReadFile(stocked[0])
	if err != nil {
		t.Fatal(err)
	}
	var stale map[string]any
	_ = json.Unmarshal(raw, &stale)
	stale["archive"], stale["archived"] = "at://"+n.Authority.DID+"/space/"+lex.ArchiveType+"/old", true
	raw, _ = json.Marshal(stale)
	if err := os.WriteFile(stocked[0], raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The stocked identity joins the archive when it's claimed.
	var claimed struct {
		DID  string `json:"did"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(must("agent", "new", "--json", "--kind", "session", "--session", "s1", "--no-refill")), &claimed); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(n.Members(n.Space), claimed.DID) {
		t.Fatalf("%s isn't in the archive: %v", claimed.DID, n.Members(n.Space))
	}
	id, err := agent.LoadIdentity(claimed.Path)
	if err != nil || id.Archive != n.Space || !id.Archived {
		t.Fatalf("identity: %+v %v", id, err)
	}

	// The agent keeps its session, as engram-mcp would.
	c, id, err := agent.IdentityClient(ctx, agent.Settings{Identity: claimed.Path}, opts)
	if err != nil {
		t.Fatal(err)
	}
	ar, err := agent.NewArchive(c, id, agent.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	data := `{"type":"session","id":"h"}` + "\n" + `{"type":"message","id":"m1","message":{"role":"user","content":"audit the handlers"}}` + "\n"
	if _, err := ar.PushSession(ctx, agent.PushSessionIn{Session: lex.Session{SessionID: "s1", Title: "audit the handlers", Machine: "box"}, Data: data}); err != nil {
		t.Fatal(err)
	}

	if out := must("sessions", "list"); !strings.Contains(out, "s1  audit the handlers") || !strings.Contains(out, claimed.DID) || !strings.Contains(out, "2 entries") {
		t.Fatalf("list: %s", out)
	}
	if out := must("sessions", "pull", "s1"); out != data {
		t.Fatalf("pull: %q", out)
	}
	if out := must("sessions", "rm", "s1"); !strings.Contains(out, "Deleted at://") {
		t.Fatalf("rm: %s", out)
	}
	if out := must("sessions", "list"); !strings.Contains(out, "No sessions") {
		t.Fatalf("list after rm: %s", out)
	}
}
