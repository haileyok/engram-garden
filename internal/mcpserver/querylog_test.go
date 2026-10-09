package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/haileyok/engram-garden/internal/agent"
)

func TestQueryLog(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "queries.jsonl")
	l := NewQueryLog(p, nil)
	calls := 0
	recall := l.logged(func(_ context.Context, in agent.RecallIn) (agent.MemoriesOut, error) {
		calls++
		if in.Query == "fail" {
			return agent.MemoriesOut{}, errors.New("appview down")
		}
		return agent.MemoriesOut{Memories: []agent.Memory{{URI: "at://a/1"}, {URI: "at://a/2"}}}, nil
	})
	ctx := context.Background()
	if _, err := recall(ctx, agent.RecallIn{Query: "engram_space_uri", Space: "coding-agents"}); err != nil {
		t.Fatal(err)
	}
	if _, err := recall(ctx, agent.RecallIn{Query: "fail"}); err == nil {
		t.Fatal("error swallowed")
	}
	if _, err := recall(ctx, agent.RecallIn{Query: "second"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if calls != 3 || len(lines) != 2 {
		t.Fatalf("%d calls, %d lines: %q", calls, len(lines), b)
	}
	var e QueryLogEntry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatal(err)
	}
	if e.Query != "engram_space_uri" || e.Space != "coding-agents" || len(e.Results) != 2 || e.Time.IsZero() {
		t.Errorf("entry = %+v", e)
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("log file mode %v, err %v", st.Mode().Perm(), err)
	}
	// Without a log, recall is unchanged.
	var none *QueryLog
	if _, err := none.logged(func(context.Context, agent.RecallIn) (agent.MemoriesOut, error) {
		return agent.MemoriesOut{}, nil
	})(ctx, agent.RecallIn{Query: "x"}); err != nil {
		t.Fatal(err)
	}
	// An unwritable log doesn't break recall.
	bad := NewQueryLog(filepath.Join(t.TempDir(), "missing", "dir", "q.jsonl"), nil)
	if _, err := bad.logged(func(context.Context, agent.RecallIn) (agent.MemoriesOut, error) {
		return agent.MemoriesOut{}, nil
	})(ctx, agent.RecallIn{Query: "x"}); err != nil {
		t.Fatal(err)
	}
}
