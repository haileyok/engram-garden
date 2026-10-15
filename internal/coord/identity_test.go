package coord

import (
	"context"
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
