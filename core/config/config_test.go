package config

import "testing"

// TestOutgestExtensions_SupersetOfSupported enforces the invariant that every
// format ingest imports can also be filed by outgest. If someone adds a RAW
// format to SupportedExtensions and forgets outgest, exports of that format
// would silently pile up in processed/ root — exactly the failure this set
// was introduced to fix.
func TestOutgestExtensions_SupersetOfSupported(t *testing.T) {
	for ext := range SupportedExtensions {
		if !OutgestExtensions[ext] {
			t.Errorf("OutgestExtensions missing %q, which is in SupportedExtensions", ext)
		}
	}
}

// TestOutgestOnlyExtensions_NotIngested is the other half of the split: an
// export-only format must never leak into the ingest set, or PNGs and PSDs
// dropped in inbox/ would be imported into originals/ and git-tracked.
func TestOutgestOnlyExtensions_NotIngested(t *testing.T) {
	for ext := range outgestOnlyExtensions {
		if SupportedExtensions[ext] {
			t.Errorf("%q is in both outgestOnlyExtensions and SupportedExtensions", ext)
		}
		if !OutgestExtensions[ext] {
			t.Errorf("OutgestExtensions missing outgest-only extension %q", ext)
		}
	}
}

// TestOutgestExtensions_PNG pins the concrete case that motivated the split:
// a Lightroom PNG export must be outgest-eligible and ingest-ineligible.
func TestOutgestExtensions_PNG(t *testing.T) {
	if !OutgestExtensions[".png"] {
		t.Error("OutgestExtensions[.png] = false, want true")
	}
	if SupportedExtensions[".png"] {
		t.Error("SupportedExtensions[.png] = true, want false — PNGs must not be ingested")
	}
}
