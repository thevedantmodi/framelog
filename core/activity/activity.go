// Package activity tracks what the daemon is doing right now — which phase of
// the card → library → backup pipeline is running, how far through it is, and
// which file it is on — so the status IPC command can report live progress
// instead of a bare "ingest_running: true".
//
// A Tracker is a tiny mutex-guarded struct, never held across I/O. That keeps
// the PROTOCOL.md §3 rule intact: status must never block behind a slow
// ingest. Every method is nil-receiver safe so packages can leave their
// Tracker field unset (tests, or callers that don't care about progress)
// without nil checks at every call site.
package activity

import (
	"sync"
	"time"
)

// Phase is the closed set of activities reported on the wire (PROTOCOL.md §3).
type Phase string

const (
	PhaseIdle      Phase = "idle"
	PhaseCopying   Phase = "copying"    // card → inbox/
	PhaseImporting Phase = "importing"  // inbox/ → originals/ + catalog
	PhaseBackingUp Phase = "backing_up" // originals/ → backup drive
	PhaseVerifying Phase = "verifying"  // card ⇄ library ⇄ backup hash check
)

// Snapshot is a point-in-time copy of the tracker, shaped for the wire.
// Total == 0 means "unknown" (the frontend shows an indeterminate state).
type Snapshot struct {
	Phase       Phase  `json:"phase"`
	Volume      string `json:"volume,omitempty"`
	Done        int    `json:"done"`
	Total       int    `json:"total"`
	CurrentFile string `json:"current_file,omitempty"`
	StartedAt   string `json:"started_at,omitempty"` // RFC 3339, UTC
}

// Tracker holds the current Snapshot. The zero value is ready to use and
// reports PhaseIdle.
type Tracker struct {
	mu sync.Mutex
	s  Snapshot
	// now is overridable in tests; nil means time.Now.
	now func() time.Time
}

func (t *Tracker) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// Start enters phase with a fresh counter. volume is the card's volume name
// when the phase belongs to a card run ("" otherwise). total may be 0 when
// the size of the work is not known up front.
func (t *Tracker) Start(phase Phase, volume string, total int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.s = Snapshot{
		Phase:     phase,
		Volume:    volume,
		Total:     total,
		StartedAt: t.clock().UTC().Format(time.RFC3339),
	}
}

// Step records progress: done is the number of items finished so far and
// file the item just handled (or now being handled).
func (t *Tracker) Step(done int, file string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.s.Done = done
	t.s.CurrentFile = file
	// A producer whose total was unknown (or an under-estimate, e.g. rclone
	// copying more than the inventory predicted) must never report done > total.
	if t.s.Total > 0 && done > t.s.Total {
		t.s.Total = done
	}
}

// SetTotal updates the expected item count without resetting progress —
// for producers that only learn the size of the work after starting.
func (t *Tracker) SetTotal(total int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.s.Total = total
}

// Idle returns the tracker to PhaseIdle.
func (t *Tracker) Idle() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.s = Snapshot{Phase: PhaseIdle}
}

// Snapshot returns a copy of the current state. A nil or never-started
// tracker reports PhaseIdle.
func (t *Tracker) Snapshot() Snapshot {
	if t == nil {
		return Snapshot{Phase: PhaseIdle}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.s
	if s.Phase == "" {
		s.Phase = PhaseIdle
	}
	return s
}
