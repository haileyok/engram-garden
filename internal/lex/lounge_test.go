package lex

import (
	"strings"
	"testing"
	"time"
)

func TestPresence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	p := Presence{State: StateWorking, Title: strings.Repeat("é", 400), UpdatedAt: "2026-11-20T12:00:00.000Z", LeaseUntil: "2026-11-20T12:05:00.000Z"}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(p.Title)); n != 150 || len(p.Title) != 300 { // the byte limit binds first
		t.Fatalf("title clipped to %d runes, %d bytes", n, len(p.Title))
	}
	if !p.Online(now) || p.Online(now.Add(6*time.Minute)) {
		t.Fatal("online follows the lease")
	}
	p.State = StateEnded
	if p.Online(now) {
		t.Fatal("an ended agent is offline")
	}
	got, err := ParsePresence(p.Record())
	if err != nil || got.State != StateEnded || got.Title != p.Title {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, bad := range []map[string]any{
		{"state": "idle", "updatedAt": "2026-11-20T12:00:00Z"},
		{"state": "idle", "updatedAt": "2026-11-20T12:00:00Z", "leaseUntil": "soon"},
		{"updatedAt": "2026-11-20T12:00:00Z", "leaseUntil": "2026-11-20T12:00:00Z"},
	} {
		if _, err := ParsePresence(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if err := (&Presence{State: "idle", Parent: "nope", UpdatedAt: "2026-11-20T12:00:00Z", LeaseUntil: "2026-11-20T12:00:00Z"}).Normalize(); err == nil {
		t.Fatal("a parent that isn't a DID was accepted")
	}
}

func TestMessage(t *testing.T) {
	t.Parallel()
	m := Message{To: []string{"did:plc:a", "did:plc:a", "did:plc:b"}, Text: "hi", CreatedAt: "2026-11-20T12:00:00.000Z"}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(m.To) != 2 {
		t.Fatalf("recipients deduplicated: %v", m.To)
	}
	if !m.ForAgent("did:plc:b") || m.ForAgent("did:plc:c") || !(Message{}).ForAgent("did:plc:c") {
		t.Fatal("ForAgent")
	}
	got, err := ParseMessage(m.Record())
	if err != nil || got.Text != "hi" || len(got.To) != 2 {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for name, bad := range map[string]Message{
		"no text":       {CreatedAt: "2026-11-20T12:00:00Z"},
		"bad recipient": {Text: "x", To: []string{"bob"}, CreatedAt: "2026-11-20T12:00:00Z"},
		"bad kind":      {Text: "x", Kind: "shout", CreatedAt: "2026-11-20T12:00:00Z"},
		"reply w/o ref": {Text: "x", Kind: KindReply, CreatedAt: "2026-11-20T12:00:00Z"},
		"no createdAt":  {Text: "x"},
		"too long":      {Text: strings.Repeat("x", MaxMessageChars+1), CreatedAt: "2026-11-20T12:00:00Z"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	exp := Message{Text: "x", ExpiresAt: "2026-11-20T12:00:00Z"}
	if !exp.Expired(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)) || exp.Expired(time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)) {
		t.Fatal("Expired")
	}
}
