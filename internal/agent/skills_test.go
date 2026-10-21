package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

func skillAgent(t *testing.T, n *spacetest.Net, did string) *Agent {
	t.Helper()
	acct := n.AccountByDID(did)
	c, err := spaceclient.New(n.Session(acct), n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Agent{Client: c, Space: n.Space}
}

func skillNet(t *testing.T) *spacetest.Net {
	t.Helper()
	n := spacetest.New(t)
	for _, did := range []string{"did:plc:alice", "did:plc:bob"} {
		n.NewAccount(did)
		n.AddMember(did)
	}
	n.AddMember(n.Authority.DID)
	return n
}

func TestSaveAndListSkills(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := skillNet(t)
	alice := skillAgent(t, n, "did:plc:alice")
	bob := skillAgent(t, n, "did:plc:bob")

	out, err := alice.SaveSkill(ctx, SaveSkillIn{
		Name: "code-review", Description: "Review a change. Use after finishing work.", Instructions: "# Review\n\nAsk two models.",
		Files: []lex.SkillFile{{Path: "scripts/review.sh", Content: "#!/bin/sh\n", Executable: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Created || !strings.HasSuffix(out.URI, "/did:plc:alice/garden.engram.skill/code-review") {
		t.Fatalf("save: %+v", out)
	}

	// Bob, another member, reads it from Alice's repo.
	skills, _, err := bob.Skills(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "code-review" || skills[0].Author != "did:plc:alice" ||
		len(skills[0].Files) != 1 || !skills[0].Files[0].Executable {
		t.Fatalf("bob sees %+v", skills)
	}

	// Updating keeps createdAt and the files when they're left out.
	time.Sleep(2 * time.Millisecond)
	out, err = alice.SaveSkill(ctx, SaveSkillIn{Name: "code-review", Description: "Review a change.", Instructions: "# Review\n\nAsk three models.", Note: "two wasn't enough"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Created {
		t.Fatal("second save says created")
	}
	skills, _, _ = bob.Skills(ctx)
	s := skills[0]
	if !strings.Contains(s.Instructions, "three models") || len(s.Files) != 1 || s.Note != "two wasn't enough" || s.CreatedAt == s.UpdatedAt {
		t.Fatalf("after update: %+v", s)
	}
}

func TestSkillVersionsResolve(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := skillNet(t)
	alice := skillAgent(t, n, "did:plc:alice")
	bob := skillAgent(t, n, "did:plc:bob")
	boss := skillAgent(t, n, n.Authority.DID)
	save := func(a *Agent, instr string) {
		t.Helper()
		if _, err := a.SaveSkill(ctx, SaveSkillIn{Name: "deploy", Description: "Deploy it.", Instructions: instr}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	save(boss, "authority's way")
	save(bob, "bob's way") // newer than the authority's
	save(alice, "alice's way")

	get := func(a *Agent) SkillInfo {
		t.Helper()
		skills, _, err := a.Skills(ctx)
		if err != nil || len(skills) != 1 {
			t.Fatalf("skills %+v, %v", skills, err)
		}
		return skills[0]
	}
	if s := get(alice); !strings.Contains(s.Instructions, "alice's") || len(s.Shadowed) != 2 {
		t.Fatalf("alice gets %+v", s)
	}
	if s := get(bob); !strings.Contains(s.Instructions, "bob's") {
		t.Fatalf("bob gets %q", s.Instructions)
	}
	// Someone with no version of their own gets the authority's, though
	// Alice's and Bob's are newer.
	carol := n.NewAccount("did:plc:carol")
	n.AddMember(carol.DID)
	if s := get(skillAgent(t, n, carol.DID)); !strings.Contains(s.Instructions, "authority's") {
		t.Fatalf("carol gets %q", s.Instructions)
	}
}

func TestDeleteSkillOnlyYourOwn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := skillNet(t)
	alice := skillAgent(t, n, "did:plc:alice")
	bob := skillAgent(t, n, "did:plc:bob")
	if _, err := alice.SaveSkill(ctx, SaveSkillIn{Name: "x", Description: "d", Instructions: "i"}); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.DeleteSkill(ctx, "x"); err == nil || !strings.Contains(err.Error(), "only delete your own") {
		t.Fatalf("bob deleting alice's: %v", err)
	}
	if _, err := alice.DeleteSkill(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if skills, _, _ := bob.Skills(ctx); len(skills) != 0 {
		t.Fatalf("still there: %+v", skills)
	}
}

func TestSkillsIgnoreInvalidRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := skillNet(t)
	bob := n.AccountByDID("did:plc:bob")
	// A record whose key isn't its name, and one with no description.
	n.Put(bob, lex.SkillCollection, "other", map[string]any{"$type": lex.SkillCollection, "name": "real", "description": "d", "instructions": "i", "createdAt": "x", "updatedAt": "x"})
	n.Put(bob, lex.SkillCollection, "bare", map[string]any{"$type": lex.SkillCollection, "name": "bare", "instructions": "i", "createdAt": "x", "updatedAt": "x"})
	skills, _, err := skillAgent(t, n, "did:plc:alice").Skills(ctx)
	if err != nil || len(skills) != 0 {
		t.Fatalf("skills %+v, %v", skills, err)
	}
}

func TestSaveSkillRejectsBadInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	alice := skillAgent(t, skillNet(t), "did:plc:alice")
	for _, in := range []SaveSkillIn{
		{Name: "Bad", Description: "d", Instructions: "i"},
		{Name: "ok", Description: "", Instructions: "i"},
		{Name: "ok", Description: "d", Instructions: "i", Files: []lex.SkillFile{{Path: "../escape", Content: "x"}}},
	} {
		if _, err := alice.SaveSkill(ctx, in); err == nil {
			t.Errorf("%+v saved", in)
		}
	}
}

func TestSpacesSkillsAcrossSpaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	one, two := spacetest.NewSpace(t, "auth1", "global"), spacetest.NewSpace(t, "auth2", "attie")
	var agents []*Agent
	for _, n := range []*spacetest.Net{one, two} {
		n.NewAccount("did:plc:alice")
		n.AddMember("did:plc:alice")
		agents = append(agents, skillAgent(t, n, "did:plc:alice"))
	}
	s := SpacesOf(agents...)
	if _, err := s.SaveSkill(ctx, SaveSkillIn{Name: "review", Description: "d", Instructions: "global review"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSkill(ctx, SaveSkillIn{Name: "deploy", Description: "d", Instructions: "attie deploy", Space: "attie"}); err != nil {
		t.Fatal(err)
	}
	// An update without a space goes where the skill is.
	if out, err := s.SaveSkill(ctx, SaveSkillIn{Name: "deploy", Description: "d", Instructions: "attie deploy v2"}); err != nil || out.Space != "attie" {
		t.Fatalf("update went to %q: %v", out.Space, err)
	}
	out, err := s.ListSkills(ctx, ListSkillsIn{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, sk := range out.Skills {
		got[sk.Name] = sk.Space
		if sk.Instructions != "" {
			t.Fatal("instructions without content")
		}
	}
	if len(out.Skills) != 2 || got["review"] != "global" || got["deploy"] != "attie" {
		t.Fatalf("skills %+v", out.Skills)
	}
	full, err := s.GetSkill(ctx, GetSkillIn{Name: "deploy"})
	if err != nil || !strings.Contains(full.Skill.Instructions, "v2") {
		t.Fatalf("get %+v, %v", full, err)
	}
	if _, err := s.DeleteSkill(ctx, DeleteSkillIn{Name: "deploy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSkill(ctx, GetSkillIn{Name: "deploy"}); err == nil {
		t.Fatal("deleted skill still found")
	}
}

// TestSkillsSayWhoseCouldntBeRead: a member whose PDS can't be reached
// doesn't hide the others' skills, and the listing says it's partial.
func TestSkillsSayWhoseCouldntBeRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := skillNet(t)
	alice := skillAgent(t, n, "did:plc:alice")
	if _, err := alice.SaveSkill(ctx, SaveSkillIn{Name: "x", Description: "d", Instructions: "i"}); err != nil {
		t.Fatal(err)
	}
	bob := n.AccountByDID("did:plc:bob")
	n.Put(bob, lex.SkillCollection, "y", (&lex.Skill{Name: "y", Description: "d", Instructions: "i", CreatedAt: "x", UpdatedAt: "x"}).Record())
	// Bob's PDS moves somewhere that refuses connections.
	pub, _ := bob.Key.PublicKey()
	n.Dir.Insert(identity.Identity{
		DID: syntax.DID(bob.DID), Handle: syntax.HandleInvalid,
		Keys:     map[string]identity.VerificationMethod{"atproto": {Type: "Multikey", PublicKeyMultibase: pub.Multibase()}},
		Services: map[string]identity.ServiceEndpoint{"atproto_pds": {Type: "AtprotoPersonalDataServer", URL: "http://127.0.0.1:1"}},
	})
	s := SpacesOf(alice)
	out, err := s.ListSkills(ctx, ListSkillsIn{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Skills) != 1 || out.Skills[0].Name != "x" || !strings.Contains(out.Note, "couldn't read did:plc:bob's skills") {
		t.Fatalf("list %+v", out)
	}
}

func TestResolveSkillsByInstant(t *testing.T) {
	t.Parallel()
	v := func(uri, updated string) SkillInfo {
		return SkillInfo{Skill: lex.Skill{Name: "x", UpdatedAt: updated}, URI: uri, Author: "did:plc:" + uri}
	}
	// 23:00+02:00 is 21:00Z, older than 22:00Z though it sorts after it as text.
	got := resolveSkills([]SkillInfo{v("a", "2026-01-21T23:00:00+02:00"), v("b", "2026-01-21T22:00:00.000Z")}, "did:plc:me", "did:plc:boss")
	if got[0].URI != "b" {
		t.Fatalf("offset time won: %+v", got[0])
	}
	// A time far in the future doesn't win.
	got = resolveSkills([]SkillInfo{v("a", "9999-01-01T00:00:00Z"), v("b", "2026-01-21T22:00:00.000Z")}, "did:plc:me", "did:plc:boss")
	if got[0].URI != "b" {
		t.Fatalf("future time won: %+v", got[0])
	}
}
