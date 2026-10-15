package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/haileyok/engram-garden/internal/coord"
)

func TestDeclareAndOperate(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	alice, op := w.agent(t), w.agent(t)
	alice.mustRun("login", "--handle", "alice.test")
	op.mustRun("login", "--handle", "authority.test")

	// Nothing declared yet.
	if code, _, errOut := alice.run("agent", "verify", "did:plc:alice"); code != 1 || !strings.Contains(errOut, "no agent declaration") {
		t.Fatalf("verify before declaring: %d %s", code, errOut)
	}

	// The agent signs in once, for just its declaration, and the sign-in
	// ends when it's written.
	out := alice.mustRun("agent", "declare", "--operator", "did:plc:authority", "--description", "researcher")
	if !strings.Contains(out, "did:plc:alice declares did:plc:authority as its operator") || !strings.Contains(out, "engram agent operate did:plc:alice") {
		t.Fatalf("declare: %s", out)
	}
	if len(alice.signInsFor) != 1 || !slices.Equal(alice.signInsFor[0], coord.DeclareScopes) || alice.ended != 1 {
		t.Fatalf("one-time sign-in: asked %v, ended %d", alice.signInsFor, alice.ended)
	}
	// One direction isn't enough.
	if code, _, errOut := alice.run("agent", "verify", "did:plc:alice"); code != 1 || !strings.Contains(errOut, "doesn't claim it back") {
		t.Fatalf("verify one way: %d %s", code, errOut)
	}

	// The operator claims it back, and the pair checks out.
	out = op.mustRun("agent", "operate", "did:plc:alice")
	if !strings.Contains(out, "did:plc:authority operates did:plc:alice") || !strings.Contains(out, "Verified") {
		t.Fatalf("operate: %s", out)
	}
	if len(op.signInsFor) != 1 || !slices.Equal(op.signInsFor[0], coord.OperateScopes) || op.ended != 1 {
		t.Fatalf("operator's one-time sign-in: asked %v, ended %d", op.signInsFor, op.ended)
	}
	// Running it again doesn't add a second record.
	var again agentOut
	if err := json.Unmarshal([]byte(op.mustRun("agent", "operate", "did:plc:alice", "--json")), &again); err != nil {
		t.Fatal(err)
	}
	if again.Created == nil || *again.Created || !again.Verified {
		t.Fatalf("operate again: %+v", again)
	}

	var v agentOut
	if err := json.Unmarshal([]byte(alice.mustRun("agent", "verify", "did:plc:alice", "--json")), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Verified || v.Operator != "did:plc:authority" || v.Agent != "did:plc:alice" {
		t.Fatalf("verify: %+v", v)
	}
}

func TestDeclareRefusesANarrowerGrant(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	alice := w.agent(t)
	alice.mustRun("login", "--handle", "alice.test")
	alice.granted = []string{"atproto"}
	code, _, errOut := alice.run("agent", "declare", "--operator", "did:plc:authority")
	if code != 1 || !strings.Contains(errOut, "didn't grant") {
		t.Fatalf("declare without the grant: %d %s", code, errOut)
	}
	if alice.ended != 1 {
		t.Fatal("the one-time sign-in wasn't ended")
	}
	if code, _, _ := alice.run("agent", "verify", "did:plc:alice"); code != 1 {
		t.Fatal("a declaration was written anyway")
	}
}

func TestDeclareWithAPasswordSignIn(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	alice := w.agent(t)
	alice.mustRun("login", "--handle", "alice.test", "--password")
	alice.mustRun("agent", "declare", "--operator", "did:plc:authority")
	if len(alice.signInsFor) != 0 {
		t.Fatalf("signed in again despite a password sign-in: %v", alice.signInsFor)
	}
}

func TestDeclareAsAnotherAccount(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a := w.agent(t)
	// No settings at all: --as names the account.
	if code, _, errOut := a.run("agent", "declare", "--operator", "did:plc:authority"); code != 2 || !strings.Contains(errOut, "--as") {
		t.Fatalf("declare with no account: %d %s", code, errOut)
	}
	a.mustRun("agent", "declare", "--operator", "did:plc:authority", "--as", "bob.test")
	if len(a.signInHandles) != 1 || a.signInHandles[0] != "bob.test" {
		t.Fatalf("signed in as %v", a.signInHandles)
	}
	if code, _, errOut := a.run("agent", "declare"); code != 2 || !strings.Contains(errOut, "--operator") {
		t.Fatalf("declare without --operator: %d %s", code, errOut)
	}
	if code, _, _ := a.run("agent", "frobnicate"); code != 2 {
		t.Fatal("unknown agent subcommand accepted")
	}
}
