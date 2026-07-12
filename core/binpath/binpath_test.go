package binpath

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFind_CandidateExists(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fakebin")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Find([]string{fake}, "fakebin", errors.New("not found"))
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got != fake {
		t.Errorf("Find = %q, want %q", got, fake)
	}
}

func TestFind_FallsBackToLookPath(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fakebin")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	got, err := Find([]string{"/nonexistent/fakebin"}, "fakebin", errors.New("not found"))
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got != fake {
		t.Errorf("Find = %q, want %q", got, fake)
	}
}

func TestFind_NoneFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir, nothing resolves

	notFound := errors.New("fakebin not found. Install it with: brew install fakebin")
	_, err := Find([]string{"/nonexistent/fakebin"}, "fakebin", notFound)
	if !errors.Is(err, notFound) {
		t.Errorf("Find error = %v, want %v", err, notFound)
	}
}
