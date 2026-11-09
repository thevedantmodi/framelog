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

func TestQuickHash(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	big := make([]byte, 3*QuickChunk)
	for i := range big {
		big[i] = byte(i * 7)
	}
	a := write("a", big)

	// Same bytes → same fingerprint.
	b := write("b", append([]byte(nil), big...))
	ha, err := QuickHash(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := QuickHash(b)
	if ha != hb {
		t.Error("identical files differ")
	}

	// A change in the head or tail is caught.
	head := append([]byte(nil), big...)
	head[10] ^= 1
	tail := append([]byte(nil), big...)
	tail[len(tail)-10] ^= 1
	for name, body := range map[string][]byte{"head": head, "tail": tail, "short": big[:len(big)-1]} {
		h, _ := QuickHash(write(name, body))
		if h == ha {
			t.Errorf("%s change not detected", name)
		}
	}

	// A change only in the unread middle is NOT caught — that is the
	// documented trade-off, and why verification uses HashFile.
	mid := append([]byte(nil), big...)
	mid[len(mid)/2] ^= 1
	if h, _ := QuickHash(write("mid", mid)); h != ha {
		t.Error("middle bytes should not affect QuickHash")
	}

	// Small files are hashed whole.
	s1, _ := QuickHash(write("s1", []byte("hello")))
	s2, _ := QuickHash(write("s2", []byte("hellp")))
	if s1 == s2 {
		t.Error("small files with different content collide")
	}

	if _, err := QuickHash(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file returned no error")
	}
}
