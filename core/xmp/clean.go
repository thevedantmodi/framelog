package xmp

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// CleanResult reports what Clean found and removed.
type CleanResult struct {
	Found   []string // every .xmp sidecar under the root, sorted
	Removed int      // how many of Found were deleted (0 on a dry run)
}

// Clean deletes every .xmp sidecar (any case) under root, skipping .git/ so
// the repo's history of those sidecars stays intact and they can be
// restored with git. With dryRun it only lists them. Symlinks are not
// followed and only regular files are touched. A failed delete stops the
// run and returns the partial result alongside the error.
//
// It deletes every sidecar, not just the ones ingest wrote: ones written by
// Lightroom or Camera Raw hold edits and go too. That is what the
// `framelogd clean-xmp` command is for; its --dry-run lists them first.
func Clean(root string, dryRun bool) (CleanResult, error) {
	var res CleanResult
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && strings.EqualFold(filepath.Ext(path), ".xmp") {
			res.Found = append(res.Found, path)
		}
		return nil
	})
	if err != nil {
		return res, fmt.Errorf("xmp: walk %s: %w", root, err)
	}
	if dryRun {
		return res, nil
	}
	for _, path := range res.Found {
		if err := os.Remove(path); err != nil {
			return res, fmt.Errorf("xmp: remove %s: %w", path, err)
		}
		res.Removed++
	}
	return res, nil
}
