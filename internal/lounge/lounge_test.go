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
	// Live: m5 for b, and the oldest messages trimmed to stay within the
	// limit; never m6, which is c's.
	var live []Event
	for len(sub.Events) > 0 {
		live = append(live, <-sub.Events)
	}
	gotM5 := false
	for _, e := range live {
		switch {
		case e.Type == EventMessage && e.Message.Text == "m5":
			gotM5 = true
		case e.Type == EventMessage:
			t.Fatalf("got another's message: %+v", e)
		case e.Type != EventMessageDeleted:
			t.Fatalf("unexpected event: %+v", e)
		}
	}
	if !gotM5 {
		t.Fatalf("no live m5: %+v", live)
	}
	resumed := s.Subscribe(testSpace, "did:plc:b", cursor)
	defer resumed.Close()
	if resumed.Reset || len(resumed.Backlog) != len(live) {
		t.Fatalf("resumed: %+v", resumed.Backlog)
	}
	// A snapshot's events carry no cursor; only ready does.
	for _, e := range sub2(s).Backlog {
		if e.ID != "" {
			t.Fatalf("snapshot event with a cursor: %+v", e)
		}
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

func sub2(s *Service) *Subscription {
	sub := s.Subscribe(testSpace, "", "")
	sub.Close()
	return sub
}

// A page cursor survives its message going away, and a deleted lounge isn't
// served as an empty one.
func TestCursorsAndDeletion(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	s := &Service{Now: func() time.Time { return now }}
	s.init()
	s.applyChanges(testSpace, "did:plc:a", indexer.Position{}, []indexer.Change{
		msg("m1", now.Add(-3*time.Minute)), msg("m2", now.Add(-2*time.Minute)), msg("m3", now.Add(-time.Minute)),
	})
	page, cursor, err := s.ListMessages(testSpace, "", time.Time{}, 2, "")
	if err != nil || len(page) != 2 || cursor == "" {
		t.Fatalf("page 1: %v %q %v", page, cursor, err)
	}
	s.applyChanges(testSpace, "did:plc:a", indexer.Position{}, []indexer.Change{{Collection: lex.MessageCollection, Rkey: "m2"}})
	page, cursor, err = s.ListMessages(testSpace, "", time.Time{}, 2, cursor)
	if err != nil || len(page) != 1 || page[0].Text != "m3" || cursor != "" {
		t.Fatalf("page 2 after its cursor's message went: %v %q %v", page, cursor, err)
	}
	if _, _, err := s.ListMessages(testSpace, "", time.Time{}, 2, "garbage"); err == nil {
		t.Fatal("a bad cursor was accepted")
	}
	s.dropSpace(testSpace)
	if !s.Deleted(testSpace) {
		t.Fatal("a dropped lounge isn't marked deleted")
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
