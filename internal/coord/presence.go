package coord

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// One run of an agent acts at a time. The agent's presence record (key
// self, in its own repo in the workspace) names the run holding the lease.
// A run takes over from a live one by writing presence with handoffFrom
// set; it starts acting once the old run writes a handoff record for it
// (Yield) or the old lease ends, so the two never act at once.
//
// Space writes have no compare-and-swap, so two runs acquiring an expired
// lease at the same moment can both briefly think they hold it; each
// re-reads presence after writing it, and Active (checked before every
// write when RequireLease is set) settles it on the next write.

// LeaseState is what Acquire got.
type LeaseState int

const (
	// LeaseActive: this run holds the lease and may act.
	LeaseActive LeaseState = iota + 1
	// LeasePending: this run is taking over from a live run, and may act
	// once that run yields or its lease ends.
	LeasePending
)

func (s LeaseState) String() string {
	switch s {
	case LeaseActive:
		return "active"
	case LeasePending:
		return "pending"
	}
	return "none"
}

// ErrNotActive: this run doesn't hold the agent's lease.
var ErrNotActive = errors.New("this run doesn't hold the agent's lease")

// LeaseHeldError: another run holds a live lease.
type LeaseHeldError struct{ Presence Presence }

func (e *LeaseHeldError) Error() string {
	return fmt.Sprintf("run %s on %s holds the agent until %s", e.Presence.Run, e.Presence.Device, e.Presence.LeaseUntil)
}

// Presence reads the agent's presence record, or nil if it has none.
func (w *Workspace) Presence(ctx context.Context) (*Presence, error) {
	rec, err := w.read(ctx, w.me(), PresenceCollection, SelfRkey)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Presence
	if err := rec.Decode(&p); err != nil {
		return nil, fmt.Errorf("reading presence: %w", err)
	}
	return &p, nil
}

func (w *Workspace) putPresence(ctx context.Context, until time.Time, from *RunRef) error {
	if w.Generator.Run == "" {
		return errors.New("no run: set Generator.Run (coord.NewRun) when the session starts")
	}
	p := Presence{
		Type: PresenceCollection, Run: w.Generator.Run, Device: w.Generator.Device,
		LeaseUntil: formatTime(until), HandoffFrom: from, UpdatedAt: formatTime(w.now()),
	}
	body := map[string]any{"space": w.Space, "repo": w.me(), "collection": PresenceCollection, "rkey": SelfRkey, "record": p}
	if err := w.Client.Session.Post(ctx, "com.atproto.space.putRecord", body, nil); err != nil {
		return fmt.Errorf("writing presence: %w", err)
	}
	return nil
}

func expired(until string, now time.Time) bool {
	t, ok := parseTime(until)
	return !ok || !now.Before(t)
}

// Acquire takes the agent's lease for d. If another run holds a live
// lease, it fails with a *LeaseHeldError, unless takeover is set: then it
// starts a handoff and returns LeasePending.
func (w *Workspace) Acquire(ctx context.Context, d time.Duration, takeover bool) (LeaseState, error) {
	p, err := w.Presence(ctx)
	if err != nil {
		return 0, err
	}
	now := w.now()
	if p == nil || p.Run == w.Generator.Run || expired(p.LeaseUntil, now) {
		if err := w.putPresence(ctx, now.Add(d), nil); err != nil {
			return 0, err
		}
		p2, err := w.Presence(ctx)
		if err != nil {
			return 0, err
		}
		if p2 == nil || p2.Run != w.Generator.Run {
			if p2 == nil {
				return 0, ErrNotActive
			}
			return 0, &LeaseHeldError{Presence: *p2}
		}
		return LeaseActive, nil
	}
	if !takeover {
		return 0, &LeaseHeldError{Presence: *p}
	}
	from := &RunRef{Run: p.Run, Device: p.Device, LeaseUntil: p.LeaseUntil}
	if err := w.putPresence(ctx, now.Add(d), from); err != nil {
		return 0, err
	}
	ok, err := w.Active(ctx)
	if err != nil {
		return 0, err
	}
	if ok {
		return LeaseActive, nil
	}
	return LeasePending, nil
}

// Active reports whether this run holds the lease and may act: presence
// names it, its lease is live, and any run it took over from has yielded
// or seen its lease end. A run that finds it isn't active must stop, and
// call Yield.
func (w *Workspace) Active(ctx context.Context) (bool, error) {
	p, err := w.Presence(ctx)
	if err != nil || p == nil {
		return false, err
	}
	now := w.now()
	if p.Run != w.Generator.Run || expired(p.LeaseUntil, now) {
		return false, nil
	}
	if p.HandoffFrom == nil || expired(p.HandoffFrom.LeaseUntil, now) {
		return true, nil
	}
	return w.yielded(ctx, p.HandoffFrom.Run)
}

// yielded reports whether the run "from" wrote a handoff to this run.
func (w *Workspace) yielded(ctx context.Context, from string) (bool, error) {
	rec, err := w.read(ctx, w.me(), HandoffCollection, w.Generator.Run)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var h Handoff
	if err := rec.Decode(&h); err != nil {
		return false, nil
	}
	return h.FromRun == from && h.ToRun == w.Generator.Run, nil
}

// Renew extends this run's lease to d from now.
func (w *Workspace) Renew(ctx context.Context, d time.Duration) error {
	p, err := w.Presence(ctx)
	if err != nil {
		return err
	}
	if p == nil || p.Run != w.Generator.Run || expired(p.LeaseUntil, w.now()) {
		return ErrNotActive
	}
	from := p.HandoffFrom
	if from != nil {
		// Keep a handoff in progress; drop one that's settled.
		if ok, err := w.Active(ctx); err != nil {
			return err
		} else if ok {
			from = nil
		}
	}
	return w.putPresence(ctx, w.now().Add(d), from)
}

// Yield acknowledges that another run took over from this one, by writing
// a handoff record keyed by the new run, so it can start at once. It
// reports whether there was a handoff to acknowledge. Call it after
// stopping work, having written anything the new run needs.
func (w *Workspace) Yield(ctx context.Context) (bool, error) {
	p, err := w.Presence(ctx)
	if err != nil || p == nil {
		return false, err
	}
	if p.Run == w.Generator.Run || p.HandoffFrom == nil || p.HandoffFrom.Run != w.Generator.Run {
		return false, nil
	}
	g, err := w.generator()
	if err != nil {
		return false, err
	}
	h := Handoff{Type: HandoffCollection, FromRun: w.Generator.Run, ToRun: p.Run, Generator: g, CreatedAt: formatTime(w.now())}
	if _, err := w.create(ctx, HandoffCollection, p.Run, h); err != nil && !isXRPCError(err, "RecordAlreadyExists") {
		return false, err
	}
	return true, nil
}
