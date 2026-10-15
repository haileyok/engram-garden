package coord

import (
	"context"
	"strings"
	"testing"

	"github.com/haileyok/cocoon/oauth/scopes"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

func TestVerifyOperator(t *testing.T) {
	t.Parallel()
	n := spacetest.NewSpaceOfType(t, SpaceType, "operator", "proj")
	ctx := context.Background()
	op := n.Authority
	agent := n.NewAccount("did:plc:researcher")
	stranger := n.NewAccount("did:plc:stranger")

	// No declaration yet.
	if _, err := VerifyOperator(ctx, n.Dir, nil, agent.DID); err == nil {
		t.Fatal("verified an account with no declaration")
	}
	if err := Declare(ctx, n.Session(agent), op.DID, "researcher"); err != nil {
		t.Fatal(err)
	}
	// One direction only.
	if _, err := VerifyOperator(ctx, n.Dir, nil, agent.DID); err == nil {
		t.Fatal("verified a declaration the operator doesn't claim back")
	}
	// The operator claims a different agent.
	if _, err := Operate(ctx, n.Session(op), stranger.DID); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyOperator(ctx, n.Dir, nil, agent.DID); err == nil {
		t.Fatal("verified with the wrong operated record")
	}
	if created, err := Operate(ctx, n.Session(op), agent.DID); err != nil || !created {
		t.Fatalf("operate: created=%v err=%v", created, err)
	}
	// Claiming it again adds nothing.
	if created, err := Operate(ctx, n.Session(op), agent.DID); err != nil || created {
		t.Fatalf("operate again: created=%v err=%v", created, err)
	}
	got, err := VerifyOperator(ctx, n.Dir, nil, agent.DID)
	if err != nil {
		t.Fatal(err)
	}
	if got != op.DID {
		t.Fatalf("operator %s", got)
	}
	// Claiming someone as operator doesn't work without their record.
	if err := Declare(ctx, n.Session(stranger), agent.DID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyOperator(ctx, n.Dir, nil, stranger.DID); err == nil {
		t.Fatal("verified an operator who never claimed the agent")
	}
}

func TestVerifyChain(t *testing.T) {
	t.Parallel()
	n := spacetest.NewSpaceOfType(t, SpaceType, "operator", "proj")
	ctx := context.Background()
	op := n.Authority // a person: no declaration
	a := n.NewAccount("did:plc:agenta")
	c := n.NewAccount("did:plc:childc")
	g := n.NewAccount("did:plc:grandg")
	pair := func(agent, operator *spacetest.Account) {
		t.Helper()
		if err := Declare(ctx, n.Session(agent), operator.DID, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := Operate(ctx, n.Session(operator), agent.DID); err != nil {
			t.Fatal(err)
		}
	}
	pair(a, op)
	pair(c, a)
	pair(g, c)
	const opKey = "did:key:zQ3shOperatorsOfflineKey"
	keys := map[string][]string{
		a.DID: {opKey, "did:key:zPDS"},
		c.DID: {opKey, "did:key:zPDS"},
		g.DID: {opKey, "did:key:zPDS"},
	}
	rotation := func(_ context.Context, did string) ([]string, error) { return keys[did], nil }

	ch, err := VerifyChain(ctx, n.Dir, nil, rotation, g.DID, 8)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ch.Agents, ",") != g.DID+","+c.DID+","+a.DID || ch.Operator != op.DID || ch.RecoveryKey != opKey {
		t.Fatalf("chain %+v", ch)
	}
	// A person's own chain is just themselves: not an agent.
	if _, err := VerifyChain(ctx, n.Dir, nil, rotation, op.DID, 8); err == nil {
		t.Fatal("verified a person as an agent")
	}
	// Too deep.
	if _, err := VerifyChain(ctx, n.Dir, nil, rotation, g.DID, 2); err == nil || !strings.Contains(err.Error(), "deep") {
		t.Fatalf("depth limit: %v", err)
	}
	// An agent created without the operator's key first breaks the chain.
	keys[c.DID] = []string{"did:key:zPDS"}
	if _, err := VerifyChain(ctx, n.Dir, nil, rotation, g.DID, 8); err == nil || !strings.Contains(err.Error(), c.DID) {
		t.Fatalf("missing recovery key: %v", err)
	}
	keys[c.DID] = []string{opKey}
	// A broken link anywhere breaks the chain: g declares c, but c no
	// longer claims it.
	h := n.NewAccount("did:plc:orphanh")
	keys[h.DID] = []string{opKey}
	if err := Declare(ctx, n.Session(h), c.DID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyChain(ctx, n.Dir, nil, rotation, h.DID, 8); err == nil {
		t.Fatal("verified an agent its operator doesn't claim")
	}
	// A cycle.
	x, y := n.NewAccount("did:plc:cyclex"), n.NewAccount("did:plc:cycley")
	keys[x.DID], keys[y.DID] = []string{opKey}, []string{opKey}
	pair(x, y)
	pair(y, x)
	if _, err := VerifyChain(ctx, n.Dir, nil, rotation, x.DID, 8); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle: %v", err)
	}
}

func TestPublicRecordScopes(t *testing.T) {
	t.Parallel()
	if !GrantsRepoWrite(DeclareScopes, DeclarationCollection, "create", "update") {
		t.Error("DeclareScopes can't write a declaration")
	}
	if GrantsRepoWrite(DeclareScopes, OperatedCollection, "create") || GrantsRepoWrite(DeclareScopes, "app.bsky.feed.post", "create") {
		t.Error("DeclareScopes reach other collections")
	}
	if !GrantsRepoWrite(OperateScopes, OperatedCollection, "create") || GrantsRepoWrite(OperateScopes, OperatedCollection, "delete") {
		t.Error("OperateScopes should create operated records and nothing else")
	}
	if GrantsRepoWrite([]string{"atproto"}, DeclarationCollection, "create") {
		t.Error("the bare atproto scope can't write records")
	}
	// What a server grants may combine collections in one scope.
	if !GrantsRepoWrite([]string{"atproto", "repo?collection=" + DeclarationCollection + "&collection=app.bsky.feed.post&action=create&action=update"}, DeclarationCollection, "create", "update") {
		t.Error("a combined repo scope wasn't understood")
	}
}

func TestAgentScopes(t *testing.T) {
	t.Parallel()
	const auth = "did:plc:operator"
	ss := AgentScopes(auth)
	allowed := func(coll, action string) bool {
		for _, s := range ss {
			p := scopes.ParseSpacePermission(s)
			if p != nil && p.Matches(scopes.SpaceMatch{Type: SpaceType, Authority: auth, Skey: "proj", Collection: coll, Action: action}) {
				return true
			}
		}
		return false
	}
	for _, c := range WriteOnceCollections {
		if !allowed(c, "read") || !allowed(c, "create") {
			t.Errorf("%s: can't read and create", c)
		}
		if allowed(c, "update") || allowed(c, "delete") {
			t.Errorf("%s: can be edited or deleted", c)
		}
	}
	if !allowed(PresenceCollection, "create") || !allowed(PresenceCollection, "update") || !allowed(PresenceCollection, "read") {
		t.Error("presence can't be kept up to date")
	}
	if allowed(PresenceCollection, "delete") {
		t.Error("presence can be deleted")
	}
	// Another operator's workspaces are out of reach.
	p := scopes.ParseSpacePermission(ss[1])
	if p.Matches(scopes.SpaceMatch{Type: SpaceType, Authority: "did:plc:other", Skey: "proj", Collection: ObservationCollection, Action: "read"}) {
		t.Error("scope reaches another authority's workspace")
	}
}
