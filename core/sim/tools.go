package sim

import (
	"fmt"
	"os"
	"path/filepath"
)

// The fake tools below are tiny shell scripts that stand in for the macOS
// and Homebrew programs the pipeline shells out to. They behave just enough
// like the real ones for the pipeline to run end to end:
//
//   diskutil  "info <path>": anything under the sandbox's Volumes/ is a
//             removable card, with a Volume UUID derived from its name.
//   exiftool  "-json -n <file>": every still was shot on an α7C II
//             (ILCE-7CM2) on the same afternoon.
//   rclone    "copy SRC DST [...]": copies files (honouring
//             --files-from-raw, --include, --ignore-existing) and logs each
//             copy as rclone's JSON log line. If the "backup-full" marker
//             file exists, copies to the backup drive fail with rclone's
//             "no space left on device" error.
//   pmset     reports battery power, so nothing is pushed.

const fakeDiskutil = `
case "$2" in
  */Volumes/*)
    name=$(basename "$2")
    echo "   Volume Name:               $name"
    echo "   Removable Media:           Removable"
    echo "   Volume UUID:               SIM-$(printf %s "$name" | tr -c 'A-Za-z0-9' '-')"
    ;;
  *)
    echo "   Removable Media:           Fixed"
    ;;
esac
`

const fakeExiftool = `echo '[{"Model":"ILCE-7CM2","DateTimeOriginal":"2026:11:06 14:03:11"}]'`

const fakePmset = `echo "Now drawing from 'Battery Power'"`

// fakeRclone takes the backup drive's path and the backup-full marker path.
const fakeRcloneTmpl = `
src="$2"; dst="$3"; shift 3
list=""; ignore_existing=""; includes=""
while [ $# -gt 0 ]; do
  case "$1" in
    --files-from-raw) list="$2"; shift ;;
    --include) includes="$includes $(printf %%s "$2" | sed 's/^\*//' | tr '[:upper:]' '[:lower:]')"; shift ;;
    --ignore-existing) ignore_existing=1 ;;
  esac
  shift
done

case "$dst" in
  %[1]s*)
    if [ -e "%[2]s" ]; then
      printf '{"level":"error","msg":"Failed to copy: write %%s: no space left on device","object":"x"}\n' "$dst" >&2
      exit 1
    fi
    ;;
esac

mkdir -p "$dst"
copy_one() {
  rel="$1"; from="$src/$rel"; to="$dst/$rel"
  if [ -e "$to" ]; then
    [ -n "$ignore_existing" ] && return
    cmp -s "$from" "$to" && return
  fi
  mkdir -p "$(dirname "$to")"
  cp "$from" "$to"
  printf '{"level":"info","msg":"Copied (new)","object":"%%s"}\n' "$rel" >&2
}
wanted() {
  [ -z "$includes" ] && return 0
  ext=".$(printf %%s "${1##*.}" | tr '[:upper:]' '[:lower:]')"
  for inc in $includes; do [ "$inc" = "$ext" ] && return 0; done
  return 1
}
if [ -n "$list" ]; then
  while IFS= read -r rel; do [ -n "$rel" ] && copy_one "$rel"; done < "$list"
else
  find "$src" -type f | while IFS= read -r f; do
    rel="${f#$src/}"
    wanted "$rel" && copy_one "$rel"
  done
fi
exit 0
`

type tools struct {
	git, exiftool, rclone, diskutil, pmset string
	realRclone                             bool
}

func writeScript(dir, name, body string) (string, error) {
	p := filepath.Join(dir, name)
	return p, os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755)
}

func writeFakeTools(binDir, backupDir, fullMarker string) (tools, error) {
	var t tools
	var err error
	if t.diskutil, err = writeScript(binDir, "diskutil", fakeDiskutil); err != nil {
		return t, err
	}
	if t.exiftool, err = writeScript(binDir, "exiftool", fakeExiftool); err != nil {
		return t, err
	}
	if t.pmset, err = writeScript(binDir, "pmset", fakePmset); err != nil {
		return t, err
	}
	t.rclone, err = writeScript(binDir, "rclone", fmt.Sprintf(fakeRcloneTmpl, backupDir, fullMarker))
	return t, err
}
