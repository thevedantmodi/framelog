// Package xmp writes Adobe XMP sidecar files next to imported photos so
// Lightroom can read and update their metadata. The structure produced here
// matches the Python predecessor's xmp.py exactly -- same namespace URIs, same
// xpacket wrapper, same dc:subject keyword bag.
//
// crs: (camera-raw-settings) namespace: deliberately absent. PROTOCOL.md section 6
// resolved this as out-of-scope for v1 -- the Python version registered the
// namespace and never emitted a single element under it. It is not registered
// here, not commented out, not reserved. If a real feature ever needs it, add
// it then.
package xmp

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
)

// xpacketBegin is the UTF-8 BOM (U+FEFF), written as hex escapes so no raw
// BOM byte sequence appears in the Go source file itself (the compiler rejects
// a BOM anywhere other than the very first byte of a .go file).
const xpacketBegin = "\xef\xbb\xbf"

// WriteXMP writes an XMP sidecar next to imagePath and returns the sidecar
// path on success. The sidecar contains a dc:subject keyword bag with two
// entries: "batch:<batchID>" and "camera:<cameraModel>" (or "camera:unknown"
// when cameraModel is nil or empty). cameraModel is a pointer to match the
// nullable CameraModel field in db.Photo and exif.Metadata -- no third
// representation of "no camera model" is introduced here.
//
// Writing overwrites any existing sidecar unconditionally -- this is the
// intended behavior, not a gap to guard against.
func WriteXMP(imagePath, batchID string, cameraModel *string) (string, error) {
	model := "unknown"
	if cameraModel != nil && *cameraModel != "" {
		model = *cameraModel
	}

	var buf bytes.Buffer

	// xpacket begin/end are Adobe's proprietary XMP packet wrapper -- standard
	// XML processors ignore them as processing instructions, but Lightroom
	// requires them to recognize the file as a valid XMP sidecar.
	fmt.Fprintf(&buf, "<?xpacket begin=%q id=\"W5M0MpCehiHzreSzNTczkc9d\"?>\n", xpacketBegin)
	buf.WriteString("<x:xmpmeta xmlns:x=\"adobe:ns:meta/\">\n")
	buf.WriteString("  <rdf:RDF xmlns:rdf=\"http://www.w3.org/1999/02/22-rdf-syntax-ns#\">\n")
	buf.WriteString("    <rdf:Description rdf:about=\"\" xmlns:dc=\"http://purl.org/dc/elements/1.1/\">\n")
	buf.WriteString("      <dc:subject>\n")
	buf.WriteString("        <rdf:Bag>\n")
	fmt.Fprintf(&buf, "          <rdf:li>batch:%s</rdf:li>\n", xmlEscape(batchID))
	fmt.Fprintf(&buf, "          <rdf:li>camera:%s</rdf:li>\n", xmlEscape(model))
	buf.WriteString("        </rdf:Bag>\n")
	buf.WriteString("      </dc:subject>\n")
	buf.WriteString("    </rdf:Description>\n")
	buf.WriteString("  </rdf:RDF>\n")
	buf.WriteString("</x:xmpmeta>\n")
	buf.WriteString("<?xpacket end=\"w\"?>\n")

	out := sidecarPath(imagePath)
	if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
		return "", fmt.Errorf("xmp: write sidecar: %w", err)
	}
	return out, nil
}

// sidecarPath replaces the extension of imagePath with .xmp, preserving the
// directory and base name regardless of the original extension's case.
func sidecarPath(imagePath string) string {
	ext := filepath.Ext(imagePath)
	return imagePath[:len(imagePath)-len(ext)] + ".xmp"
}

// xmlEscape returns s with XML special characters escaped so keyword values
// containing <, >, &, " or ' don't produce malformed output.
func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s)) //nolint:errcheck // bytes.Buffer.Write never errors
	return b.String()
}
