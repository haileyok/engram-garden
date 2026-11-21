package lex

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// An archive is a space where agent sessions keep their transcripts, each
// written by the session's own identity. A session record says where the
// work stands and names the transcript's bytes: chunks of complete
// transcript lines in the identity's blobstore. See docs/design/sessions.md.
const (
	ArchiveType       = "garden.engram.archive"
	SessionCollection = "garden.engram.session"
)

// SessionBlobMime is what a transcript chunk uploads as.
const SessionBlobMime = "application/octet-stream"

// Session statuses.
const (
	SessionActive = "active"
	SessionEnded  = "ended"
)

// Session kinds: a top-level session, or a subagent another agent started.
const (
	SessionKindSession  = "session"
	SessionKindSubagent = "subagent"
)

// Session is a garden.engram.session record.
type Session struct {
	// SessionID is the harness's session ID, which is also the record key.
	SessionID string `json:"sessionId,omitempty"`
	// Title is a short name for the work.
	Title string `json:"title"`
	// Summary is markdown: the goal, what's done, what's in progress,
	// decisions, and the next steps. The harness's own compaction summary
	// when it has one.
	Summary string `json:"summary"`
	// Status is active or ended; empty when the writer didn't say.
	Status string `json:"status,omitempty"`
	// Kind is session or subagent, and Parent the DID of the agent that
	// started a subagent. Both come from the writer's identity.
	Kind   string `json:"kind,omitempty"`
	Parent string `json:"parent,omitempty"`
	// Repo is the repository, as its remote URL.
	Repo string `json:"repo,omitempty"`
	// Branch is the branch checked out.
	Branch string `json:"branch,omitempty"`
	// Cwd is the working directory, on the machine.
	Cwd string `json:"cwd,omitempty"`
	// Machine is the machine's host name.
	Machine string `json:"machine,omitempty"`
	// Harness is the agent harness, such as pi.
	Harness string `json:"harness,omitempty"`
	// Model is the model the session was using.
	Model string `json:"model,omitempty"`
	// GitHead is the commit the working tree was on, and GitDirty whether
	// it had uncommitted changes: resuming needs the same code.
	GitHead  string `json:"gitHead,omitempty"`
	GitDirty bool   `json:"gitDirty,omitempty"`
	// StartedAt and UpdatedAt are when the session began and when this
	// record was last written.
	StartedAt string `json:"startedAt,omitempty"`
	UpdatedAt string `json:"updatedAt"`
	// Head says how much of the transcript is stored.
	Head *SessionHead `json:"head,omitempty"`
	// Chunks name the transcript's bytes, oldest first.
	Chunks []SessionChunkRef `json:"chunks,omitempty"`
}

// SessionHead is how far the stored transcript goes.
type SessionHead struct {
	// EntryID is the last transcript entry stored.
	EntryID string `json:"entryId,omitempty"`
	// Entries and Bytes count the whole transcript, not one chunk.
	Entries int   `json:"entries,omitempty"`
	Bytes   int64 `json:"bytes,omitempty"`
}

// SessionChunkRef is one stored chunk of transcript lines: a blob, whose CID
// is the digest of its bytes.
type SessionChunkRef struct {
	Blob Blob `json:"blob"`
	// Bytes, Entries, FirstEntry and LastEntry describe the chunk's lines.
	Bytes      int64  `json:"bytes"`
	Entries    int    `json:"entries,omitempty"`
	FirstEntry string `json:"firstEntry,omitempty"`
	LastEntry  string `json:"lastEntry,omitempty"`
}

// Blob is a blob reference as a record writes it; the PDS reads refs in this
// shape out of a space record to learn which blobs the space may serve.
type Blob struct {
	Link     string `json:"$link"`
	MimeType string `json:"mimeType,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

// UnmarshalJSON reads a blob ref in the shape a record writes it,
// {"$type":"blob","ref":{"$link":…}}, and also a bare {"$link":…}.
func (b *Blob) UnmarshalJSON(data []byte) error {
	var raw struct {
		Link *string `json:"$link"`
		Ref  *struct {
			Link string `json:"$link"`
		} `json:"ref"`
		MimeType string `json:"mimeType"`
		Size     int64  `json:"size"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch {
	case raw.Ref != nil:
		b.Link = raw.Ref.Link
	case raw.Link != nil:
		b.Link = *raw.Link
	}
	b.MimeType, b.Size = raw.MimeType, raw.Size
	return nil
}

// MarshalJSON writes the ref the way a record holds it.
func (b Blob) MarshalJSON() ([]byte, error) {
	return json.Marshal(b.Record())
}

// Record is the blob as a record field: {"$type":"blob","ref":{"$link":…}}.
func (b Blob) Record() map[string]any {
	m := map[string]any{"$type": "blob", "ref": map[string]any{"$link": b.Link}}
	if b.MimeType != "" {
		m["mimeType"] = b.MimeType
	}
	if b.Size > 0 {
		m["size"] = b.Size
	}
	return m
}

// Limits, from the lexicon.
const (
	MaxSessionTitle    = 200
	MaxSessionSummary  = 50000
	MaxSessionChunks   = 2000
	MaxSessionRefField = 256
)

var sessionID = regexp.MustCompile(`^[A-Za-z0-9._~:-]{1,128}$`)

// ValidSessionID reports whether id can key a session record: the characters
// a record key allows, at most 128 of them.
func ValidSessionID(id string) bool { return sessionID.MatchString(id) }

// Validate checks the record against the lexicon and its limits.
func (s *Session) Validate() error {
	if s == nil {
		return fmt.Errorf("no session record")
	}
	if !ValidSessionID(s.SessionID) {
		return fmt.Errorf("%q isn't a session id", s.SessionID)
	}
	if len(s.Title) > MaxSessionTitle {
		return fmt.Errorf("title is %d characters; the most is %d", len(s.Title), MaxSessionTitle)
	}
	if len(s.Summary) > MaxSessionSummary {
		return fmt.Errorf("summary is %d characters; the most is %d", len(s.Summary), MaxSessionSummary)
	}
	switch s.Status {
	case "", SessionActive, SessionEnded:
	default:
		return fmt.Errorf("status is %q; it can be active or ended", s.Status)
	}
	switch s.Kind {
	case "", SessionKindSession, SessionKindSubagent:
	default:
		return fmt.Errorf("kind is %q; it can be session or subagent", s.Kind)
	}
	if s.Parent != "" {
		if _, err := syntax.ParseDID(s.Parent); err != nil {
			return fmt.Errorf("parent %q isn't a DID", s.Parent)
		}
	}
	if s.UpdatedAt == "" {
		return fmt.Errorf("a session record needs updatedAt")
	}
	if _, err := time.Parse(time.RFC3339, s.UpdatedAt); err != nil {
		return fmt.Errorf("updatedAt %q isn't a timestamp", s.UpdatedAt)
	}
	if len(s.Chunks) > MaxSessionChunks {
		return fmt.Errorf("%d chunks; the most is %d", len(s.Chunks), MaxSessionChunks)
	}
	var stored int64
	for i, c := range s.Chunks {
		if c.Blob.Link == "" {
			return fmt.Errorf("chunk %d has an empty blob ref", i)
		}
		if c.Bytes <= 0 {
			return fmt.Errorf("chunk %d has no bytes", i)
		}
		for _, f := range []string{c.FirstEntry, c.LastEntry} {
			if len(f) > MaxSessionRefField {
				return fmt.Errorf("chunk %d has a field longer than %d characters", i, MaxSessionRefField)
			}
		}
		stored += c.Bytes
	}
	if s.Head != nil && s.Head.Bytes != stored {
		return fmt.Errorf("head says %d bytes but the chunks hold %d", s.Head.Bytes, stored)
	}
	return nil
}

// Record is the session as a record.
func (s *Session) Record() map[string]any {
	rec := map[string]any{
		"$type":     SessionCollection,
		"title":     s.Title,
		"summary":   s.Summary,
		"updatedAt": s.UpdatedAt,
	}
	for k, v := range map[string]string{
		"sessionId": s.SessionID, "status": s.Status, "kind": s.Kind, "parent": s.Parent,
		"repo": s.Repo, "branch": s.Branch, "cwd": s.Cwd, "machine": s.Machine,
		"harness": s.Harness, "model": s.Model, "gitHead": s.GitHead, "startedAt": s.StartedAt,
	} {
		if v != "" {
			rec[k] = v
		}
	}
	if s.GitDirty {
		rec["gitDirty"] = true
	}
	if s.Head != nil {
		head := map[string]any{"bytes": s.Head.Bytes}
		if s.Head.EntryID != "" {
			head["entryId"] = s.Head.EntryID
		}
		if s.Head.Entries > 0 {
			head["entries"] = s.Head.Entries
		}
		rec["head"] = head
	}
	if len(s.Chunks) > 0 {
		chunks := make([]map[string]any, len(s.Chunks))
		for i, c := range s.Chunks {
			m := map[string]any{"blob": c.Blob.Record(), "bytes": c.Bytes}
			if c.Entries > 0 {
				m["entries"] = c.Entries
			}
			if c.FirstEntry != "" {
				m["firstEntry"] = c.FirstEntry
			}
			if c.LastEntry != "" {
				m["lastEntry"] = c.LastEntry
			}
			chunks[i] = m
		}
		rec["chunks"] = chunks
	}
	return rec
}
