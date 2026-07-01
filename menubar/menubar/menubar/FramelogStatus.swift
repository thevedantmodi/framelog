import AppKit
import Combine
import Darwin
import Foundation
import ServiceManagement
import UserNotifications

// MARK: - Pure functions (all testable without live APIs)

// Returns menu item label for the given SMAppService status.
// .requiresApproval shows a visible indicator so the user knows to act.
func loginItemLabelString(status: SMAppService.Status) -> String {
    switch status {
    case .notRegistered:    return "Launch at Login"
    case .enabled:          return "Launch at Login"
    case .requiresApproval: return "Launch at Login (check System Settings)"
    case .notFound:         return "Launch at Login"
    @unknown default:       return "Launch at Login"
    }
}

// Returns true when the toggle should appear checked.
func loginItemIsChecked(status: SMAppService.Status) -> Bool {
    switch status {
    case .enabled, .requiresApproval: return true
    default: return false
    }
}

// Returns false only for .notFound — the app bundle isn't recognized, nothing to do.
func loginItemIsInteractive(status: SMAppService.Status) -> Bool {
    if case .notFound = status { return false }
    return true
}

// Returns the notification body for a photo-count delta. Pure function, no side effects.
func importDeltaMessage(oldCount: Int, newCount: Int) -> String {
    let delta = max(0, newCount - oldCount)
    return "Imported \(delta) new photo\(delta == 1 ? "" : "s")"
}

// isCoreReachable attempts a POSIX connect() to the Unix socket the Go core
// listens on. Returns immediately — ECONNREFUSED/ENOENT come back in microseconds.
// Pure function; no Swift concurrency needed.
func isCoreReachable(socketPath: String) -> Bool {
    let fd = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
    guard fd >= 0 else { return false }
    defer { Darwin.close(fd) }
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let pathBytes = socketPath.utf8.prefix(103) // 104-byte macOS limit
    withUnsafeMutablePointer(to: &addr.sun_path) { ptr in
        pathBytes.withContiguousStorageIfAvailable { src in
            UnsafeMutableRawPointer(ptr).copyMemory(from: src.baseAddress!, byteCount: src.count)
        }
    }
    return withUnsafePointer(to: &addr) { ptr in
        ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) == 0
        }
    }
}

// Returns the status display string for the menu (FL-403 / FL-604).
// Four states derived from socket reachability and DB snapshot:
// (a) socket down + db missing  → "Install Core to get started"
// (b) socket down + db exists   → "Core restarting…"  (launchd recovering from crash)
// (c) socket up   + db empty    → "No photos imported yet"
// (d) socket up   + db has data → "N photos · last import: <relative time>"
func statusDisplayString(snapshot: CatalogSnapshot?, coreReachable: Bool) -> String {
    guard coreReachable else {
        return snapshot == nil ? "Install Core to get started" : "Core restarting…"
    }
    guard let snapshot else { return "Core not running" }
    guard snapshot.photoCount > 0 else { return "No photos imported yet" }
    let n = snapshot.photoCount
    let countPart = "\(n) photo\(n == 1 ? "" : "s")"
    guard let lastStr = snapshot.lastImport,
          let date = ISO8601DateFormatter().date(from: lastStr) else { return countPart }
    let fmt = RelativeDateTimeFormatter()
    fmt.unitsStyle = .full
    return "\(countPart) · last import: \(fmt.localizedString(for: date, relativeTo: Date()))"
}

// Sends {"command":"status"} over the Unix socket and returns daemon_version
// from the response. Returns nil if the socket is unreachable or the field is absent.
func fetchDaemonVersion(socketPath: String) -> String? {
    let fd = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
    guard fd >= 0 else { return nil }
    defer { Darwin.close(fd) }
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let pathBytes = socketPath.utf8.prefix(103)
    withUnsafeMutablePointer(to: &addr.sun_path) { ptr in
        pathBytes.withContiguousStorageIfAvailable { src in
            UnsafeMutableRawPointer(ptr).copyMemory(from: src.baseAddress!, byteCount: src.count)
        }
    }
    let connected = withUnsafePointer(to: &addr) { ptr in
        ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) == 0
        }
    }
    guard connected else { return nil }
    let payload = "{\"command\":\"status\"}\n"
    payload.withCString { ptr in _ = Darwin.write(fd, ptr, strlen(ptr)) }
    var buf = [UInt8](repeating: 0, count: 512)
    let n = Darwin.read(fd, &buf, buf.count - 1)
    guard n > 0 else { return nil }
    let data = Data(buf.prefix(n))
    guard let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
          let version = json["daemon_version"] as? String,
          !version.isEmpty else { return nil }
    return version
}

// Sends set_backup_path over the Unix socket. Fire-and-forget — failures are
// silent (the drive picker already confirmed a valid path).
func sendSetBackupPath(socketPath: String, path: String) {
    let fd = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
    guard fd >= 0 else { return }
    defer { Darwin.close(fd) }

    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let pathBytes = socketPath.utf8.prefix(103)
    withUnsafeMutablePointer(to: &addr.sun_path) { ptr in
        pathBytes.withContiguousStorageIfAvailable { src in
            UnsafeMutableRawPointer(ptr).copyMemory(from: src.baseAddress!, byteCount: src.count)
        }
    }
    let connected = withUnsafePointer(to: &addr) { ptr in
        ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) == 0
        }
    }
    guard connected else { return }

    // Escape path for JSON (backslashes and quotes are the only chars that need it
    // on macOS volume paths, but a full escape is safer).
    let escaped = path
        .replacingOccurrences(of: "\\", with: "\\\\")
        .replacingOccurrences(of: "\"", with: "\\\"")
    let payload = "{\"command\":\"set_backup_path\",\"path\":\"\(escaped)\"}\n"
    payload.withCString { ptr in
        _ = Darwin.write(fd, ptr, strlen(ptr))
    }
    // Read (and discard) the response so the server sees a clean close.
    var buf = [UInt8](repeating: 0, count: 256)
    _ = Darwin.read(fd, &buf, buf.count)
}

enum CoreInstallState {
    case idle, installing, success, error(String)

    var label: String {
        switch self {
        case .idle:            return "Install Core…"
        case .installing:      return "Installing…"
        case .success:         return "Installed ✓"
        case .error:           return "Install Failed"
        }
    }
    var isInProgress: Bool {
        if case .installing = self { return true }
        return false
    }
}

// MARK: - FramelogStatus

@MainActor
final class FramelogStatus: ObservableObject {
    @Published private(set) var displayString: String = "Core not running"
    @Published private(set) var loginItemStatus: SMAppService.Status = .notRegistered
    @Published var ingestRequested = false
    @Published var outgestRequested = false
    @Published private(set) var coreInstallState: CoreInstallState = .idle

    private var previousCount = 0
    private var previousLastImport: String?
    private var timer: Timer?

    // Version stamped into the app bundle at build time — same source as framelogd's
    // -ldflags Version. Used to detect when an upgrade replaced the bundle but the
    // old daemon is still running.
    private let bundledVersion: String? =
        Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String
    // Guard against re-triggering install in the same app session.
    private var hasAutoInstalled = false

    init() {
        requestNotificationPermissionIfNeeded()
        refreshLoginItemStatus()
        refresh()
        // 15-second poll matches PROTOCOL.md §4 — don't change this number.
        timer = Timer.scheduledTimer(withTimeInterval: 15, repeats: true) { [weak self] _ in
            self?.refresh()
        }
    }

    deinit { timer?.invalidate() }

    // MARK: Login item (FL-402)

    func refreshLoginItemStatus() {
        loginItemStatus = SMAppService.mainApp.status
    }

    func toggleLoginItem() {
        switch loginItemStatus {
        case .notRegistered:
            try? SMAppService.mainApp.register()
        case .enabled:
            try? SMAppService.mainApp.unregister()
        case .requiresApproval:
            // Already registered, pending approval — direct user to System Settings.
            SMAppService.openSystemSettingsLoginItems()
        case .notFound:
            // Bundle not recognized; nothing the app can do. Disabled in UI.
            break
        @unknown default:
            break
        }
        refreshLoginItemStatus()
    }

    // MARK: Refresh cycle (FL-403, FL-405)

    func refresh() {
        let snapshot = CatalogReader.read(dbPath: FramelogPaths.catalogDB.path)
        let coreReachable = isCoreReachable(socketPath: FramelogPaths.socket.path)
        let newCount = snapshot?.photoCount ?? 0
        let newLastImport = snapshot?.lastImport

        // Reuse this tick for import notifications (FL-405) — no second timer.
        if let newLast = newLastImport,
           newLast != previousLastImport,
           newCount > previousCount {
            fireImportNotification(oldCount: previousCount, newCount: newCount)
        }

        previousCount = newCount
        previousLastImport = newLastImport
        displayString = statusDisplayString(snapshot: snapshot, coreReachable: coreReachable)
        refreshLoginItemStatus()

        if coreReachable, !hasAutoInstalled, let expected = bundledVersion {
            let running = fetchDaemonVersion(socketPath: FramelogPaths.socket.path)
            if let running, running != expected {
                hasAutoInstalled = true
                installCore()
            }
        }
    }

    // MARK: Notifications (FL-405)

    private func requestNotificationPermissionIfNeeded() {
        let key = "notificationPermissionRequested"
        guard !UserDefaults.standard.bool(forKey: key) else { return }
        UserDefaults.standard.set(true, forKey: key)
        Task {
            try? await UNUserNotificationCenter.current()
                .requestAuthorization(options: [.alert, .sound])
        }
    }

    private func fireImportNotification(oldCount: Int, newCount: Int) {
        let content = UNMutableNotificationContent()
        content.title = "Framelog"
        content.body = importDeltaMessage(oldCount: oldCount, newCount: newCount)
        let req = UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil)
        Task { try? await UNUserNotificationCenter.current().add(req) }
    }

    // MARK: Manual controls (FL-404)

    func requestIngest() {
        guard !ingestRequested,
              (try? touchTriggerFile(at: FramelogPaths.ingestTrigger)) != nil else { return }
        ingestRequested = true
        Task {
            try? await Task.sleep(for: .seconds(2))
            ingestRequested = false
        }
    }

    func requestOutgest() {
        guard !outgestRequested,
              (try? touchTriggerFile(at: FramelogPaths.outgestTrigger)) != nil else { return }
        outgestRequested = true
        Task {
            try? await Task.sleep(for: .seconds(2))
            outgestRequested = false
        }
    }

    // MARK: Core install (FL-603)

    private static let gitRemoteURLKey = "gitRemoteURL"

    func installCore() {
        guard !coreInstallState.isInProgress else { return }
        guard let binary = FramelogPaths.framelogdBinary else {
            coreInstallState = .error("framelogd not found")
            resetInstallState()
            return
        }
        coreInstallState = .installing
        var arguments = ["install"]
        let remoteURL = UserDefaults.standard.string(forKey: Self.gitRemoteURLKey) ?? ""
        if !remoteURL.isEmpty {
            arguments += ["--remote", remoteURL]
        }
        Task.detached {
            let proc = Process()
            proc.executableURL = binary
            proc.arguments = arguments
            do {
                try proc.run()
                proc.waitUntilExit()
                let ok = proc.terminationStatus == 0
                await MainActor.run {
                    self.coreInstallState = ok ? .success : .error("exit \(proc.terminationStatus)")
                    self.resetInstallState()
                }
            } catch {
                await MainActor.run {
                    self.coreInstallState = .error(error.localizedDescription)
                    self.resetInstallState()
                }
            }
        }
    }

    private func resetInstallState() {
        Task {
            try? await Task.sleep(for: .seconds(3))
            coreInstallState = .idle
        }
    }

    // MARK: Backup path (set_backup_path IPC command)

    func chooseAndSetBackupDrive() {
        let panel = NSOpenPanel()
        panel.canChooseFiles = false
        panel.canChooseDirectories = true
        panel.allowsMultipleSelection = false
        panel.prompt = "Set as Backup Drive"
        panel.message = "Choose the folder where Framelog should copy your originals."
        guard panel.runModal() == .OK, let url = panel.url else { return }
        Task.detached {
            sendSetBackupPath(socketPath: FramelogPaths.socket.path, path: url.path)
        }
    }

    func configureGitRemote() {
        let alert = NSAlert()
        alert.messageText = "Git Remote URL"
        alert.informativeText = "Where framelogd pushes originals/ to (e.g. git@github.com:user/photos.git). Leave blank to unset."
        alert.addButton(withTitle: "Save")
        alert.addButton(withTitle: "Cancel")
        let field = NSTextField(frame: NSRect(x: 0, y: 0, width: 280, height: 24))
        field.stringValue = UserDefaults.standard.string(forKey: Self.gitRemoteURLKey) ?? ""
        alert.accessoryView = field
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        let url = field.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
        UserDefaults.standard.set(url, forKey: Self.gitRemoteURLKey)
        guard !url.isEmpty else { return }
        installCore()
    }

    // MARK: Other menu actions (FL-406)

    func openLogFile() {
        NSWorkspace.shared.open(FramelogPaths.framelogLog)
    }

    func runSetup() {
        // Re-runs login-item registration and notification permission request.
        // Scope: only what this app owns (login item + notification permission).
        // Does NOT install or touch the Go core's launchd agent (com.framelog.core.plist) —
        // that is FL-303's job, deliberately separate from the frontend's setup flow.
        try? SMAppService.mainApp.register()
        refreshLoginItemStatus()
        Task {
            try? await UNUserNotificationCenter.current()
                .requestAuthorization(options: [.alert, .sound])
        }
    }

    // MARK: View bindings

    var loginItemLabel: String   { loginItemLabelString(status: loginItemStatus) }
    var loginItemIsOn: Bool      { loginItemIsChecked(status: loginItemStatus) }
    var loginItemIsEnabled: Bool { loginItemIsInteractive(status: loginItemStatus) }
}
