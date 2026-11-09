package activity

import (
	"sync"
	"testing"
	"time"
)

func TestZeroValueIsIdle(t *testing.T) {
	var tr Tracker
	if got := tr.Snapshot().Phase; got != PhaseIdle {
		t.Fatalf("zero Tracker phase = %q, want idle", got)
	}
}

func TestNilTrackerIsSafe(t *testing.T) {
	var tr *Tracker
	tr.Start(PhaseCopying, "X", 3)
	tr.Step(1, "a")
	tr.Idle()
	if got := tr.Snapshot().Phase; got != PhaseIdle {
		t.Fatalf("nil Tracker phase = %q, want idle", got)
	}
}

func TestStartStepIdle(t *testing.T) {
	fixed := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	tr := &Tracker{now: func() time.Time { return fixed }}

	tr.Start(PhaseCopying, "Untitled", 10)
	tr.Step(3, "DSC00003.ARW")
	s := tr.Snapshot()
	want := Snapshot{Phase: PhaseCopying, Volume: "Untitled", Done: 3, Total: 10,
		CurrentFile: "DSC00003.ARW", StartedAt: "2026-11-07T10:00:00Z"}
	if s != want {
		t.Fatalf("snapshot = %+v, want %+v", s, want)
	}

	// A new phase resets the counter.
	tr.Start(PhaseImporting, "", 4)
	if s := tr.Snapshot(); s.Done != 0 || s.CurrentFile != "" || s.Total != 4 {
		t.Fatalf("Start did not reset: %+v", s)
	}

	tr.Idle()
	if s := tr.Snapshot(); s != (Snapshot{Phase: PhaseIdle}) {
		t.Fatalf("after Idle = %+v", s)
	}
}

func TestStepNeverExceedsTotal(t *testing.T) {
	var tr Tracker
	tr.Start(PhaseCopying, "", 2)
	tr.Step(5, "x")
	if s := tr.Snapshot(); s.Total != 5 {
		t.Fatalf("Total = %d, want raised to 5", s.Total)
	}
	// Unknown total (0) stays unknown.
	tr.Start(PhaseBackingUp, "", 0)
	tr.Step(7, "y")
	if s := tr.Snapshot(); s.Total != 0 {
		t.Fatalf("unknown Total became %d", s.Total)
	}
}

func TestConcurrentUse(t *testing.T) {
	var tr Tracker
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				tr.Start(PhaseImporting, "", 200)
				tr.Step(j, "f")
				_ = tr.Snapshot()
			}
		}(i)
	}
	wg.Wait()
}

func TestSetTotalKeepsProgress(t *testing.T) {
	var tr Tracker
	tr.Start(PhaseVerifying, "Untitled", 0)
	tr.Step(2, "a")
	started := tr.Snapshot().StartedAt
	tr.SetTotal(9)
	s := tr.Snapshot()
	if s.Total != 9 || s.Done != 2 || s.StartedAt != started || s.Volume != "Untitled" {
		t.Fatalf("SetTotal clobbered state: %+v", s)
	}
}
