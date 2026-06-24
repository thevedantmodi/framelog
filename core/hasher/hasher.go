// Package hasher computes SHA-256 digests for dedup checks. Chunked reads —
// never load the whole RAW file into memory, same rule as the Python version.
package hasher

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// HashFile returns the hex-encoded SHA-256 digest of the file at path.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil { // io.Copy already chunks internally
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
