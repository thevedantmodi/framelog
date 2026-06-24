package xmp

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// liValues walks the XML in data and returns all character-data values found
// inside <rdf:li> elements. Using a token walk rather than a typed struct so
// the test doesn't have to replicate the full namespace hierarchy.
func liValues(t *testing.T, data []byte) []string {
	t.Helper()
	var items []string
	d := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := d.Token()
		if err != nil {
			break // io.EOF or end of document
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "li" {
			continue
		}
		var v string
		if err := d.DecodeElement(&v, &se); err != nil {
			t.Fatalf("decode <rdf:li>: %v", err)
		}
		items = append(items, v)
	}
	return items
}

// containsAll reports whether every want string is present in got.
func containsAll(got []string, want ...string) bool {
outer:
	for _, w := range want {
		for _, g := range got {
			if g == w {
				continue outer
			}
		}
		return false
	}
	return true
}

func TestWriteXMP_FullMetadata(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "abc123.cr3")
	model := "X-T5"

	sidecar, err := WriteXMP(image, "batch20260622", &model)
	if err != nil {
		t.Fatalf("WriteXMP: %v", err)
	}

	data, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}

	items := liValues(t, data)
	if !containsAll(items, "batch:batch20260622", "camera:X-T5") {
		t.Errorf("rdf:li values = %v, want [batch:batch20260622 camera:X-T5]", items)
	}
}

func TestWriteXMP_NilCameraModel(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "photo.raf")

	sidecar, err := WriteXMP(image, "b1", nil)
	if err != nil {
		t.Fatalf("WriteXMP: %v", err)
	}

	data, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}

	items := liValues(t, data)
	if !containsAll(items, "camera:unknown") {
		t.Errorf("rdf:li values = %v, want camera:unknown when cameraModel is nil", items)
	}
}

func TestWriteXMP_Overwrite(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "photo.dng")
	model := "GFX100S"

	if _, err := WriteXMP(image, "first", &model); err != nil {
		t.Fatalf("first WriteXMP: %v", err)
	}
	sidecar, err := WriteXMP(image, "second", &model)
	if err != nil {
		t.Fatalf("second WriteXMP: %v", err)
	}

	data, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}

	items := liValues(t, data)
	if containsAll(items, "batch:first") {
		t.Error("sidecar still contains first batchID after overwrite")
	}
	if !containsAll(items, "batch:second") {
		t.Errorf("rdf:li values = %v, want batch:second after overwrite", items)
	}
}

func TestWriteXMP_SidecarPath(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		image string
		want  string
	}{
		{filepath.Join(dir, "abc123.CR3"), filepath.Join(dir, "abc123.xmp")},
		{filepath.Join(dir, "abc123.raf"), filepath.Join(dir, "abc123.xmp")},
		{filepath.Join(dir, "abc123.RAF"), filepath.Join(dir, "abc123.xmp")},
	}

	for _, tc := range cases {
		got, err := WriteXMP(tc.image, "b", nil)
		if err != nil {
			t.Fatalf("WriteXMP(%q): %v", tc.image, err)
		}
		if got != tc.want {
			t.Errorf("sidecar path = %q, want %q", got, tc.want)
		}
	}
}

// TestWriteXMP_NoCRSNamespace locks in the PROTOCOL.md §6 decision: the crs:
// (camera-raw-settings) namespace must not appear anywhere in the output. The
// Python predecessor registered it and never used it; this assertion prevents
// that from happening again.
func TestWriteXMP_NoCRSNamespace(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "photo.jpg")
	model := "X100VI"

	sidecar, err := WriteXMP(image, "b", &model)
	if err != nil {
		t.Fatalf("WriteXMP: %v", err)
	}

	data, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}

	raw := string(data)
	if strings.Contains(raw, "crs:") {
		t.Error("sidecar contains \"crs:\" prefix — crs namespace is out of scope for v1 (PROTOCOL.md §6)")
	}
	if strings.Contains(raw, "camera-raw-settings") {
		t.Error("sidecar contains \"camera-raw-settings\" — crs namespace is out of scope for v1 (PROTOCOL.md §6)")
	}
}
