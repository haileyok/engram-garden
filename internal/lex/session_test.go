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
		Status: "active", Mode: SessionModeBlob, UpdatedAt: "2026-10-23T07:00:00.000Z",
		Head:   &SessionHead{Bytes: 10, Entries: 1, EntryID: "e1"},
		Chunks: []SessionChunkRef{{Blob: &Blob{Link: "bafkreiabc", MimeType: SessionBlobMime, Size: 10}, Bytes: 10, Entries: 1, FirstEntry: "e1", LastEntry: "e1"}},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}

	bad := map[string]func(*Session){
		"no session id":      func(s *Session) { s.SessionID = "" },
		"unsafe session id":  func(s *Session) { s.SessionID = "a/b" },
		"long title":         func(s *Session) { s.Title = strings.Repeat("t", MaxSessionTitle+1) },
		"long summary":       func(s *Session) { s.Summary = strings.Repeat("s", MaxSessionSummary+1) },
		"unknown status":     func(s *Session) { s.Status = "paused" },
		"unknown mode":       func(s *Session) { s.Mode = "sideways" },
		"no updatedAt":       func(s *Session) { s.UpdatedAt = "" },
		"bad updatedAt":      func(s *Session) { s.UpdatedAt = "yesterday" },
		"chunk with neither": func(s *Session) { s.Chunks[0].Blob = nil },
		"chunk with both":    func(s *Session) { s.Chunks[0].Rkey = "x-0000" },
		"empty blob ref":     func(s *Session) { s.Chunks[0].Blob = &Blob{} },
		"head disagrees":     func(s *Session) { s.Head.Bytes = 11 },
		"too many chunks":    func(s *Session) { s.Chunks = make([]SessionChunkRef, MaxSessionChunks+1) },
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

	// A metadata record stores no bytes, so its head may say none.
	meta := Session{SessionID: "handoff", Title: "t", Summary: "s", Mode: SessionModeMetadata, UpdatedAt: ok.UpdatedAt}
	if err := meta.Validate(); err != nil {
		t.Errorf("metadata record rejected: %v", err)
	}
}

func TestSessionChunkValidate(t *testing.T) {
	t.Parallel()
	c := SessionChunk{SessionID: "s1", Index: 0, Data: "a\nb\n", Bytes: 4, Entries: 2, FirstEntry: "a", LastEntry: "b"}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid chunk rejected: %v", err)
	}
	for name, mutate := range map[string]func(*SessionChunk){
		"no data":     func(c *SessionChunk) { c.Data = "" },
		"wrong bytes": func(c *SessionChunk) { c.Bytes = 5 },
		"negative":    func(c *SessionChunk) { c.Index = -1 },
		"long data": func(c *SessionChunk) {
			c.Data = strings.Repeat("x", MaxSessionChunkData+1)
			c.Bytes = int64(len(c.Data))
		},
		"bad digest": func(c *SessionChunk) { c.SHA256 = "abc" },
	} {
		bad := c
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
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

func TestChunkRkey(t *testing.T) {
	t.Parallel()
	rkey := ChunkRkey("01a16641-36cd", 7)
	if rkey != "01a16641-36cd-0007" {
		t.Fatalf("rkey %q", rkey)
	}
	id, n, ok := ChunkIndexOf(rkey)
	if !ok || id != "01a16641-36cd" || n != 7 {
		t.Fatalf("ChunkIndexOf: %q %d %v", id, n, ok)
	}
	if _, _, ok := ChunkIndexOf("nochunkhere"); ok {
		t.Fatal("a plain id parsed as a chunk")
	}
}
