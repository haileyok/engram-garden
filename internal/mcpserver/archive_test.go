package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

// TestPushSessionTakesWhatPiSends: push_session's input schema accepts the
// record pi sends (no updatedAt, kind or parent: the archive fills those in).
func TestPushSessionTakesWhatPiSends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := spacetest.NewTyped(t, "principal", lex.ArchiveType, "sessions")
	alice := n.NewAccount("did:plc:alice")
	n.AddMember(alice.DID)
	c, err := spaceclient.New(n.Session(alice), n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ar := &agent.Archive{Client: c, Space: n.Space, Identity: agent.Identity{DID: alice.DID, Kind: "session"}}
	ct, st := mcp.NewInMemoryTransports()
	if _, err := NewServer(agent.SpacesOf(&agent.Agent{Client: c, Space: n.Space}), WithArchive(ar)).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, tl := range tools.Tools {
		have[tl.Name] = true
	}
	for _, name := range []string{"push_session", "list_sessions", "pull_session", "delete_session", "search_sessions", "read_session"} {
		if !have[name] {
			t.Errorf("no %s tool", name)
		}
	}

	var out agent.PushSessionOut
	if msg := call(t, cs, "push_session", map[string]any{
		"session": map[string]any{
			"sessionId": "s1", "title": "a title", "summary": "", "status": "active",
			"cwd": "/root/x", "machine": "box", "harness": "pi", "startedAt": "2026-11-21T00:00:00.000Z",
		},
		"fromBytes": 0,
		"data":      `{"type":"session","id":"s1"}` + "\n",
	}, &out); msg != "" {
		t.Fatalf("push_session refused pi's record: %s", msg)
	}
	if out.Bytes == 0 || !strings.HasSuffix(out.URI, "/garden.engram.session/s1") {
		t.Fatalf("push: %+v", out)
	}
}
