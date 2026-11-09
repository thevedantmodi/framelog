// Package hasher computes SHA-256 digests for dedup checks. Chunked reads —
// never load the whole RAW file into memory, same rule as the Python version.
package hasher

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// QuickChunk is how much of each end of a file QuickHash reads.
const QuickChunk = 64 << 10

// QuickHash is a cheap fingerprint for the "what's new on this card" preview:
// SHA-256 over the file size, its first 64 KiB, and its last 64 KiB (the
// whole file when it is smaller than 128 KiB). It reads at most 128 KiB per
// file, so a card of a thousand photos is fingerprinted in seconds instead
// of the minutes a full read takes.
//
// It is a hint, never proof: two different files could in principle share
// size and both ends. Camera files make that vanishingly unlikely (the
// header carries the capture time and shot counter), and everything that
// decides whether a card is safe to format still uses the full HashFile.
func QuickHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := fi.Size()

	h := sha256.New()
	fmt.Fprintf(h, "%d\n", size)
	if size <= 2*QuickChunk {
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	if _, err := io.CopyN(h, f, QuickChunk); err != nil {
		return "", err
	}
	if _, err := f.Seek(size-QuickChunk, io.SeekStart); err != nil {
		return "", err
	}
	if _, err := io.CopyN(h, f, QuickChunk); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
