package cardverify

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"
)

var unsafeNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// ReceiptName is the receipt's file name: <local start time>_<volume>.txt,
// sortable by date and safe on any filesystem.
func ReceiptName(r *Report) string {
	vol := unsafeNameChars.ReplaceAllString(r.VolumeName, "_")
	if vol == "" {
		vol = "card"
	}
	return fmt.Sprintf("%s_%s.txt", r.StartedAt.Local().Format("20060102_150405"), vol)
}

// WriteReceipt writes the human-readable report into dir and returns its
// path. The receipt is the durable "what was on this card and where did it
// go" record: open it, read the verdict line, format with confidence.
func WriteReceipt(r *Report, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// O_EXCL + a numeric suffix: a re-verify in the same second must not
	// overwrite the earlier receipt — each one is a record of a check.
	name := ReceiptName(r)
	stem := strings.TrimSuffix(name, ".txt")
	body := []byte(RenderReceipt(r))
	for i := 1; i <= 100; i++ {
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			name = fmt.Sprintf("%s-%d.txt", stem, i+1)
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(body)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		return path, werr
	}
	return "", fmt.Errorf("too many receipts named %s", stem)
}

// RenderReceipt formats the report as plain text.
func RenderReceipt(r *Report) string {
	sum := r.Summary()
	counts := r.StateCounts()
	var b strings.Builder

	fmt.Fprintf(&b, "Framelog card report\n")
	fmt.Fprintf(&b, "====================\n\n")
	if r.SafeToFormat {
		fmt.Fprintf(&b, "VERDICT:  SAFE TO FORMAT ✓\n")
	} else {
		fmt.Fprintf(&b, "VERDICT:  DO NOT FORMAT ✗\n")
	}
	fmt.Fprintln(&b)

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	card := r.VolumeName
	if r.VolumeUUID != "" {
		card += "  (" + r.VolumeUUID + ")"
	}
	fmt.Fprintf(tw, "Card:\t%s\n", card)
	fmt.Fprintf(tw, "Checked:\t%s → %s (%s)\n",
		r.StartedAt.Local().Format("2006-01-02 15:04:05"),
		r.FinishedAt.Local().Format("15:04:05"),
		r.FinishedAt.Sub(r.StartedAt).Round(time.Second))
	fmt.Fprintf(tw, "Media files:\t%d (%s)\n", sum.FilesTotal, HumanBytes(sum.BytesTotal))
	if sum.FileTypes != "" {
		fmt.Fprintf(tw, "Types:\t%s\n", sum.FileTypes)
	}
	if sum.CameraModels != "" {
		fmt.Fprintf(tw, "Camera:\t%s\n", sum.CameraModels)
	}
	if sum.CaptureFirst != "" {
		fmt.Fprintf(tw, "Shot:\t%s → %s\n", exifToDisplay(sum.CaptureFirst), exifToDisplay(sum.CaptureLast))
	}
	fmt.Fprintf(tw, "Imported:\t%d new, %d already in library\n", sum.FilesNew, sum.FilesExisting)
	libOK := sum.FilesTotal - counts[StateNotInLibrary] - counts[StateLibraryMissing] -
		counts[StateLibraryMismatch] - counts[StateCardUnreadable]
	fmt.Fprintf(tw, "Library:\t%d/%d verified (SHA-256 re-read from card and library)\n", libOK, sum.FilesTotal)
	switch r.BackupState {
	case BackupNotConfigured:
		fmt.Fprintf(tw, "Backup:\tnot configured\n")
	case BackupDriveMissing:
		fmt.Fprintf(tw, "Backup:\tdrive not connected (%s)\n", r.BackupPath)
	default:
		fmt.Fprintf(tw, "Backup:\t%d/%d verified on %s\n", counts[StateVerified], sum.FilesTotal, r.BackupPath)
	}
	if n := counts[StateIgnored]; n > 0 {
		fmt.Fprintf(tw, "Ignored:\t%d file(s) of a type switched off in File Types (%s)\n", n, r.ignoredTypes())
	}
	if r.Sidecars > 0 {
		fmt.Fprintf(tw, "Sidecars:\t%d clip metadata file(s), copied with their clips\n", r.Sidecars)
	}
	tw.Flush()

	if len(r.Problems) > 0 {
		fmt.Fprintf(&b, "\nProblems\n--------\n")
		for _, p := range r.Problems {
			fmt.Fprintf(&b, "  • %s\n", p)
		}
	}
	if len(r.Notes) > 0 {
		fmt.Fprintf(&b, "\nNotes\n-----\n")
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "  • %s\n", n)
		}
	}

	// Problem files first so they are on screen without scrolling.
	fmt.Fprintf(&b, "\nFiles\n-----\n")
	tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "STATE\tCARD FILE\tSIZE\tLIBRARY / DETAIL\n")
	write := func(f FileResult) {
		where := f.Detail
		if f.State == StateVerified {
			where = f.LibraryPath
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", f.State, f.RelPath, HumanBytes(f.Size), where)
	}
	for _, f := range r.Files {
		if f.State != StateVerified {
			write(f)
		}
	}
	for _, f := range r.Files {
		if f.State == StateVerified {
			write(f)
		}
	}
	tw.Flush()
	return b.String()
}

// exifToDisplay turns "2026:11:07 18:03:11" into "2026-11-07 18:03".
func exifToDisplay(s string) string {
	t, err := time.Parse("2006:01:02 15:04:05", s)
	if err != nil {
		return s
	}
	return t.Format("2006-01-02 15:04")
}

// HumanBytes renders n with a decimal unit, matching Finder's convention.
func HumanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
