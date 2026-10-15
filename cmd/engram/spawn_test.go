package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/coord"
)

func didKey(t *testing.T) string {
	t.Helper()
	k, err := atcrypto.GeneratePrivateKeyK256()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := k.PublicKey()
	return pub.DIDKey()
}

func TestSpawnAChainOfAgents(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.net.SetInvites(map[string]int{"invite-1": 2})
	recovery := didKey(t)

	// A person signed in with OAuth spawns the first agent. A person's own
	// rotation keys aren't an offline recovery key, so they name one.
	hailey := w.agent(t)
	hailey.mustRun("login", "--handle", "alice.test")
	if code, _, errOut := hailey.run("agent", "spawn", "kid.test", "--invite", "invite-1"); code != 2 || !strings.Contains(errOut, "--recovery-key") {
		t.Fatalf("spawn by a person without a recovery key: %d %s", code, errOut)
	}
	var out spawnOut
	if err := json.Unmarshal([]byte(hailey.mustRun("agent", "spawn", "kid.test", "--invite", "invite-1", "--recovery-key", recovery, "--description", "researcher", "--json")), &out); err != nil {
		t.Fatal(err)
	}
	if out.Agent.DID != "did:plc:kid" || !out.Verified || !slices.Equal(out.Chain, []string{"did:plc:kid"}) || out.Operator != "did:plc:alice" {
		t.Fatalf("spawn: %+v", out)
	}
	// The PDS got the recovery key first and the child's sign-in key, and
	// no password.
	if keys := w.net.RotationKeys("did:plc:kid"); len(keys) < 2 || keys[0] != recovery {
		t.Fatalf("child's rotation keys %v", keys)
	}
	if sk := w.net.SignInKeys("did:plc:kid"); len(sk) != 1 {
		t.Fatalf("child's sign-in keys %v", sk)
	}
	// The child's key and settings are its own, readable only by the owner.
	st, err := os.Stat(out.KeyFile)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file %s: %v %v", out.KeyFile, st, err)
	}
	child, err := agent.LoadSettings(out.Config, nil)
	if err != nil || child.Account.SignIn != agent.SignInKey || child.Account.DID != "did:plc:kid" || child.Account.KeyFile != out.KeyFile {
		t.Fatalf("child settings %+v %v", child.Account, err)
	}
	// The child signed in with its key to declare; the person signed in once
	// for the operated record.
	if len(hailey.keySignIns) != 1 || !slices.Equal(hailey.keySignIns[0], coord.DeclareScopes) {
		t.Fatalf("child's sign-ins %v", hailey.keySignIns)
	}
	if len(hailey.signInsFor) != 1 || !slices.Equal(hailey.signInsFor[0], coord.OperateScopes) || hailey.ended != 1 || hailey.keyEnded != 1 {
		t.Fatalf("person's sign-ins %v, ended %d/%d", hailey.signInsFor, hailey.ended, hailey.keyEnded)
	}

	// The child, running from its own settings, spawns a grandchild with no
	// browser at all: its recovery key comes from its own rotation keys.
	kid := w.agent(t)
	kid.path = out.Config
	var g spawnOut
	if err := json.Unmarshal([]byte(kid.mustRun("agent", "spawn", "grand.test", "--invite", "invite-1", "--json")), &g); err != nil {
		t.Fatal(err)
	}
	if !g.Verified || !slices.Equal(g.Chain, []string{"did:plc:grand", "did:plc:kid"}) || g.Operator != "did:plc:alice" || g.RecoveryKey != recovery {
		t.Fatalf("grandchild: %+v", g)
	}
	if len(kid.signInsFor) != 0 {
		t.Fatalf("a key account opened a browser sign-in: %v", kid.signInsFor)
	}
	if len(kid.keySignIns) != 2 { // the grandchild's declaration, the kid's operated record
		t.Fatalf("key sign-ins %v", kid.keySignIns)
	}

	// verify shows the whole chain.
	var v agentOut
	if err := json.Unmarshal([]byte(kid.mustRun("agent", "verify", "did:plc:grand", "--json")), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Verified || !slices.Equal(v.Chain, []string{"did:plc:grand", "did:plc:kid"}) || v.Operator != "did:plc:alice" || v.RecoveryKey != recovery {
		t.Fatalf("verify: %+v", v)
	}

	// The invite is used up.
	if code, _, errOut := kid.run("agent", "spawn", "third.test", "--invite", "invite-1"); code != 1 || !strings.Contains(errOut, "InvalidInviteCode") {
		t.Fatalf("spawn past the invite's uses: %d %s", code, errOut)
	}
	// The failed spawn left no key behind to confuse a retry.
	if _, err := os.Stat(filepath.Join(kid.children, "third.test")); !os.IsNotExist(err) {
		t.Fatalf("leftover from a failed spawn: %v", err)
	}
}

func TestSpawnRefusesAnExistingAgentDir(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a := w.agent(t)
	a.mustRun("login", "--handle", "alice.test")
	a.mustRun("agent", "spawn", "kid.test", "--recovery-key", didKey(t))
	if code, _, errOut := a.run("agent", "spawn", "kid.test", "--recovery-key", didKey(t)); code != 1 || !strings.Contains(errOut, "already") {
		t.Fatalf("second spawn into the same dir: %d %s", code, errOut)
	}
}
