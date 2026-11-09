package sim

import (
	"os"
	"strings"
	"testing"
)

// TestSimulation runs every scenario against the fake tools — the same run
// `make sim` narrates — so CI exercises the whole card → library → backup →
// verify path, including the IPC socket, on every push.
func TestSimulation(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end simulation")
	}
	var out strings.Builder
	sum, err := Run(Options{Dir: t.TempDir(), Out: &out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sum.Results) != len(scenarios) {
		t.Fatalf("ran %d of %d scenarios", len(sum.Results), len(scenarios))
	}
	for _, r := range sum.Results {
		if !r.Passed {
			t.Errorf("scenario %s failed: %v", r.ID, r.Failures)
		}
	}
	if t.Failed() || testing.Verbose() {
		t.Log("\n" + out.String())
	}
	if entries, _ := os.ReadDir(sum.ReportsDir); len(entries) == 0 {
		t.Error("no card receipts written")
	}
}

func TestOnly_RunsPrerequisitesThenStops(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end simulation")
	}
	sum, err := Run(Options{Dir: t.TempDir(), Only: []string{"same-card-again"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Results) != 2 || sum.Results[1].ID != "same-card-again" || !sum.Passed() {
		t.Errorf("results = %+v", sum.Results)
	}
}
