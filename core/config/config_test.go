package config

import (
	"errors"
	"testing"
)

// restoreImports resets the global import set after a test that changes it.
func restoreImports(t *testing.T) {
	t.Helper()
	t.Cleanup(resetImportExtensions)
}

// Every importable type must be filed by outgest too — otherwise a format
// added to the import list would have its exports ignored.
func TestOutgest_SupersetOfImport(t *testing.T) {
	restoreImports(t)
	if err := SetImportExtensions([]string{".arw", ".insv"}); err != nil {
		t.Fatal(err)
	}
	for _, ext := range ImportExtensions() {
		if !IsOutgestExtension(ext) {
			t.Errorf("IsOutgestExtension(%q) = false for an importable type", ext)
		}
	}
}

// PNG is the case that motivated the split: exported, never imported.
func TestPNG_ExportOnly(t *testing.T) {
	if !IsOutgestExtension(".png") {
		t.Error("PNG exports must be filed by outgest")
	}
	if IsImportable(".png") {
		t.Error("PNG must not be imported")
	}
	restoreImports(t)
	if err := SetImportExtensions([]string{".png"}); err == nil {
		t.Error("SetImportExtensions allowed .png")
	}
}

// Switching JPEG import off must not stop Lightroom JPEG exports being filed.
func TestOutgest_IndependentOfDisabledImports(t *testing.T) {
	restoreImports(t)
	if err := SetImportExtensions([]string{".arw"}); err != nil {
		t.Fatal(err)
	}
	if IsImportable(".jpg") {
		t.Fatal("jpg still importable")
	}
	if !IsOutgestExtension(".JPG") {
		t.Error("JPEG exports stopped being filed after JPEG import was switched off")
	}
}

func TestDefaults_AllKnownTypesOn(t *testing.T) {
	for _, mt := range KnownMediaTypes {
		if !IsImportable(mt.Ext) {
			t.Errorf("known type %s is off by default — it would be dropped without the user choosing", mt.Ext)
		}
	}
	for _, ext := range []string{".hif", ".HIF", ".arw", ".mp4"} {
		if !IsImportable(ext) {
			t.Errorf("IsImportable(%q) = false", ext)
		}
	}
	if IsImportable(".xml") || IsImportable(".xmp") {
		t.Error("sidecars must never be importable")
	}
}

func TestSetImportExtensions_Validation(t *testing.T) {
	restoreImports(t)
	bad := [][]string{{}, {"../x"}, {".toolongextension"}, {".xml"}, {"xmp"}, {". arw"}}
	for _, b := range bad {
		if err := SetImportExtensions(b); err == nil {
			t.Errorf("SetImportExtensions(%q) accepted", b)
		}
	}
	// A rejected set must leave the current one untouched.
	if !IsImportable(".arw") {
		t.Error("failed Set changed the import set")
	}

	if err := SetImportExtensions([]string{"ARW", " .Mp4 "}); err != nil {
		t.Fatal(err)
	}
	if got := ImportExtensions(); len(got) != 2 || got[0] != ".arw" || got[1] != ".mp4" {
		t.Errorf("ImportExtensions = %v", got)
	}

	if err := SetImportExtensions(nil); err != nil {
		t.Fatal(err)
	}
	if len(ImportExtensions()) != len(KnownMediaTypes) {
		t.Error("nil did not restore defaults")
	}
}

func TestFileTypeSettings(t *testing.T) {
	restoreImports(t)
	if err := SetImportExtensions([]string{".arw", ".insv"}); err != nil {
		t.Fatal(err)
	}
	s := FileTypeSettings()
	if len(s) != len(KnownMediaTypes)+1 {
		t.Fatalf("len = %d", len(s))
	}
	if s[0].Ext != ".arw" || !s[0].Enabled || s[0].Label != "Sony RAW" {
		t.Errorf("first = %+v", s[0])
	}
	for _, row := range s {
		if row.Ext == ".jpg" && row.Enabled {
			t.Error(".jpg should be off")
		}
	}
	last := s[len(s)-1]
	if last.Ext != ".insv" || !last.Custom || !last.Enabled {
		t.Errorf("custom row = %+v", last)
	}
}

func TestNormalizeExtension(t *testing.T) {
	for in, want := range map[string]string{"ARW": ".arw", ".Hif": ".hif", " mp4 ": ".mp4", "": ""} {
		if got := NormalizeExtension(in); got != want {
			t.Errorf("NormalizeExtension(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateImportExtensions(t *testing.T) {
	got, err := ValidateImportExtensions([]string{"MP4", ".arw", "arw"})
	if err != nil || len(got) != 2 || got[0] != ".arw" || got[1] != ".mp4" {
		t.Errorf("Validate = %v, %v", got, err)
	}
	if _, err := ValidateImportExtensions([]string{".xmp"}); !errors.Is(err, ErrInvalidFileType) {
		t.Errorf("err = %v, want ErrInvalidFileType", err)
	}
}
