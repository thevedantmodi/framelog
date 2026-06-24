package hasher

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "photo.raf")
	data := []byte("fake raw bytes")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}

	want := sha256.Sum256(data)
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("HashFile() = %q, want %q", got, hex.EncodeToString(want[:]))
	}
}

func TestHashFile_MissingFile(t *testing.T) {
	if _, err := HashFile("/nonexistent/path.raf"); err == nil {
		t.Error("expected an error for a missing file, got nil")
	}
}
