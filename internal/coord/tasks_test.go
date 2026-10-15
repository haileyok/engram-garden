package coord

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func (f *fixture) task(t *testing.T, tk Task) StrongRef {
	t.Helper()
	ref, err := f.operator.CreateTask(context.Background(), tk)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestClaimHoldsTheTask(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	task := f.task(t, Task{Title: "Find the cause of the Q3 churn rise"})
	st, err := f.researcher.Claim(ctx, task, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusClaimed || st.Holder != "did:plc:researcher" {
		t.Fatalf("state %+v", st)
	}
	if _, err := f.writer.Claim(ctx, task, time.Hour); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("second claim: %v", err)
	}
}

func TestClaimRaceHasOneWinner(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	for range 5 {
		task := f.task(t, Task{Title: "race"})
		var wg sync.WaitGroup
		results := make([]error, 2)
		for i, w := range []*Workspace{f.researcher, f.writer} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, results[i] = w.Claim(ctx, task, time.Hour)
			}()
		}
		wg.Wait()
		wins := 0
		for _, err := range results {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrClaimLost), errors.Is(err, ErrNotClaimable):
			default:
				t.Fatal(err)
			}
		}
		if wins != 1 {
			t.Fatalf("%d winners: %v", wins, results)
		}
		st, err := f.operator.TaskState(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != StatusClaimed {
			t.Fatalf("state %+v", st)
		}
	}
}

func TestExpiredLeaseLetsAnotherClaim(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	task := f.task(t, Task{Title: "t"})
	if _, err := f.researcher.Claim(ctx, task, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	st, err := f.writer.Claim(ctx, task, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st.Holder != "did:plc:writer" {
		t.Fatalf("state %+v", st)
	}
	// The old holder can no longer finish it.
	if _, err := f.researcher.Done(ctx, task, nil, "done"); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("done by old holder: %v", err)
	}
}

func TestDoneAndRelease(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	task := f.task(t, Task{Title: "t"})
	if _, err := f.researcher.Claim(ctx, task, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := f.researcher.Release(ctx, task, "can't do it"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.writer.Claim(ctx, task, time.Hour); err != nil {
		t.Fatal(err)
	}
	out := f.observe(t, f.writer, "result")
	if _, err := f.writer.Done(ctx, task, &out, ""); err != nil {
		t.Fatal(err)
	}
	st, err := f.operator.TaskState(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusDone || st.DoneBy != "did:plc:writer" || st.Output == nil || *st.Output != out {
		t.Fatalf("state %+v", st)
	}
	if _, err := f.researcher.Claim(ctx, task, time.Hour); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("claim after done: %v", err)
	}
}

func TestBlock(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	task := f.task(t, Task{Title: "t"})
	if _, err := f.researcher.Claim(ctx, task, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := f.researcher.Block(ctx, task, "need credentials"); err != nil {
		t.Fatal(err)
	}
	st, _ := f.operator.TaskState(ctx, task)
	if st.Status != StatusBlocked || st.Note != "need credentials" {
		t.Fatalf("state %+v", st)
	}
	// Someone can pick it up again.
	if _, err := f.writer.Claim(ctx, task, time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestAssigneeOnly(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	task := f.task(t, Task{Title: "t", Assignee: "did:plc:researcher"})
	if _, err := f.writer.Claim(ctx, task, time.Hour); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("claim by non-assignee: %v", err)
	}
	if _, err := f.researcher.Claim(ctx, task, time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalIsTheOperators(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	task := f.task(t, Task{Title: "Publish the summary", RequiresApproval: true})
	if _, err := f.researcher.Claim(ctx, task, time.Hour); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("claim before approval: %v", err)
	}
	// An agent's approve doesn't count.
	if _, err := f.writer.Approve(ctx, task); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.operator.TaskState(ctx, task); st.Status != StatusAwaitingApproval {
		t.Fatalf("state after agent approve %+v", st)
	}
	if _, err := f.operator.Approve(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := f.researcher.Claim(ctx, task, time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestFoldOrder(t *testing.T) {
	t.Parallel()
	task := StrongRef{URI: "at://x/task/1", CID: "c"}
	at := time.Date(2026, 10, 14, 16, 0, 0, 0, time.UTC)
	lease := at.Add(time.Hour).Format(timeFormat)
	ev := func(author, rkey, kind string) Event {
		return Event{Author: author, Rkey: rkey, TaskEvent: TaskEvent{Task: task, Event: kind, LeaseUntil: lease}}
	}
	t1 := tidAt(at, 1)
	t2 := tidAt(at.Add(time.Second), 1)
	// Out of order on input; the earlier TID wins.
	st := Fold(Task{}, "did:plc:op", []Event{ev("did:plc:a", t2, "claim"), ev("did:plc:b", t1, "claim")}, at.Add(time.Minute))
	if st.Holder != "did:plc:b" {
		t.Fatalf("holder %s", st.Holder)
	}
	// Same TID: the lower DID wins.
	st = Fold(Task{}, "did:plc:op", []Event{ev("did:plc:b", t1, "claim"), ev("did:plc:a", t1, "claim")}, at.Add(time.Minute))
	if st.Holder != "did:plc:a" {
		t.Fatalf("holder %s", st.Holder)
	}
	// After the lease, the task is open again.
	st = Fold(Task{}, "did:plc:op", []Event{ev("did:plc:a", t1, "claim")}, at.Add(2*time.Hour))
	if st.Status != StatusOpen {
		t.Fatalf("status %s", st.Status)
	}
	// A claim without a lease holds nothing.
	bad := ev("did:plc:a", t1, "claim")
	bad.LeaseUntil = ""
	if st := Fold(Task{}, "did:plc:op", []Event{bad}, at.Add(time.Minute)); st.Holder != "" {
		t.Fatalf("holder %s", st.Holder)
	}
}
