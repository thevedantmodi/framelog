package nrtmeta

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// sampleXML is a real Sony ILCE-7CM2 C####M01.XML, trimmed of nothing that
// matters: the fields the parser reads are surrounded by the tables and
// records it must ignore.
const sampleXML = `<?xml version="1.0" encoding="UTF-8"?>
<NonRealTimeMeta xmlns="urn:schemas-professionalDisc:nonRealTimeMeta:ver.2.20" xmlns:lib="urn:schemas-professionalDisc:lib:ver.2.10" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" lastUpdate="2026-07-26T17:35:40-07:00">
    <TargetMaterial umidRef="060A2B340101010501010D4313000000D7CDAB99481206D97CB8DAFFFEE981AD"/>
    <Duration value="435"/>
    <LtcChangeTable tcFps="30" halfStep="false">
        <LtcChange frameCount="0" value="44590400" status="increment"/>
    </LtcChangeTable>
    <CreationDate value="2026-07-26T17:35:25-07:00"/>
    <VideoFormat>
        <VideoFrame videoCodec="AVC140_3840_2160_H422P@L51" captureFps="29.97p" formatFps="29.97p"/>
    </VideoFormat>
    <Device manufacturer="Sony" modelName="ILCE-7CM2" serialNo="01380224"/>
    <Lens modelName="SAMYANG AF 35mm F1.8"/>
    <RecordingMode type="normal" cacheRec="false"/>
    <RelevantFiles>
        <RelatedTo file="SL3SG3Ctos709.cube" rel="LUT"/>
    </RelevantFiles>
</NonRealTimeMeta>
`

func TestParseBytes_RealSonySidecar(t *testing.T) {
	m, err := ParseBytes([]byte(sampleXML))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}

	want := time.Date(2026, 7, 26, 17, 35, 25, 0, time.FixedZone("", -7*3600))
	if !m.CreationDate.Equal(want) {
		t.Errorf("CreationDate = %v, want %v", m.CreationDate, want)
	}
	// The wall-clock components matter more than the instant: they are what
	// ingest formats into originals/YYYY/MM/DD.
	y, mo, d := m.CreationDate.Date()
	if y != 2026 || mo != time.July || d != 26 || m.CreationDate.Hour() != 17 {
		t.Errorf("CreationDate wall clock = %v, want 2026-07-26 17:xx local to the camera", m.CreationDate)
	}
	if m.CameraModel != "ILCE-7CM2" {
		t.Errorf("CameraModel = %q, want %q", m.CameraModel, "ILCE-7CM2")
	}
	if m.LensModel != "SAMYANG AF 35mm F1.8" {
		t.Errorf("LensModel = %q, want %q", m.LensModel, "SAMYANG AF 35mm F1.8")
	}
}

// A newer firmware bumping the schema version in the xmlns must not stop the
// fields from being read — this is why the struct tags omit the namespace.
func TestParseBytes_DifferentSchemaVersion(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<NonRealTimeMeta xmlns="urn:schemas-professionalDisc:nonRealTimeMeta:ver.9.99">
    <CreationDate value="2026-01-02T03:04:05+09:00"/>
    <Device manufacturer="Sony" modelName="ILCE-1"/>
</NonRealTimeMeta>`
	m, err := ParseBytes([]byte(xml))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if m.CameraModel != "ILCE-1" {
		t.Errorf("CameraModel = %q, want ILCE-1", m.CameraModel)
	}
	if m.CreationDate.Hour() != 3 {
		t.Errorf("CreationDate = %v, want 03:04:05 camera-local", m.CreationDate)
	}
}

func TestParseBytes_NoOffsetFallsBackToLocal(t *testing.T) {
	m, err := ParseBytes([]byte(
		`<NonRealTimeMeta><CreationDate value="2026-07-26T17:35:25"/></NonRealTimeMeta>`))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if m.CreationDate.IsZero() {
		t.Fatal("CreationDate is zero, want the offset-less timestamp parsed as local")
	}
	if h, mi := m.CreationDate.Hour(), m.CreationDate.Minute(); h != 17 || mi != 35 {
		t.Errorf("CreationDate = %v, want 17:35 local", m.CreationDate)
	}
}

// Absent fields are not an error: the clip still imports on exiftool's data.
func TestParseBytes_MissingFieldsAreZero(t *testing.T) {
	m, err := ParseBytes([]byte(`<NonRealTimeMeta><Duration value="435"/></NonRealTimeMeta>`))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if !m.CreationDate.IsZero() || m.CameraModel != "" || m.LensModel != "" {
		t.Errorf("got %+v, want zero Meta", m)
	}
}

func TestParseBytes_MalformedIsError(t *testing.T) {
	if _, err := ParseBytes([]byte("<NonRealTimeMeta><unclosed>")); err == nil {
		t.Fatal("ParseBytes on malformed XML: want error, got nil")
	}
}

func TestParse_ReadsFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "C0001M01.XML")
	if err := os.WriteFile(p, []byte(sampleXML), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Parse(p)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.CameraModel != "ILCE-7CM2" {
		t.Errorf("CameraModel = %q, want ILCE-7CM2", m.CameraModel)
	}
}

func TestParse_MissingFileIsError(t *testing.T) {
	if _, err := Parse(filepath.Join(t.TempDir(), "nope.xml")); err == nil {
		t.Fatal("Parse on missing file: want error, got nil")
	}
}

// ---- FindSidecar ------------------------------------------------------------

func TestFindSidecar(t *testing.T) {
	tests := []struct {
		name    string
		files   []string
		clip    string
		want    string
		wantAny bool
	}{
		{
			name:  "camera spelling C0001.MP4 -> C0001M01.XML",
			files: []string{"C0001.MP4", "C0001M01.XML", "C0002.MP4", "C0002M01.XML"},
			clip:  "C0001.MP4",
			want:  "C0001M01.XML",
		},
		{
			name:  "lowercase copy on a case-preserving host",
			files: []string{"c0001.mp4", "c0001m01.xml"},
			clip:  "c0001.mp4",
			want:  "c0001m01.xml",
		},
		{
			name:  "mixed case clip against uppercase sidecar",
			files: []string{"C0001.mp4", "C0001M01.XML"},
			clip:  "C0001.mp4",
			want:  "C0001M01.XML",
		},
		{
			name:  "suffix-less sidecar still matches",
			files: []string{"C0003.MP4", "C0003.XML"},
			clip:  "C0003.MP4",
			want:  "C0003.XML",
		},
		{
			name:  "M01 spelling wins over the bare stem",
			files: []string{"C0004.MP4", "C0004.XML", "C0004M01.XML"},
			clip:  "C0004.MP4",
			want:  "C0004M01.XML",
		},
		{
			name:  "no sidecar",
			files: []string{"IMG_0001.JPG"},
			clip:  "IMG_0001.JPG",
			want:  "",
		},
		{
			// C0001's sidecar must not be handed to a differently-named clip.
			name:  "unrelated sidecar is not claimed",
			files: []string{"C0010.MP4", "C0001M01.XML"},
			clip:  "C0010.MP4",
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := FindSidecar(filepath.Join(dir, tc.clip))
			var want string
			if tc.want != "" {
				want = filepath.Join(dir, tc.want)
			}
			if got != want {
				t.Errorf("FindSidecar = %q, want %q", got, want)
			}
		})
	}
}

func TestFindSidecar_UnreadableDirReturnsEmpty(t *testing.T) {
	if got := FindSidecar(filepath.Join(t.TempDir(), "gone", "C0001.MP4")); got != "" {
		t.Errorf("FindSidecar = %q, want empty for a nonexistent directory", got)
	}
}
