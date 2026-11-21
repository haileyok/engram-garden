package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// session builds transcript lines in pi's format.
type session struct {
	b strings.Builder
	n int
}

func (s *session) add(typ string, fields map[string]any) string {
	s.n++
	id := fmt.Sprintf("e%03d", s.n)
	m := map[string]any{"type": typ, "id": id, "timestamp": fmt.Sprintf("2026-11-21T10:00:%02d.000Z", s.n%60)}
	for k, v := range fields {
		m[k] = v
	}
	raw, _ := json.Marshal(m)
	s.b.Write(raw)
	s.b.WriteByte('\n')
	return id
}

func (s *session) msg(role string, content any, extra ...map[string]any) string {
	m := map[string]any{"role": role, "content": content}
	for _, e := range extra {
		for k, v := range e {
			m[k] = v
		}
	}
	return s.add("message", map[string]any{"message": m})
}

func (s *session) user(text string) string {
	return s.msg("user", []any{map[string]any{"type": "text", "text": text}})
}

func (s *session) say(text string, calls ...map[string]any) string {
	var content []any
	content = append(content, map[string]any{"type": "thinking", "thinking": "secret musings nobody searches"})
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, c := range calls {
		content = append(content, map[string]any{"type": "toolCall", "id": c["id"], "name": c["name"], "arguments": c["args"]})
	}
	return s.msg("assistant", content)
}

func (s *session) result(callID, tool, out string, isErr bool) string {
	return s.msg("toolResult", []any{map[string]any{"type": "text", "text": out}}, map[string]any{"toolCallId": callID, "toolName": tool, "isError": isErr})
}

func call(id, name string, args map[string]any) map[string]any {
	return map[string]any{"id": id, "name": name, "args": args}
}

func byKey(ps []Passage) map[string]Passage {
	m := map[string]Passage{}
	for _, p := range ps {
		m[p.Key()] = p
	}
	return m
}

func TestPassages(t *testing.T) {
	t.Parallel()
	var s session
	s.add("session", map[string]any{"cwd": "/root/x"})
	s.msg("system", "You are a coding assistant")
	prompt := s.user("why does the indexer drop memories when the model changes?")
	s.add("custom_message", map[string]any{"customType": "engram-recall", "content": "<memories>an old memory</memories>"})
	step := s.say("Let me look at the indexer.",
		call("c1", "bash", map[string]any{"command": "cd /root/eg && go test ./internal/indexer/ -run TestModel"}),
		call("c2", "read", map[string]any{"path": "internal/indexer/indexer.go"}),
		call("c3", "recall", map[string]any{"query": "indexer model change"}),
	)
	fail := s.result("c1", "bash", "--- FAIL: TestModelChange\n    indexer_test.go:88: dropped 3 memories\nFAIL", true)
	s.result("c2", "read", "package indexer\n// lots of code", false)
	s.result("c3", "recall", "memories...", false)
	s.say("The test fails because SetConfig marks a resync before the vectors exist; I'll hold the position.",
		call("c4", "edit", map[string]any{"path": "internal/indexer/indexer.go", "oldText": "a", "newText": "b"}))
	s.result("c4", "edit", "ok", false)
	s.say("Fixed: the position no longer advances until the new model's vectors are indexed.")
	compact := s.add("compaction", map[string]any{"summary": "## Goal\nFix memory drops on model change."})
	next := s.user("now push it")

	var st State
	ps := st.Feed(s.b.String())
	got := byKey(ps)

	turn, ok := got["turn:"+prompt]
	if !ok || !strings.Contains(turn.Text, "Asked: why does the indexer drop memories") || !strings.Contains(turn.Text, "Answered: Fixed: the position no longer advances") {
		t.Fatalf("turn: %+v", turn)
	}
	st1 := got["step:"+step]
	if !strings.Contains(st1.Text, "Let me look at the indexer.") || !strings.Contains(st1.Text, "$ cd /root/eg && go test ./internal/indexer/ -run TestModel") || !strings.Contains(st1.Text, "read internal/indexer/indexer.go") {
		t.Fatalf("step: %q", st1.Text)
	}
	if strings.Contains(st1.Text, "recall") {
		t.Fatalf("Engram's own calls are in a step: %q", st1.Text)
	}
	if strings.Join(st1.Files, ",") != "internal/indexer/indexer.go" || strings.Join(st1.Commands, ",") != "go test" {
		t.Fatalf("facets: %v %v", st1.Files, st1.Commands)
	}
	er := got["error:"+fail]
	if !strings.Contains(er.Text, "Failed: $ cd /root/eg && go test") || !strings.Contains(er.Text, "dropped 3 memories") || !strings.Contains(er.Text, "Then: The test fails because SetConfig") {
		t.Fatalf("error: %q", er.Text)
	}
	if sum := got["summary:"+compact]; !strings.Contains(sum.Text, "Fix memory drops") {
		t.Fatalf("summary: %+v", sum)
	}
	for _, p := range ps {
		for _, never := range []string{"secret musings", "an old memory", "coding assistant", "lots of code"} {
			if strings.Contains(p.Text, never) {
				t.Fatalf("%s holds %q: %q", p.Key(), never, p.Text)
			}
		}
		if p.Time.IsZero() || p.Offset < 0 {
			t.Fatalf("%s: no time or offset", p.Key())
		}
	}
	// The offset points at the anchor's line.
	lines := strings.SplitAfter(s.b.String(), "\n")
	var off int
	for _, l := range lines {
		if strings.Contains(l, `"id":"`+fail+`"`) {
			break
		}
		off += len(l)
	}
	if er.Offset != int64(off) {
		t.Fatalf("error offset %d, want %d", er.Offset, off)
	}
	// The new prompt opens a turn that isn't a passage until it's answered.
	if _, ok := got["turn:"+next]; ok || st.Turn == nil || st.Turn.EntryID != next {
		t.Fatalf("open turn: %+v", st.Turn)
	}
	if st.Offset != int64(s.b.Len()) {
		t.Fatalf("offset %d, want %d", st.Offset, s.b.Len())
	}
}

// Reading a transcript in pieces, with the state saved and restored
// between them, gives what reading it whole does.
func TestFeedInPiecesMatchesWhole(t *testing.T) {
	t.Parallel()
	var s session
	for i := range 5 {
		s.user(fmt.Sprintf("task %d", i))
		s.say(fmt.Sprintf("working on %d", i), call(fmt.Sprintf("c%d", i), "bash", map[string]any{"command": fmt.Sprintf("make step%d", i)}))
		s.result(fmt.Sprintf("c%d", i), "bash", "exit 2", i%2 == 0)
		s.say(fmt.Sprintf("done with %d", i))
	}
	all := s.b.String()
	var whole State
	want := byKey(append(whole.Feed(all), whole.Finish()...))

	lines := strings.SplitAfter(all, "\n")
	for cut := 1; cut < len(lines)-1; cut += 3 {
		var st State
		got := map[string]Passage{}
		for k, v := range byKey(st.Feed(strings.Join(lines[:cut], ""))) {
			got[k] = v
		}
		raw, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		var back State
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		for k, v := range byKey(append(back.Feed(strings.Join(lines[cut:], "")), back.Finish()...)) {
			got[k] = v
		}
		if len(got) != len(want) {
			t.Fatalf("cut at %d: %d passages, want %d", cut, len(got), len(want))
		}
		for k, w := range want {
			g := got[k]
			if g.Text != w.Text || g.Offset != w.Offset || !g.Time.Equal(w.Time) {
				t.Fatalf("cut at %d: %s differs:\n%+v\n%+v", cut, k, g, w)
			}
		}
	}
}

func TestPassagesAreRedacted(t *testing.T) {
	t.Parallel()
	var s session
	s.user("deploy with ENGRAM_PASSWORD=correct-horse-battery-staple")
	s.say("", call("c1", "bash", map[string]any{"command": "curl -H 'Authorization: Bearer abcdefghijklmnop1234567890' https://x"}))
	s.result("c1", "bash", "401 for token ghp_aB3dE5fG7hI9jK1lM3nO5pQ7rS9tU1vW3xY5", true)
	s.say("The token is wrong.")
	var st State
	for _, p := range st.Feed(s.b.String()) {
		for _, secret := range []string{"correct-horse", "abcdefghijklmnop1234567890", "ghp_aB3dE5"} {
			if strings.Contains(p.Text, secret) {
				t.Fatalf("%s holds a secret: %q", p.Key(), p.Text)
			}
		}
	}
}

func TestErrorWithoutAFixWaitsForTheEnd(t *testing.T) {
	t.Parallel()
	var s session
	s.user("run it")
	s.say("", call("c1", "bash", map[string]any{"command": "./run"}))
	fail := s.result("c1", "bash", "segfault", true)
	var st State
	if ps := st.Feed(s.b.String()); len(ps) != 1 || ps[0].Kind != KindStep {
		t.Fatalf("before the end: %+v", ps)
	}
	ps := st.Finish()
	if len(ps) != 1 || ps[0].Key() != "error:"+fail || strings.Contains(ps[0].Text, "Then:") {
		t.Fatalf("at the end: %+v", ps)
	}
}

func TestFacets(t *testing.T) {
	t.Parallel()
	_, cmds := facetsOf("$ cd /root/x && GOFLAGS=-mod=mod go test ./... | tail -5; git status")
	if strings.Join(cmds, ",") != "go test,tail,git status" {
		t.Fatalf("commands %v", cmds)
	}
	files, _ := facetsOf("edit internal/agent/archive.go")
	if strings.Join(files, ",") != "internal/agent/archive.go" {
		t.Fatalf("files %v", files)
	}
}

// What State keeps between pieces is stored with the index's positions, so
// it's redacted too.
func TestStateHoldsNoSecrets(t *testing.T) {
	t.Parallel()
	var s session
	s.user("deploy with ENGRAM_PASSWORD=correct-horse-battery-staple please")
	s.say("", call("c1", "bash", map[string]any{"command": "curl -H 'Authorization: Bearer abcdefghijklmnop1234567890' https://x"}))
	s.result("c1", "bash", "denied for ghp_aB3dE5fG7hI9jK1lM3nO5pQ7rS9tU1vW3xY5", true)
	s.say("", call("c2", "bash", map[string]any{"command": "export TOKEN=s3cr3t-t0ken-value"}))
	var st State
	st.Feed(s.b.String())
	raw, _ := json.Marshal(st)
	for _, secret := range []string{"correct-horse", "abcdefghijklmnop1234567890", "ghp_aB3dE5", "s3cr3t-t0ken"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("state holds %q: %s", secret, raw)
		}
	}
}

// Clipping can't cut a secret's closing quote off before redaction sees it.
func TestRedactedBeforeClipped(t *testing.T) {
	t.Parallel()
	var s session
	summary := strings.Repeat("words ", (maxSummary-30)/6) + `password="` + strings.Repeat("hunter", 20) + `"`
	s.add("compaction", map[string]any{"summary": summary})
	var st State
	for _, p := range st.Feed(s.b.String()) {
		if strings.Contains(p.Text, "hunterhunter") {
			t.Fatalf("a clipped secret survived: …%s", p.Text[len(p.Text)-80:])
		}
	}
}
