package lex

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionValidate(t *testing.T) {
	t.Parallel()
	ok := Session{
		SessionID: "01a16641-36cd-7695-a8b2-e41aada6d16d", Title: "two words", Summary: "done",
		Status: "active", Kind: "subagent", Parent: "did:plc:parent", UpdatedAt: "2026-10-23T07:00:00.000Z",
		Head:   &SessionHead{Bytes: 10, Entries: 1, EntryID: "e1"},
		Chunks: []SessionChunkRef{{Blob: Blob{Link: "bafkreiabc", MimeType: SessionBlobMime, Size: 10}, Bytes: 10, Entries: 1, FirstEntry: "e1", LastEntry: "e1"}},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}

	bad := map[string]func(*Session){
		"no session id":     func(s *Session) { s.SessionID = "" },
		"unsafe session id": func(s *Session) { s.SessionID = "a/b" },
		"long title":        func(s *Session) { s.Title = strings.Repeat("t", MaxSessionTitle+1) },
		"long summary":      func(s *Session) { s.Summary = strings.Repeat("s", MaxSessionSummary+1) },
		"unknown status":    func(s *Session) { s.Status = "paused" },
		"unknown kind":      func(s *Session) { s.Kind = "daemon" },
		"parent not a DID":  func(s *Session) { s.Parent = "alice" },
		"no updatedAt":      func(s *Session) { s.UpdatedAt = "" },
		"bad updatedAt":     func(s *Session) { s.UpdatedAt = "yesterday" },
		"empty blob ref":    func(s *Session) { s.Chunks[0].Blob = Blob{} },
		"empty chunk":       func(s *Session) { s.Chunks[0].Bytes = 0 },
		"head disagrees":    func(s *Session) { s.Head.Bytes = 11 },
		"too many chunks":   func(s *Session) { s.Chunks = make([]SessionChunkRef, MaxSessionChunks+1) },
	}
	for name, mutate := range bad {
		s := ok
		s.Chunks = []SessionChunkRef{ok.Chunks[0]}
		s.Head = &SessionHead{Bytes: 10, Entries: 1, EntryID: "e1"}
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A record with no transcript yet is fine.
	meta := Session{SessionID: "handoff", Title: "t", Summary: "s", UpdatedAt: ok.UpdatedAt}
	if err := meta.Validate(); err != nil {
		t.Errorf("record without a transcript rejected: %v", err)
	}
}

func TestSessionRecordRoundTrips(t *testing.T) {
	t.Parallel()
	s := Session{
		SessionID: "s1", Title: "t", Summary: "s", Status: "ended", Kind: "subagent", Parent: "did:plc:p",
		Repo: "git@github.com:x/y.git", GitDirty: true, UpdatedAt: "2026-10-23T07:00:00.000Z",
		Head:   &SessionHead{Bytes: 4, Entries: 2, EntryID: "b"},
		Chunks: []SessionChunkRef{{Blob: Blob{Link: "bafkreiabc", MimeType: SessionBlobMime, Size: 4}, Bytes: 4, Entries: 2, FirstEntry: "a", LastEntry: "b"}},
	}
	raw, err := json.Marshal(s.Record())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"blob":{"$type":"blob"`) {
		t.Fatalf("chunk blobs aren't in record shape: %s", raw)
	}
	var back Session
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(s)
	b, _ := json.Marshal(back)
	if string(a) != string(b) {
		t.Fatalf("round trip:\n%s\n%s", a, b)
	}
}

func TestSessionBlobShapes(t *testing.T) {
	t.Parallel()
	// A record writes the wrapped shape and reads it back.
	b := Blob{Link: "bafkreiabc", MimeType: SessionBlobMime, Size: 12}
	raw, err := b.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"$type":"blob"`) || !strings.Contains(string(raw), `"$link":"bafkreiabc"`) {
		t.Fatalf("wrapped shape: %s", raw)
	}
	var got Blob
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got != b {
		t.Fatalf("round trip: %+v", got)
	}
	// A bare {"$link":…} reads too.
	var bare Blob
	if err := json.Unmarshal([]byte(`{"$link":"bafkreiabc"}`), &bare); err != nil {
		t.Fatal(err)
	}
	if bare.Link != "bafkreiabc" {
		t.Fatalf("bare: %+v", bare)
	}
}
