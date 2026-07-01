import SwiftUI

struct ContentView: View {
    @EnvironmentObject var status: FramelogStatus

    var body: some View {
        // FL-403: Three-state display — "Core not running" / "No photos imported yet" / count+age
        Text(status.displayString)
            .foregroundStyle(.secondary)

        Divider()

        // FL-402: Login item toggle. .requiresApproval shows "(check System Settings)" suffix.
        Toggle(isOn: Binding(
            get: { status.loginItemIsOn },
            set: { _ in status.toggleLoginItem() }
        )) {
            Text(status.loginItemLabel)
        }
        .disabled(!status.loginItemIsEnabled)

        Divider()

        // Pause/resume: a single global toggle covering ingest + outgest,
        // automatic (SD card / trigger file) and manual alike.
        Button(status.isPaused ? "Resume Framelog" : "Pause Framelog") {
            status.togglePause()
        }
        .disabled(status.pauseToggleInFlight)

        if status.isPaused {
            Text("Paused — ingest and outgest are stopped")
                .foregroundStyle(.secondary)
        }

        Divider()

        // FL-404: Trigger-file controls. "Requested…" for ~2s — no false confirmation.
        Button(status.ingestRequested ? "Requested…" : "Run Ingest Now") {
            status.requestIngest()
        }
        .disabled(status.ingestRequested || status.isPaused)

        Button(status.outgestRequested ? "Requested…" : "Run Outgest Now") {
            status.requestOutgest()
        }
        .disabled(status.outgestRequested || status.isPaused)

        Divider()

        // FL-603: Install the Go daemon as a launchd agent.
        Button(status.coreInstallState.label) { status.installCore() }
            .disabled(status.coreInstallState.isInProgress)
        Button("Set Git Remote…") { status.configureGitRemote() }

        Divider()

        // FL-406
        Button("Open Log File")       { status.openLogFile() }
        Button("Set Backup Drive…")   { status.chooseAndSetBackupDrive() }
        Button("Run Setup")           { status.runSetup() }

        Divider()

        Button("Quit Framelog") {
            NSApplication.shared.terminate(nil)
        }
    }
}
