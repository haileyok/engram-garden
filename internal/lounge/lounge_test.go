package lounge

import (
	"fmt"
	"testing"
	"time"

	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/lex"
)

const testSpace = "at://did:plc:principal/space/garden.engram.lounge/agents"

func msg(text string, created time.Time, to ...string) indexer.Change {
	return indexer.Change{Collection: lex.MessageCollection, Rkey: text, Record: lex.Message{To: to, Text: text, CreatedAt: created.Format(time.RFC3339Nano)}.Record()}
}

// The store keeps messages within its TTL and count, and a subscriber
// resumes from any cursor still in the log, else gets a snapshot.
func TestStoreKeepsWhatItShould(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	s := &Service{MessageTTL: time.Hour, MaxMessages: 3, Now: func() time.Time { return now }}
	s.init()

	s.applyChanges(testSpace, "did:plc:a", indexer.Position{}, []indexer.Change{
		msg("old", now.Add(-2*time.Hour)),
		msg("m1", now.Add(-4*time.Minute)),
		msg("m2", now.Add(-3*time.Minute), "did:plc:b"),
		msg("m3", now.Add(-2*time.Minute)),
		msg("m4", now.Add(-1*time.Minute), "did:plc:c"),
	})
	all, _, err := s.ListMessages(testSpace, "", time.Time{}, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Text != "m2" || all[2].Text != "m4" {
		t.Fatalf("kept: %+v", all)
	}
	forB, _, _ := s.ListMessages(testSpace, "did:plc:b", time.Time{}, 10, "")
	if len(forB) != 2 || forB[0].Text != "m2" || forB[1].Text != "m3" {
		t.Fatalf("for b: %+v", forB)
	}

	sub := s.Subscribe(testSpace, "did:plc:b", "")
	defer sub.Close()
	if sub.Reset || len(sub.Backlog) != 2 {
		t.Fatalf("snapshot: %+v", sub)
	}
	cursor := sub.Cursor
	s.applyChanges(testSpace, "did:plc:a", indexer.Position{}, []indexer.Change{msg("m5", now, "did:plc:b"), msg("m6", now, "did:plc:c")})
	select {
	case e := <-sub.Events:
		if e.Message == nil || e.Message.Text != "m5" {
			t.Fatalf("live: %+v", e)
		}
	default:
		t.Fatal("no live event")
	}
	select {
	case e := <-sub.Events:
		t.Fatalf("got another's message: %+v", e)
	default:
	}
	resumed := s.Subscribe(testSpace, "did:plc:b", cursor)
	defer resumed.Close()
	if resumed.Reset || len(resumed.Backlog) != 1 || resumed.Backlog[0].Message.Text != "m5" {
		t.Fatalf("resumed: %+v", resumed.Backlog)
	}
	stale := s.Subscribe(testSpace, "", "x-1")
	defer stale.Close()
	if !stale.Reset {
		t.Fatal("a cursor from another epoch didn't reset")
	}

	// Past the TTL, messages go.
	now = now.Add(2 * time.Hour)
	s.Tick()
	if left, _, _ := s.ListMessages(testSpace, "", time.Time{}, 10, ""); len(left) != 0 {
		t.Fatalf("expired messages kept: %+v", left)
	}
}

// A slow subscriber is cut off instead of holding up the others.
func TestSlowSubscriberIsDropped(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	s := &Service{MaxMessages: 10000, Now: func() time.Time { return now }}
	sub := s.Subscribe(testSpace, "", "")
	for i := range subBuffer + 1 {
		s.applyChanges(testSpace, "did:plc:a", indexer.Position{}, []indexer.Change{msg(fmt.Sprint("m", i), now)})
	}
	n := 0
	for range sub.Events {
		n++
	}
	if n != subBuffer {
		t.Fatalf("got %d events before the cut", n)
	}
	sub.Close() // after the cut, closing again is fine
}
