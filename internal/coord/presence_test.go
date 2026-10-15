package coord

import (
	"context"
	"errors"
	"testing"
	"time"
)

// device is another run of the same agent, on another device.
func (f *fixture) device(t *testing.T, w *Workspace, device string) *Workspace {
	t.Helper()
	return &Workspace{Client: w.Client, Space: w.Space, Generator: Generator{Model: "test-model", Device: device, Run: NewRun()}}
}

func TestOneRunHoldsTheAgent(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	laptop := f.researcher
	phone := f.device(t, laptop, "phone")
	if _, err := laptop.Acquire(ctx, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if ok, err := laptop.Active(ctx); err != nil || !ok {
		t.Fatalf("laptop active=%v err=%v", ok, err)
	}
	var held *LeaseHeldError
	if _, err := phone.Acquire(ctx, time.Hour, false); !errors.As(err, &held) || held.Presence.Device != "laptop" {
		t.Fatalf("phone acquire: %v", err)
	}
	if ok, _ := phone.Active(ctx); ok {
		t.Fatal("phone active without the lease")
	}
	// Renewing keeps it.
	if err := laptop.Renew(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := phone.Renew(ctx, time.Hour); !errors.Is(err, ErrNotActive) {
		t.Fatalf("phone renew: %v", err)
	}
}

func TestHandoff(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	laptop := f.researcher
	phone := f.device(t, laptop, "phone")
	if _, err := laptop.Acquire(ctx, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	st, err := phone.Acquire(ctx, time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	if st != LeasePending {
		t.Fatalf("takeover state %v, want pending", st)
	}
	// Neither acts until the laptop yields: no overlap.
	if ok, _ := phone.Active(ctx); ok {
		t.Fatal("phone active before the laptop yielded")
	}
	if ok, _ := laptop.Active(ctx); ok {
		t.Fatal("laptop still active after the takeover")
	}
	yielded, err := laptop.Yield(ctx)
	if err != nil || !yielded {
		t.Fatalf("yield=%v err=%v", yielded, err)
	}
	if ok, err := phone.Active(ctx); err != nil || !ok {
		t.Fatalf("phone active=%v err=%v after the yield", ok, err)
	}
	// Yielding twice is harmless.
	if _, err := laptop.Yield(ctx); err != nil {
		t.Fatal(err)
	}
	// The laptop can't write any more once writes need the lease.
	laptop.RequireLease = true
	if _, err := laptop.Observe(ctx, Observation{Source: Source{Kind: "url", Value: "x"}, Method: "m", Text: "late"}); !errors.Is(err, ErrNotActive) {
		t.Fatalf("laptop write after handoff: %v", err)
	}
	phone.RequireLease = true
	if _, err := phone.Observe(ctx, Observation{Source: Source{Kind: "url", Value: "x"}, Method: "m", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffFromAnOfflineDeviceWaitsForTheLease(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	laptop := f.researcher
	phone := f.device(t, laptop, "phone")
	if _, err := laptop.Acquire(ctx, 80*time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	if st, err := phone.Acquire(ctx, time.Hour, true); err != nil || st != LeasePending {
		t.Fatalf("state %v err %v", st, err)
	}
	if ok, _ := phone.Active(ctx); ok {
		t.Fatal("active before the old lease ended")
	}
	time.Sleep(150 * time.Millisecond)
	if ok, err := phone.Active(ctx); err != nil || !ok {
		t.Fatalf("active=%v err=%v after the old lease ended", ok, err)
	}
	// The returning laptop finds it lost the lease.
	if ok, _ := laptop.Active(ctx); ok {
		t.Fatal("laptop active after returning")
	}
}

func TestExpiredLeaseCanBeTakenWithoutHandoff(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	laptop := f.researcher
	phone := f.device(t, laptop, "phone")
	if _, err := laptop.Acquire(ctx, 50*time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	st, err := phone.Acquire(ctx, time.Hour, false)
	if err != nil || st != LeaseActive {
		t.Fatalf("state %v err %v", st, err)
	}
}

func TestRequireLease(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	f.researcher.RequireLease = true
	obs := Observation{Source: Source{Kind: "url", Value: "x"}, Method: "m", Text: "t"}
	if _, err := f.researcher.Observe(ctx, obs); !errors.Is(err, ErrNotActive) {
		t.Fatalf("write without a lease: %v", err)
	}
	if _, err := f.researcher.Acquire(ctx, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.researcher.Observe(ctx, obs); err != nil {
		t.Fatal(err)
	}
}
