package exif

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeExiftool writes a shell script to dir/exiftool that executes body
// when invoked and makes it executable. Returns its absolute path. Tests pass
// this directly to ReadExif — no PATH modification needed, no real exiftool
// required.
func writeFakeExiftool(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "exiftool")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writeFakeExiftool: %v", err)
	}
	return p
}

// writePhoto creates a dummy file in dir that ReadExif can be pointed at.
func writePhoto(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "photo.raf")
	if err := os.WriteFile(p, []byte("fake raw"), 0o644); err != nil {
		t.Fatalf("writePhoto: %v", err)
	}
	return p
}

func TestReadExif_FullMetadata(t *testing.T) {
	dir := t.TempDir()
	photo := writePhoto(t, dir)

	fake := writeFakeExiftool(t, dir, `echo '[{"Model":"X-T5","DateTimeOriginal":"2026:06:01 12:30:00","GPSLatitude":37.7749,"GPSLongitude":-122.4194}]'`)

	meta, err := ReadExif(photo, fake)
	if err != nil {
		t.Fatalf("ReadExif: %v", err)
	}

	if meta.CaptureDate != "2026:06:01 12:30:00" {
		t.Errorf("CaptureDate = %q, want %q", meta.CaptureDate, "2026:06:01 12:30:00")
	}
	if meta.CameraModel == nil || *meta.CameraModel != "X-T5" {
		t.Errorf("CameraModel = %v, want \"X-T5\"", meta.CameraModel)
	}
	if meta.GPSLat == nil || *meta.GPSLat != 37.7749 {
		t.Errorf("GPSLat = %v, want 37.7749", meta.GPSLat)
	}
	if meta.GPSLon == nil || *meta.GPSLon != -122.4194 {
		t.Errorf("GPSLon = %v, want -122.4194", meta.GPSLon)
	}
}

func TestReadExif_MtimeFallback(t *testing.T) {
	dir := t.TempDir()
	photo := writePhoto(t, dir)

	// No DateTimeOriginal in output — CaptureDate must fall back to file mtime.
	fake := writeFakeExiftool(t, dir, `echo '[{"Model":"X-T5"}]'`)

	fi, err := os.Stat(photo)
	if err != nil {
		t.Fatal(err)
	}
	wantDate := fi.ModTime().Format(ExifTimeFormat)

	meta, err := ReadExif(photo, fake)
	if err != nil {
		t.Fatalf("ReadExif: %v", err)
	}
	if meta.CaptureDate != wantDate {
		t.Errorf("CaptureDate = %q, want mtime %q", meta.CaptureDate, wantDate)
	}
}

func TestReadExif_GPSAbsent(t *testing.T) {
	dir := t.TempDir()
	photo := writePhoto(t, dir)

	// JSON has no GPS fields at all — both must be nil, not 0.0.
	fake := writeFakeExiftool(t, dir, `echo '[{"Model":"X-T5","DateTimeOriginal":"2026:06:01 09:00:00"}]'`)

	meta, err := ReadExif(photo, fake)
	if err != nil {
		t.Fatalf("ReadExif: %v", err)
	}
	if meta.GPSLat != nil {
		t.Errorf("GPSLat = %v, want nil", meta.GPSLat)
	}
	if meta.GPSLon != nil {
		t.Errorf("GPSLon = %v, want nil", meta.GPSLon)
	}
}

func TestReadExif_ProcessFails(t *testing.T) {
	dir := t.TempDir()
	photo := writePhoto(t, dir)

	// Script exits non-zero and writes to stderr.
	fake := writeFakeExiftool(t, dir, `echo "bad file: permission denied" >&2; exit 1`)

	_, err := ReadExif(photo, fake)
	if err == nil {
		t.Fatal("expected error for non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "bad file: permission denied") {
		t.Errorf("error %q does not contain stderr output", err.Error())
	}
}

// TestFindExiftool_LookPathFallback tests the exec.LookPath branch of
// FindExiftool by replacing candidatePaths with nonexistent paths so the
// function is forced to fall through to LookPath. A fake exiftool binary is
// placed in a t.TempDir() that is prepended to PATH.
func TestFindExiftool_LookPathFallback(t *testing.T) {
	dir := t.TempDir()
	fake := writeFakeExiftool(t, dir, `echo "fake"`)
	_ = fake

	// Replace candidate list with paths that won't exist so LookPath is
	// the only remaining resolution path.
	orig := candidatePaths
	candidatePaths = []string{"/nonexistent/a", "/nonexistent/b"}
	t.Cleanup(func() { candidatePaths = orig })

	// Prepend our temp dir to PATH so exec.LookPath finds the fake binary.
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+":"+origPath)

	got, err := FindExiftool()
	if err != nil {
		t.Fatalf("FindExiftool: %v", err)
	}
	if got == "" {
		t.Error("FindExiftool returned empty path, want path to fake exiftool")
	}
}

func TestFindExiftool_NoneFound(t *testing.T) {
	orig := candidatePaths
	candidatePaths = []string{"/nonexistent/a", "/nonexistent/b"}
	t.Cleanup(func() { candidatePaths = orig })

	// PATH with no exiftool anywhere.
	t.Setenv("PATH", t.TempDir()) // empty dir, no exiftool

	_, err := FindExiftool()
	if err == nil {
		t.Fatal("expected error when exiftool is not found, got nil")
	}
	if !strings.Contains(err.Error(), "brew install exiftool") {
		t.Errorf("error %q does not contain actionable install hint", err.Error())
	}
}
