// Package transcript cuts an agent session's transcript (pi's JSONL session
// format) into passages worth searching, and leaves out the rest. It reads
// the transcript as it grows: State carries what it needs between pieces, so
// each line is read once.
//
// Passages (see docs/design/sessions.md):
//
//   - turn: the person's prompt and the final reply to it;
//   - step: the assistant's text before tool calls, and a line per call;
//   - error: a failed tool call, the start and end of its output, and the
//     assistant's next text (usually the fix);
//   - summary: a compaction summary.
//
// Thinking, successful tool output, injected messages (custom_message: the
// memories and lounge messages a harness adds), images and Engram's own
// tool calls are left out. Every passage's text is redacted.
//
// Any change to what it returns for the same input changes the index: bump
// Version.
package transcript

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/haileyok/engram-garden/internal/redact"
)

// Version is the passage format. The index of a session cut by another
// version is rebuilt.
const Version = 1

// Passage kinds.
const (
	KindTurn    = "turn"
	KindStep    = "step"
	KindError   = "error"
	KindSummary = "summary"
)

// Limits on a passage's parts, in bytes.
const (
	maxPrompt   = 4000
	maxReply    = 4000
	maxNarrate  = 3000
	maxCallLine = 300
	maxCalls    = 30
	maxErrHead  = 500
	maxErrTail  = 500
	maxFix      = 1500
	maxSummary  = 20000
	maxFacets   = 12
)

// Passage is one searchable piece of a transcript.
type Passage struct {
	Kind string
	// EntryID is the entry it's anchored at (the prompt, the assistant
	// message, the failed result, the compaction), and Offset that entry's
	// line's byte offset in the transcript.
	EntryID string
	Offset  int64
	Time    time.Time
	Text    string
	// Files are paths the passage's tool calls read or changed, and
	// Commands the programs its shell calls ran.
	Files    []string
	Commands []string
}

// Key is the passage's name within its session: its kind and entry.
func (p Passage) Key() string { return p.Kind + ":" + p.EntryID }

// State is how far a transcript has been read, and what's still open: the
// turn in progress, and failed calls waiting for what came after.
type State struct {
	Offset  int64           `json:"offset"`
	Turn    *Turn           `json:"turn,omitempty"`
	Calls   map[string]Call `json:"calls,omitempty"`
	Pending []Pending       `json:"pending,omitempty"`
}

// Turn is the turn in progress: its prompt, and the latest reply text.
type Turn struct {
	EntryID string    `json:"id"`
	Offset  int64     `json:"offset"`
	Time    time.Time `json:"time"`
	Prompt  string    `json:"prompt"`
	Reply   string    `json:"reply,omitempty"`
}

// Call is a tool call waiting for its result, as one line.
type Call struct {
	Line string `json:"line"`
}

// Pending is a failed call's passage, waiting for the assistant's next text.
type Pending struct {
	EntryID string    `json:"id"`
	Offset  int64     `json:"offset"`
	Time    time.Time `json:"time"`
	Text    string    `json:"text"`
	Files   []string  `json:"files,omitempty"`
	Cmds    []string  `json:"cmds,omitempty"`
}

type entry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Summary   string          `json:"summary"`
	Message   json.RawMessage `json:"message"`
}

type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	IsError    bool            `json:"isError"`
}

type part struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Feed reads complete transcript lines (data ends with a newline, or is
// empty), which start at st.Offset, and returns the passages they finish.
// A passage can come out again later with more in it (a turn whose reply
// grew): the later one replaces it.
func (st *State) Feed(data string) []Passage {
	var out []Passage
	for data != "" {
		nl := strings.IndexByte(data, '\n')
		if nl < 0 {
			break // not a complete line: the caller sends whole lines
		}
		line := data[:nl]
		off := st.Offset
		st.Offset += int64(nl + 1)
		data = data[nl+1:]
		out = append(out, st.line(line, off)...)
	}
	return dedupe(out)
}

// Finish closes what's still open, for a session that has ended.
func (st *State) Finish() []Passage {
	var out []Passage
	for _, p := range st.Pending {
		out = append(out, p.passage(""))
	}
	st.Pending = nil
	if t := st.Turn; t != nil && t.Reply != "" {
		out = append(out, t.passage())
	}
	st.Turn = nil
	st.Calls = nil
	return out
}

func (st *State) line(line string, off int64) []Passage {
	var e entry
	if json.Unmarshal([]byte(line), &e) != nil || e.ID == "" {
		return nil
	}
	at, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
	switch e.Type {
	case "compaction":
		if strings.TrimSpace(e.Summary) == "" {
			return nil
		}
		return []Passage{{Kind: KindSummary, EntryID: e.ID, Offset: off, Time: at, Text: clip(redact.Text(e.Summary), maxSummary)}}
	case "message":
	default:
		return nil
	}
	var m message
	if json.Unmarshal(e.Message, &m) != nil {
		return nil
	}
	switch m.Role {
	case "user":
		return st.user(e, m, off, at)
	case "assistant":
		return st.assistant(e, m, off, at)
	case "toolResult":
		return st.result(e, m, off, at)
	}
	return nil
}

func (st *State) user(e entry, m message, off int64, at time.Time) []Passage {
	text := strings.TrimSpace(texts(m.Content))
	if text == "" {
		return nil
	}
	var out []Passage
	if t := st.Turn; t != nil && t.Reply != "" {
		out = append(out, t.passage())
	}
	// Everything kept in State is redacted as it's read: State is stored too.
	st.Turn = &Turn{EntryID: e.ID, Offset: off, Time: at, Prompt: clip(redact.Text(text), maxPrompt)}
	return out
}

func (st *State) assistant(e entry, m message, off int64, at time.Time) []Passage {
	var parts []part
	if json.Unmarshal(m.Content, &parts) != nil {
		return nil
	}
	var text []string
	var calls []part
	for _, p := range parts {
		switch p.Type {
		case "text":
			if s := strings.TrimSpace(p.Text); s != "" {
				text = append(text, s)
			}
		case "toolCall":
			calls = append(calls, p)
		}
	}
	said := redact.Text(strings.Join(text, "\n"))
	var out []Passage
	// What the assistant said next is how its failed calls were taken.
	if said != "" && len(st.Pending) > 0 {
		for _, p := range st.Pending {
			out = append(out, p.passage(said))
		}
		st.Pending = nil
	}
	if len(calls) == 0 {
		// A reply: the turn's final answer, unless more follows.
		if said != "" && st.Turn != nil {
			st.Turn.Reply = clip(said, maxReply)
			out = append(out, st.Turn.passage())
		}
		return out
	}
	var lines, files, cmds []string
	shown := 0
	for _, c := range calls {
		l, f, cmd := callLine(c)
		if st.Calls == nil {
			st.Calls = map[string]Call{}
		}
		if c.ID != "" {
			st.Calls[c.ID] = Call{Line: l}
		}
		if l == "" || ownTool(c.Name) {
			continue
		}
		files = appendFacet(files, f...)
		cmds = appendFacet(cmds, cmd...)
		if shown < maxCalls {
			lines = append(lines, l)
		}
		shown++
	}
	if shown > maxCalls {
		lines = append(lines, fmt.Sprintf("… and %d more calls", shown-maxCalls))
	}
	if said == "" && len(lines) == 0 {
		return out
	}
	var b strings.Builder
	if said != "" {
		b.WriteString(clip(said, maxNarrate))
		if len(lines) > 0 {
			b.WriteString("\n\n")
		}
	}
	b.WriteString(strings.Join(lines, "\n"))
	// Redacted again as a whole: a secret can span the parts.
	return append(out, Passage{Kind: KindStep, EntryID: e.ID, Offset: off, Time: at, Text: redact.Text(b.String()), Files: files, Commands: cmds})
}

func (st *State) result(e entry, m message, off int64, at time.Time) []Passage {
	call, known := st.Calls[m.ToolCallID]
	delete(st.Calls, m.ToolCallID)
	if !m.IsError || ownTool(m.ToolName) {
		return nil
	}
	line := call.Line
	if !known || line == "" {
		line = m.ToolName
	}
	output := strings.TrimSpace(redact.Text(texts(m.Content)))
	var b strings.Builder
	b.WriteString("Failed: " + line)
	if output != "" {
		b.WriteString("\n" + headTail(output, maxErrHead, maxErrTail))
	}
	p := Pending{EntryID: e.ID, Offset: off, Time: at, Text: redact.Text(b.String())}
	if known {
		p.Files, p.Cmds = facetsOf(line)
	}
	st.Pending = append(st.Pending, p)
	return nil
}

func (t *Turn) passage() Passage {
	text := "Asked: " + t.Prompt
	if t.Reply != "" {
		text += "\n\nAnswered: " + t.Reply
	}
	return Passage{Kind: KindTurn, EntryID: t.EntryID, Offset: t.Offset, Time: t.Time, Text: redact.Text(text)}
}

func (p Pending) passage(fix string) Passage {
	text := p.Text
	if fix != "" {
		text += "\n\nThen: " + clip(fix, maxFix)
	}
	return Passage{Kind: KindError, EntryID: p.EntryID, Offset: p.Offset, Time: p.Time, Text: redact.Text(text), Files: p.Files, Commands: p.Cmds}
}

// ownTool reports Engram's own tools, whose calls echo memories and
// sessions back into the index.
func ownTool(name string) bool {
	switch name {
	case "remember", "recall", "forget", "get_memory", "list_memories", "search_sessions", "read_session":
		return true
	}
	return strings.HasPrefix(name, "engram_") || strings.HasPrefix(name, "mcp__engram")
}

// callLine is one line for a tool call, with the files and commands it
// names.
func callLine(c part) (string, []string, []string) {
	var args map[string]any
	_ = json.Unmarshal(c.Arguments, &args)
	str := func(k string) string { s, _ := args[k].(string); return s }
	var line string
	switch c.Name {
	case "bash":
		line = "$ " + oneLine(str("command"))
	case "read", "write", "ls":
		line = c.Name + " " + str("path")
	case "edit":
		line = "edit " + str("path")
	case "grep":
		line = "grep " + str("pattern") + " " + str("path")
	case "find":
		line = "find " + str("pattern") + " " + str("path")
	default:
		raw := oneLine(string(c.Arguments))
		if raw == "{}" || raw == "null" {
			raw = ""
		}
		line = strings.TrimSpace(c.Name + " " + raw)
	}
	line = clip(redact.Text(strings.TrimSpace(line)), maxCallLine)
	f, cmd := facetsOf(line)
	return line, f, cmd
}

// facetsOf reads a call line's file (read, write, edit) or the programs a
// shell line runs.
func facetsOf(line string) (files, cmds []string) {
	verb, rest, _ := strings.Cut(line, " ")
	switch verb {
	case "read", "write", "edit":
		if rest != "" {
			files = append(files, rest)
		}
	case "$":
		for seg := range strings.FieldsFuncSeq(rest, func(r rune) bool { return r == '|' || r == ';' || r == '&' }) {
			fields := strings.Fields(seg)
			for len(fields) > 0 && (strings.Contains(fields[0], "=") || fields[0] == "sudo" || fields[0] == "cd" || fields[0] == "(") {
				if fields[0] == "cd" && len(fields) > 1 {
					fields = fields[2:]
					continue
				}
				fields = fields[1:]
			}
			if len(fields) == 0 {
				continue
			}
			prog := fields[0]
			if i := strings.LastIndexByte(prog, '/'); i >= 0 {
				prog = prog[i+1:]
			}
			if prog == "" || strings.ContainsAny(prog, "\"'`$(){}<>") {
				continue
			}
			if (prog == "go" || prog == "git" || prog == "npm" || prog == "make" || prog == "just" || prog == "docker" || prog == "kubectl" || prog == "gh") && len(fields) > 1 && !strings.HasPrefix(fields[1], "-") {
				prog += " " + fields[1]
			}
			cmds = appendFacet(cmds, prog)
		}
	}
	return files, cmds
}

func appendFacet(to []string, vals ...string) []string {
	for _, v := range vals {
		if v != "" && len(to) < maxFacets && !slices.Contains(to, v) {
			to = append(to, clip(v, 200))
		}
	}
	return to
}

// texts is a message's text parts, whichever shape its content has.
func texts(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []part
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b []string
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			b = append(b, p.Text)
		}
	}
	return strings.Join(b, "\n")
}

// dedupe keeps the last passage of each key, in the order they were last
// seen.
func dedupe(ps []Passage) []Passage {
	if len(ps) < 2 {
		return ps
	}
	last := map[string]int{}
	for i, p := range ps {
		last[p.Key()] = i
	}
	out := ps[:0:0]
	for i, p := range ps {
		if last[p.Key()] == i {
			out = append(out, p)
		}
	}
	return out
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip cuts s to at most n bytes, on a character boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// headTail keeps the start and end of long output.
func headTail(s string, head, tail int) string {
	if len(s) <= head+tail+20 {
		return s
	}
	h := head
	for h > 0 && !utf8.RuneStart(s[h]) {
		h--
	}
	t := len(s) - tail
	for t < len(s) && !utf8.RuneStart(s[t]) {
		t++
	}
	return s[:h] + "\n…\n" + s[t:]
}

// Entry is one transcript entry as a person reads it, redacted: what was
// said, the calls made, and the start and end of their results. Thinking
// and injected messages are left out (ok false).
type Entry struct {
	ID   string
	Role string
	Time time.Time
	Text string
}

// Render reads one transcript line as an Entry. A result's output is cut to
// its start and end, maxOutput bytes in all.
func Render(line string, maxOutput int) (Entry, bool) {
	var e entry
	if json.Unmarshal([]byte(line), &e) != nil || e.ID == "" {
		return Entry{}, false
	}
	at, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
	out := Entry{ID: e.ID, Time: at}
	switch e.Type {
	case "compaction":
		out.Role, out.Text = "compaction", clip(redact.Text(e.Summary), 4000)
	case "message":
		var m message
		if json.Unmarshal(e.Message, &m) != nil {
			return Entry{}, false
		}
		switch m.Role {
		case "user":
			out.Role, out.Text = "user", redact.Text(strings.TrimSpace(texts(m.Content)))
		case "assistant":
			var parts []part
			_ = json.Unmarshal(m.Content, &parts)
			var b []string
			for _, p := range parts {
				switch p.Type {
				case "text":
					if s := strings.TrimSpace(p.Text); s != "" {
						b = append(b, redact.Text(s))
					}
				case "toolCall":
					l, _, _ := callLine(p)
					b = append(b, "→ "+l)
				}
			}
			out.Role, out.Text = "assistant", strings.Join(b, "\n")
		case "toolResult":
			out.Role = "result"
			if m.IsError {
				out.Role = "error"
			}
			half := max(maxOutput/2, 100)
			out.Text = m.ToolName + ": " + headTail(redact.Text(strings.TrimSpace(texts(m.Content))), half, half)
		default:
			return Entry{}, false
		}
	default:
		return Entry{}, false
	}
	// Redacted again as a whole: a secret can span parts, or sit in a
	// tool's name.
	out.Text = redact.Text(out.Text)
	if strings.TrimSpace(out.Text) == "" {
		return Entry{}, false
	}
	return out, true
}
