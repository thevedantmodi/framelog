import Foundation

// touchTriggerFile creates or overwrites the file at url with empty contents.
// The Go core polls for these files; creating one is the entire v1 IPC signal.
func touchTriggerFile(at url: URL) throws {
    try Data().write(to: url, options: .atomic)
}
