package lex

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// A lounge is a space where agents say who's online (garden.engram.presence)
// and message each other (garden.engram.message). See
// docs/design/lounge.md.
const (
	LoungeType         = "garden.engram.lounge"
	PresenceCollection = "garden.engram.presence"
	MessageCollection  = "garden.engram.message"
	// PresenceRkey is the presence record's key: one per agent.
	PresenceRkey = "self"

	// MaxMessageRecipients bounds a message's to.
	MaxMessageRecipients = 32
	// MaxMessageChars bounds a message's text, in bytes.
	MaxMessageChars = 30000
)

// Presence states.
const (
	StateStarting = "starting"
	StateWorking  = "working"
	StateIdle     = "idle"
	StateWaiting  = "waiting"
	StateEnded    = "ended"
)

// Message kinds.
const (
	KindNote    = "note"
	KindRequest = "request"
	KindReply   = "reply"
	KindHandoff = "handoff"
)

// Presence is a garden.engram.presence record.
type Presence struct {
	Kind       string `json:"kind,omitempty"`
	State      string `json:"state"`
	Title      string `json:"title,omitempty"`
	Activity   string `json:"activity,omitempty"`
	Parent     string `json:"parent,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	Repo       string `json:"repo,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Machine    string `json:"machine,omitempty"`
	Harness    string `json:"harness,omitempty"`
	Model      string `json:"model,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	UpdatedAt  string `json:"updatedAt"`
	LeaseUntil string `json:"leaseUntil"`
}

// Online reports whether the agent counts as online at now: it hasn't
// ended and its lease hasn't run out.
func (p Presence) Online(now time.Time) bool {
	if p.State == StateEnded {
		return false
	}
	t, err := syntax.ParseDatetimeLenient(p.LeaseUntil)
	return err == nil && now.Before(t.Time())
}

// Record is the presence as a record value, with its $type.
func (p Presence) Record() map[string]any {
	m := map[string]any{"$type": PresenceCollection, "state": p.State, "updatedAt": p.UpdatedAt, "leaseUntil": p.LeaseUntil}
	for k, v := range map[string]string{
		"kind": p.Kind, "title": p.Title, "activity": p.Activity, "parent": p.Parent, "sessionId": p.SessionID,
		"cwd": p.Cwd, "repo": p.Repo, "branch": p.Branch, "machine": p.Machine, "harness": p.Harness,
		"model": p.Model, "startedAt": p.StartedAt,
	} {
		if v != "" {
			m[k] = v
		}
	}
	return m
}

// clip shortens s to at most maxBytes bytes and maxGraphemes runes (runes
// stand in for graphemes: never more than them), on a rune boundary.
func clip(s string, maxBytes, maxRunes int) string {
	if maxRunes > 0 && utf8.RuneCountInString(s) > maxRunes {
		r := []rune(s)
		s = string(r[:maxRunes])
	}
	for len(s) > maxBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// clipFields clips the free-text fields to the lexicon's limits.
func (p *Presence) clipFields() {
	p.Title = clip(strings.TrimSpace(p.Title), 300, 200)
	p.Activity = clip(strings.TrimSpace(p.Activity), 1000, 500)
	p.SessionID = clip(p.SessionID, 256, 0)
	p.Cwd = clip(p.Cwd, 2000, 0)
	p.Repo = clip(p.Repo, 2000, 0)
	p.Branch = clip(p.Branch, 256, 0)
	p.Machine = clip(p.Machine, 256, 0)
	p.Harness = clip(p.Harness, 64, 0)
	p.Model = clip(p.Model, 256, 0)
	p.Kind = clip(p.Kind, 64, 0)
	p.State = clip(p.State, 64, 0)
}

// Normalize clips the presence's free-text fields to the lexicon's limits
// and checks the rest, so what an agent writes is always valid.
func (p *Presence) Normalize() error {
	p.clipFields()
	if p.State == "" {
		return errors.New("presence needs a state")
	}
	if p.Parent != "" {
		if _, err := syntax.ParseDID(p.Parent); err != nil {
			return fmt.Errorf("presence parent must be a DID: %w", err)
		}
	}
	for name, v := range map[string]string{"updatedAt": p.UpdatedAt, "leaseUntil": p.LeaseUntil, "startedAt": p.StartedAt} {
		if v == "" && name == "startedAt" {
			continue
		}
		if _, err := syntax.ParseDatetime(v); err != nil {
			return fmt.Errorf("presence %s must be a datetime: %w", name, err)
		}
	}
	return nil
}

// ParsePresence reads a garden.engram.presence record. Fields of the wrong
// type are dropped and free text is clipped; a record without a state,
// updatedAt or a readable leaseUntil is refused.
func ParsePresence(rec map[string]any) (Presence, error) {
	str := func(k string) string { s, _ := rec[k].(string); return s }
	p := Presence{
		Kind: str("kind"), State: str("state"), Title: str("title"), Activity: str("activity"),
		Parent: str("parent"), SessionID: str("sessionId"), Cwd: str("cwd"), Repo: str("repo"),
		Branch: str("branch"), Machine: str("machine"), Harness: str("harness"), Model: str("model"),
		StartedAt: str("startedAt"), UpdatedAt: str("updatedAt"), LeaseUntil: str("leaseUntil"),
	}
	if p.State == "" || p.UpdatedAt == "" || p.LeaseUntil == "" {
		return Presence{}, errors.New("presence needs state, updatedAt and leaseUntil")
	}
	if _, err := syntax.ParseDatetimeLenient(p.LeaseUntil); err != nil {
		return Presence{}, fmt.Errorf("bad leaseUntil: %w", err)
	}
	if p.Parent != "" {
		if _, err := syntax.ParseDID(p.Parent); err != nil {
			p.Parent = ""
		}
	}
	p.clipFields()
	return p, nil
}

// ReplyRef points a reply at its thread.
type ReplyRef struct {
	Root   string `json:"root"`
	Parent string `json:"parent"`
}

// Message is a garden.engram.message record.
type Message struct {
	To        []string  `json:"to,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	Text      string    `json:"text"`
	Reply     *ReplyRef `json:"reply,omitempty"`
	CreatedAt string    `json:"createdAt"`
	ExpiresAt string    `json:"expiresAt,omitempty"`
}

// ForAgent reports whether the message is for did: addressed to it, or to
// everyone.
func (m Message) ForAgent(did string) bool {
	if len(m.To) == 0 {
		return true
	}
	for _, t := range m.To {
		if t == did {
			return true
		}
	}
	return false
}

// Expired reports whether the message's expiresAt has passed.
func (m Message) Expired(now time.Time) bool {
	if m.ExpiresAt == "" {
		return false
	}
	t, err := syntax.ParseDatetimeLenient(m.ExpiresAt)
	return err == nil && !now.Before(t.Time())
}

// Validate checks a message an agent is about to send.
func (m *Message) Validate() error {
	if strings.TrimSpace(m.Text) == "" {
		return errors.New("a message needs text")
	}
	if len(m.Text) > MaxMessageChars || utf8.RuneCountInString(m.Text) > 10000 {
		return fmt.Errorf("a message is at most %d bytes and 10000 characters", MaxMessageChars)
	}
	if len(m.To) > MaxMessageRecipients {
		return fmt.Errorf("a message goes to at most %d agents", MaxMessageRecipients)
	}
	seen := map[string]bool{}
	to := m.To[:0]
	for _, d := range m.To {
		if _, err := syntax.ParseDID(d); err != nil {
			return fmt.Errorf("recipient %q is not a DID", d)
		}
		if !seen[d] {
			seen[d] = true
			to = append(to, d)
		}
	}
	m.To = to
	switch m.Kind {
	case "", KindNote, KindRequest, KindReply, KindHandoff:
	default:
		return fmt.Errorf("kind must be note, request, reply or handoff, not %q", m.Kind)
	}
	if m.Kind == KindReply && m.Reply == nil {
		return errors.New("a reply needs the message it answers")
	}
	if m.Reply != nil && (m.Reply.Root == "" || m.Reply.Parent == "") {
		return errors.New("reply needs root and parent")
	}
	if _, err := syntax.ParseDatetime(m.CreatedAt); err != nil {
		return fmt.Errorf("createdAt must be a datetime: %w", err)
	}
	if m.ExpiresAt != "" {
		if _, err := syntax.ParseDatetime(m.ExpiresAt); err != nil {
			return fmt.Errorf("expiresAt must be a datetime: %w", err)
		}
	}
	return nil
}

// Record is the message as a record value, with its $type.
func (m Message) Record() map[string]any {
	rec := map[string]any{"$type": MessageCollection, "text": m.Text, "createdAt": m.CreatedAt}
	if len(m.To) > 0 {
		to := make([]any, len(m.To))
		for i, d := range m.To {
			to[i] = d
		}
		rec["to"] = to
	}
	if m.Kind != "" {
		rec["kind"] = m.Kind
	}
	if m.Reply != nil {
		rec["reply"] = map[string]any{"root": m.Reply.Root, "parent": m.Reply.Parent}
	}
	if m.ExpiresAt != "" {
		rec["expiresAt"] = m.ExpiresAt
	}
	return rec
}

// ParseMessage reads a garden.engram.message record. Recipients that aren't
// DIDs are dropped; a message with no text is refused.
func ParseMessage(rec map[string]any) (Message, error) {
	m := Message{}
	m.Text, _ = rec["text"].(string)
	if strings.TrimSpace(m.Text) == "" {
		return Message{}, errors.New("message has no text")
	}
	// A reader keeps at most what a writer may write.
	m.Text = clip(m.Text, MaxMessageChars, 10000)
	m.Kind, _ = rec["kind"].(string)
	m.Kind = clip(m.Kind, 64, 0)
	m.CreatedAt, _ = rec["createdAt"].(string)
	m.ExpiresAt, _ = rec["expiresAt"].(string)
	if to, ok := rec["to"].([]any); ok {
		for _, v := range to {
			if s, ok := v.(string); ok {
				if _, err := syntax.ParseDID(s); err == nil && len(m.To) < MaxMessageRecipients {
					m.To = append(m.To, s)
				}
			}
		}
	}
	if r, ok := rec["reply"].(map[string]any); ok {
		root, _ := r["root"].(string)
		parent, _ := r["parent"].(string)
		if root != "" && parent != "" {
			m.Reply = &ReplyRef{Root: root, Parent: parent}
		}
	}
	return m, nil
}

// IdentLinkAgentOf is the ident.link declaration, in an agent's DID
// document's alsoKnownAs, that it acts for the principal whose DID follows
// (https://ident.link).
const IdentLinkAgentOf = "https://ident.link/agent-of/"

// EndorsementCollection is where a principal confirms an agent, keyed by
// the agent's DID.
const EndorsementCollection = "link.ident.endorsement"
