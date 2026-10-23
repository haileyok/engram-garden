package lex

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// A session record is two things at once. It says where an agent's working
// session stands, so a person or another agent can pick the work up later;
// and it is the index of that session's transcript, so the session can be
// resumed verbatim on another machine. The record key is the harness's
// session ID.
//
// The transcript itself is bytes, not records: chunks of complete transcript
// lines go in the member's blobstore, referenced here (Mode blob), and only
// the manifest is a record. Mode record is the fallback for a PDS where blob
// upload isn't allowed yet: each chunk is a garden.engram.sessionChunk
// record instead. Mode metadata means only this record was written.

// SessionBlobMime is what a transcript chunk uploads as: opaque bytes, so
// the sign-in needs blob:application/octet-stream.
const SessionBlobMime = "application/octet-stream"

// Session modes: where the transcript's bytes are.
const (
	// SessionModeBlob stores chunks as space blobs (the default).
	SessionModeBlob = "blob"
	// SessionModeRecord stores each chunk as a sessionChunk record.
	SessionModeRecord = "record"
	// SessionModeMetadata stores no transcript bytes, only this record.
	SessionModeMetadata = "metadata"
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
	// it had uncomitted changes: resuming needs the same code.
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
	// Mode says where the bytes are: blob, record or metadata.
	Mode string `json:"mode,omitempty"`
}

// SessionHead is how far the stored transcript goes.
type SessionHead struct {
	// EntryID is the last transcript entry stored.
	EntryID string `json:"entryId,omitempty"`
	// Entries and Bytes count the whole transcript, not one chunk.
	Entries int   `json:"entries,omitempty"`
	Bytes   int64 `json:"bytes,omitempty"`
}

// SessionChunkRef is one stored chunk of transcript lines: a blob, or a
// sessionChunk record's key.
type SessionChunkRef struct {
	Blob *Blob `json:"blob,omitempty"`
	// Rkey names the sessionChunk record holding the bytes (Mode record).
	Rkey string `json:"rkey,omitempty"`
	// Bytes, Entries, FirstEntry and LastEntry describe the chunk's lines.
	Bytes      int64  `json:"bytes,omitempty"`
	Entries    int    `json:"entries,omitempty"`
	FirstEntry string `json:"firstEntry,omitempty"`
	LastEntry  string `json:"lastEntry,omitempty"`
	// SHA256 is of the chunk's bytes, so a reader can check what it got.
	SHA256 string `json:"sha256,omitempty"`
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

// SessionChunk is a garden.engram.sessionChunk record: a slice of transcript
// lines, for a PDS where the transcript can't go in blobs.
type SessionChunk struct {
	SessionID string `json:"sessionId"`
	// Index is the chunk's position in the transcript, from 0.
	Index int `json:"index"`
	// Bytes, Entries, FirstEntry and LastEntry describe the lines it holds.
	Bytes      int64  `json:"bytes"`
	Entries    int    `json:"entries,omitempty"`
	FirstEntry string `json:"firstEntry,omitempty"`
	LastEntry  string `json:"lastEntry,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	// Data is the chunk's bytes: complete transcript lines.
	Data string `json:"data"`
}

// Limits, from the lexicons. MaxSessionChunkData keeps a chunk record's
// encoding inside what a space record accepts (1 MiB).
const (
	MaxSessionTitle     = 200
	MaxSessionSummary   = 50000
	MaxSessionChunks    = 2000
	MaxSessionRefField  = 256
	MaxSessionChunkData = 512 * 1024
)

var sessionID = regexp.MustCompile(`^[A-Za-z0-9._~:-]{1,128}$`)

// ValidSessionID reports whether id can key a session record: the characters
// a record key allows, at most 128 of them.
func ValidSessionID(id string) bool { return sessionID.MatchString(id) }

// Validate checks the manifest against the lexicon and its limits.
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
	case "", "active", "ended":
	default:
		return fmt.Errorf("status is %q; it can be active or ended", s.Status)
	}
	switch s.Mode {
	case "", SessionModeBlob, SessionModeRecord, SessionModeMetadata:
	default:
		return fmt.Errorf("mode is %q; it can be blob, record or metadata", s.Mode)
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
		if (c.Blob == nil) == (c.Rkey == "") {
			return fmt.Errorf("chunk %d needs exactly one of a blob and an rkey", i)
		}
		if c.Blob != nil && c.Blob.Link == "" {
			return fmt.Errorf("chunk %d has an empty blob ref", i)
		}
		if c.Bytes < 0 {
			return fmt.Errorf("chunk %d has negative size", i)
		}
		for _, f := range []string{c.FirstEntry, c.LastEntry, c.SHA256} {
			if len(f) > MaxSessionRefField {
				return fmt.Errorf("chunk %d has a field longer than %d characters", i, MaxSessionRefField)
			}
		}
		stored += c.Bytes
	}
	if s.Mode != SessionModeMetadata && s.Head != nil && s.Head.Bytes != stored {
		return fmt.Errorf("head says %d bytes but the chunks hold %d", s.Head.Bytes, stored)
	}
	return nil
}

// Validate checks a chunk record.
func (c *SessionChunk) Validate() error {
	if !ValidSessionID(c.SessionID) {
		return fmt.Errorf("%q isn't a session id", c.SessionID)
	}
	if c.Index < 0 {
		return fmt.Errorf("chunk index %d is negative", c.Index)
	}
	if c.Data == "" {
		return fmt.Errorf("a chunk record needs data")
	}
	if c.Bytes != int64(len(c.Data)) {
		return fmt.Errorf("bytes says %d but data is %d", c.Bytes, len(c.Data))
	}
	if len(c.Data) > MaxSessionChunkData {
		return fmt.Errorf("chunk data is %d bytes; the most is %d", len(c.Data), MaxSessionChunkData)
	}
	if c.SHA256 != "" && len(c.SHA256) != 64 {
		return fmt.Errorf("sha256 %q isn't a hex digest", c.SHA256)
	}
	return nil
}

// Record is the manifest as a record.
func (s *Session) Record() map[string]any {
	rec := map[string]any{
		"$type":     SessionCollection,
		"title":     s.Title,
		"summary":   s.Summary,
		"updatedAt": s.UpdatedAt,
	}
	for k, v := range map[string]string{
		"sessionId": s.SessionID, "status": s.Status, "repo": s.Repo, "branch": s.Branch,
		"cwd": s.Cwd, "machine": s.Machine, "harness": s.Harness, "model": s.Model,
		"gitHead": s.GitHead, "startedAt": s.StartedAt, "mode": s.Mode,
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
			m := map[string]any{"bytes": c.Bytes}
			if c.Blob != nil {
				m["blob"] = c.Blob.Record()
			}
			if c.Rkey != "" {
				m["rkey"] = c.Rkey
			}
			if c.Entries > 0 {
				m["entries"] = c.Entries
			}
			if c.FirstEntry != "" {
				m["firstEntry"] = c.FirstEntry
			}
			if c.LastEntry != "" {
				m["lastEntry"] = c.LastEntry
			}
			if c.SHA256 != "" {
				m["sha256"] = c.SHA256
			}
			chunks[i] = m
		}
		rec["chunks"] = chunks
	}
	return rec
}

// Record is the chunk as a record.
func (c *SessionChunk) Record() map[string]any {
	rec := map[string]any{
		"$type":     SessionChunkCollection,
		"sessionId": c.SessionID,
		"index":     c.Index,
		"bytes":     c.Bytes,
		"data":      c.Data,
	}
	if c.Entries > 0 {
		rec["entries"] = c.Entries
	}
	if c.FirstEntry != "" {
		rec["firstEntry"] = c.FirstEntry
	}
	if c.LastEntry != "" {
		rec["lastEntry"] = c.LastEntry
	}
	if c.SHA256 != "" {
		rec["sha256"] = c.SHA256
	}
	return rec
}

// ChunkRkey names the record holding chunk n of a session.
func ChunkRkey(sessionID string, n int) string {
	return fmt.Sprintf("%s-%04d", sessionID, n)
}

// ChunkIndexOf reports whether an rkey names a session's chunk, and which.
func ChunkIndexOf(rkey string) (string, int, bool) {
	i := strings.LastIndex(rkey, "-")
	if i <= 0 {
		return "", 0, false
	}
	var n int
	if _, err := fmt.Sscanf(rkey[i+1:], "%d", &n); err != nil {
		return "", 0, false
	}
	return rkey[:i], n, true
}
