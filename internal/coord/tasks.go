package coord

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Task statuses.
const (
	StatusOpen             = "open"
	StatusClaimed          = "claimed"
	StatusBlocked          = "blocked"
	StatusDone             = "done"
	StatusAwaitingApproval = "awaitingApproval"
)

var (
	// ErrNotClaimable: the task is held by another agent, done, waiting
	// for the operator's approval, or assigned to someone else.
	ErrNotClaimable = errors.New("task can't be claimed")
	// ErrClaimLost: an earlier claim won the race.
	ErrClaimLost = errors.New("another agent claimed the task first")
	// ErrNotHolder: only the agent holding a task can report on it.
	ErrNotHolder = errors.New("this agent doesn't hold the task")
)

// Event is a task event with where it came from.
type Event struct {
	Author string
	Rkey   string
	URI    string
	CID    string
	TaskEvent
}

// TaskState is a task's state, computed from its events.
type TaskState struct {
	Status string
	// Holder and LeaseUntil: who holds a claimed task, and until when.
	Holder     string
	LeaseUntil time.Time
	// Note is the latest note from the holder (e.g. why it's blocked).
	Note string
	// DoneBy and Output: who finished it, with what.
	DoneBy string
	Output *StrongRef
	// Approved: the operator approved a task that requires it.
	Approved bool
	Events   []Event
}

// eventTime is when an event was written: its record key's TID, which
// the PDS assigns, falling back to its createdAt.
func eventTime(e Event) time.Time {
	if tid, err := syntax.ParseTID(e.Rkey); err == nil {
		return tid.Time()
	}
	t, _ := parseTime(e.CreatedAt)
	return t
}

func tidAt(t time.Time, clock uint) string { return syntax.NewTIDFromTime(t, clock).String() }

// Fold computes a task's state from its events, as of now. Events are
// ordered by record key, then author DID. A claim takes the task unless
// another agent's lease is live at the claim's time; only the holder's
// progress (which may extend the lease), release, block and done count,
// and only while its lease is live. On a task requiring approval, claims
// count only after the operator's approve; on an assigned task, only the
// assignee's claims count.
func Fold(task Task, operator string, events []Event, now time.Time) TaskState {
	evs := append([]Event(nil), events...)
	sort.SliceStable(evs, func(i, j int) bool {
		if evs[i].Rkey != evs[j].Rkey {
			return evs[i].Rkey < evs[j].Rkey
		}
		return evs[i].Author < evs[j].Author
	})
	st := TaskState{Events: evs, Approved: !task.RequiresApproval}
	var holder string
	var lease time.Time
	var done, blocked bool
	for _, e := range evs {
		at := eventTime(e)
		live := holder != "" && at.Before(lease)
		holding := live && holder == e.Author
		switch e.Event {
		case EventApprove:
			if e.Author == operator {
				st.Approved = true
			}
		case EventClaim:
			if done || !st.Approved || (task.Assignee != "" && e.Author != task.Assignee) {
				continue
			}
			if live && holder != e.Author {
				continue // lost to the live holder
			}
			until, ok := parseTime(e.LeaseUntil)
			if !ok || !at.Before(until) {
				continue // a claim without a lease holds nothing
			}
			holder, lease, blocked = e.Author, until, false
			st.Note = e.Note
		case EventProgress:
			if !holding {
				continue
			}
			if until, ok := parseTime(e.LeaseUntil); ok && until.After(lease) {
				lease = until
			}
			st.Note = e.Note
		case EventRelease:
			if holding {
				holder, st.Note = "", e.Note
			}
		case EventBlock:
			if holding {
				holder, blocked, st.Note = "", true, e.Note
			}
		case EventDone:
			if holding && !done {
				done, holder = true, ""
				st.DoneBy, st.Output, st.Note = e.Author, e.Output, e.Note
			}
		}
	}
	switch {
	case done:
		st.Status = StatusDone
	case holder != "" && now.Before(lease):
		st.Status, st.Holder, st.LeaseUntil = StatusClaimed, holder, lease
	case !st.Approved:
		st.Status = StatusAwaitingApproval
	case blocked:
		st.Status = StatusBlocked
	default:
		st.Status = StatusOpen
	}
	return st
}

func (w *Workspace) getTask(ctx context.Context, ref StrongRef) (Task, error) {
	rec, err := w.Get(ctx, ref)
	if err != nil {
		return Task{}, err
	}
	if rec.Collection != TaskCollection {
		return Task{}, fmt.Errorf("%s isn't a task", ref.URI)
	}
	var t Task
	if err := rec.Decode(&t); err != nil {
		return Task{}, fmt.Errorf("reading task: %w", err)
	}
	return t, nil
}

// Events lists every member's events for a task version.
func (w *Workspace) Events(ctx context.Context, task StrongRef) ([]Event, error) {
	recs, err := w.List(ctx, TaskEventCollection)
	if err != nil {
		return nil, err
	}
	var evs []Event
	for _, r := range recs {
		var te TaskEvent
		if err := r.Decode(&te); err != nil || te.Task != task {
			continue
		}
		evs = append(evs, Event{Author: r.Author, Rkey: r.Rkey, URI: r.URI, CID: r.CID, TaskEvent: te})
	}
	return evs, nil
}

// TaskState reads a task and its events and computes its state.
func (w *Workspace) TaskState(ctx context.Context, task StrongRef) (TaskState, error) {
	t, err := w.getTask(ctx, task)
	if err != nil {
		return TaskState{}, err
	}
	return w.taskState(ctx, task, t)
}

func (w *Workspace) taskState(ctx context.Context, ref StrongRef, t Task) (TaskState, error) {
	op, err := w.Operator()
	if err != nil {
		return TaskState{}, err
	}
	evs, err := w.Events(ctx, ref)
	if err != nil {
		return TaskState{}, err
	}
	return Fold(t, op, evs, w.now()), nil
}

func (w *Workspace) writeEvent(ctx context.Context, e TaskEvent) (StrongRef, error) {
	g, err := w.generator()
	if err != nil {
		return StrongRef{}, err
	}
	e.Type, e.Generator, e.CreatedAt = TaskEventCollection, g, formatTime(w.now())
	return w.create(ctx, TaskEventCollection, "", e)
}

// Claim takes a task for lease (claiming a task this agent already holds
// renews the lease). It writes a claim, reads the task's events again,
// and fails with ErrClaimLost if an earlier claim won.
func (w *Workspace) Claim(ctx context.Context, task StrongRef, lease time.Duration) (TaskState, error) {
	if lease <= 0 {
		return TaskState{}, errors.New("a claim needs a lease")
	}
	t, err := w.getTask(ctx, task)
	if err != nil {
		return TaskState{}, err
	}
	st, err := w.taskState(ctx, task, t)
	if err != nil {
		return TaskState{}, err
	}
	me := w.me()
	switch {
	case t.Assignee != "" && t.Assignee != me:
		return st, fmt.Errorf("%w: assigned to %s", ErrNotClaimable, t.Assignee)
	case st.Status == StatusClaimed && st.Holder == me, st.Status == StatusOpen, st.Status == StatusBlocked:
	default:
		return st, fmt.Errorf("%w: %s", ErrNotClaimable, st.Status)
	}
	if _, err := w.writeEvent(ctx, TaskEvent{Task: task, Event: EventClaim, LeaseUntil: formatTime(w.now().Add(lease))}); err != nil {
		return st, err
	}
	st, err = w.taskState(ctx, task, t)
	if err != nil {
		return st, err
	}
	if st.Status != StatusClaimed || st.Holder != me {
		return st, ErrClaimLost
	}
	return st, nil
}

// holding checks this agent holds a task.
func (w *Workspace) holding(ctx context.Context, task StrongRef) error {
	st, err := w.TaskState(ctx, task)
	if err != nil {
		return err
	}
	if st.Status != StatusClaimed || st.Holder != w.me() {
		return ErrNotHolder
	}
	return nil
}

// Progress reports progress on a held task. A positive lease extends it.
func (w *Workspace) Progress(ctx context.Context, task StrongRef, note string, lease time.Duration) (StrongRef, error) {
	if err := w.holding(ctx, task); err != nil {
		return StrongRef{}, err
	}
	e := TaskEvent{Task: task, Event: EventProgress, Note: note}
	if lease > 0 {
		e.LeaseUntil = formatTime(w.now().Add(lease))
	}
	return w.writeEvent(ctx, e)
}

// Done finishes a held task, optionally pointing at the work.
func (w *Workspace) Done(ctx context.Context, task StrongRef, output *StrongRef, note string) (StrongRef, error) {
	if err := w.holding(ctx, task); err != nil {
		return StrongRef{}, err
	}
	if output != nil {
		if _, err := w.Get(ctx, *output); err != nil {
			return StrongRef{}, fmt.Errorf("output: %w", err)
		}
	}
	return w.writeEvent(ctx, TaskEvent{Task: task, Event: EventDone, Output: output, Note: note})
}

// Release gives up a held task.
func (w *Workspace) Release(ctx context.Context, task StrongRef, note string) (StrongRef, error) {
	if err := w.holding(ctx, task); err != nil {
		return StrongRef{}, err
	}
	return w.writeEvent(ctx, TaskEvent{Task: task, Event: EventRelease, Note: note})
}

// Block gives up a held task because it can't proceed, saying why.
func (w *Workspace) Block(ctx context.Context, task StrongRef, note string) (StrongRef, error) {
	if err := w.holding(ctx, task); err != nil {
		return StrongRef{}, err
	}
	return w.writeEvent(ctx, TaskEvent{Task: task, Event: EventBlock, Note: note})
}

// Approve approves a task. Only the operator's (the workspace authority's)
// approval counts.
func (w *Workspace) Approve(ctx context.Context, task StrongRef) (StrongRef, error) {
	if _, err := w.getTask(ctx, task); err != nil {
		return StrongRef{}, err
	}
	return w.writeEvent(ctx, TaskEvent{Task: task, Event: EventApprove})
}
