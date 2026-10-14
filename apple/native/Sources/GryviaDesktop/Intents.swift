// SPDX-License-Identifier: BUSL-1.1
// Copyright © 2026 Zyvor AI Labs Private Limited
#if os(macOS)
import AppIntents
import Foundation
import Security
import GryviaCore

/// Shortcuts / Spotlight actions. They use the same Keychain token and server origin as the app and only call the
/// public API, so a tenant token limits what a shortcut can do. Registration with Shortcuts needs the app to be
/// built by Xcode (App Intents metadata); the code compiles in the SwiftPM build used by CI.
enum IntentClient {
    static func api() throws -> API {
        let q: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: "dev.zyvor.gryvia.admin",
                                kSecReturnData as String: true, kSecMatchLimit as String: kSecMatchLimitOne]
        var out: AnyObject?
        guard SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess, let d = out as? Data else {
            throw GryviaError.invalid("Open Gryvia once and connect to a server first.")
        }
        let server = UserDefaults(suiteName: "dev.zyvor.gryvia.desktop")?.string(forKey: "server")
            ?? UserDefaults.standard.string(forKey: "server") ?? "http://127.0.0.1:8443"
        return try API(base: server, token: String(decoding: d, as: UTF8.self))
    }
}

struct FleetStatusIntent: AppIntent {
    static let title: LocalizedStringResource = "Fleet status"
    static let description = IntentDescription("How many Macs are online, how many jobs are running and queued.")
    func perform() async throws -> some IntentResult & ProvidesDialog {
        let s = try await IntentClient.api().state()
        let online = s.nodes.filter(\.online).count
        let running = s.jobs.filter { $0.state == "running" }.count
        let queued = s.jobs.filter { $0.state == "queued" }.count
        return .result(dialog: "\(online) of \(s.nodes.count) Macs online, \(running) running, \(queued) queued.")
    }
}

struct RunDemoJobIntent: AppIntent {
    static let title: LocalizedStringResource = "Run a demo job"
    static let description = IntentDescription("Send a 3-second test workload to the fleet.")
    func perform() async throws -> some IntentResult & ProvidesDialog {
        _ = try await IntentClient.api().request("api/jobs", body: ["runtime": "demo", "model": "Shortcut demo", "memoryGiB": 1.0, "priority": 50,
                                                                    "args": ["--seconds", "3"], "mode": "exclusive", "labels": ["shortcut"]])
        return .result(dialog: "Demo job submitted.")
    }
}

struct PauseThisMacIntent: AppIntent {
    static let title: LocalizedStringResource = "Pause fleet work on this Mac"
    static let description = IntentDescription("Stop the fleet from starting new work on this Mac.")
    @Parameter(title: "Resume instead") var resume: Bool?
    func perform() async throws -> some IntentResult & ProvidesDialog {
        let api = try IntentClient.api()
        let host = ProcessInfo.processInfo.hostName.lowercased()
        let short = host.split(separator: ".").first.map(String.init) ?? host
        let state = try await api.state()
        guard let me = state.nodes.first(where: { ($0.host ?? "").lowercased().hasPrefix(short) || $0.id.lowercased() == short }) else {
            return .result(dialog: "This Mac is not registered with the fleet.")
        }
        let mode = (resume ?? false) ? "always" : "paused"
        _ = try await api.request("api/nodes/policy", body: ["id": me.id, "mode": mode])
        return .result(dialog: mode == "paused" ? "Fleet work paused on \(me.id)." : "Fleet work resumed on \(me.id).")
    }
}

struct GryviaShortcuts: AppShortcutsProvider {
    static var appShortcuts: [AppShortcut] {
        AppShortcut(intent: FleetStatusIntent(), phrases: ["Fleet status in \(.applicationName)", "How is my \(.applicationName) fleet"], shortTitle: "Fleet status", systemImageName: "waveform.path.ecg")
        AppShortcut(intent: RunDemoJobIntent(), phrases: ["Run a demo job in \(.applicationName)"], shortTitle: "Run demo job", systemImageName: "play.fill")
        AppShortcut(intent: PauseThisMacIntent(), phrases: ["Pause \(.applicationName) on this Mac"], shortTitle: "Pause this Mac", systemImageName: "pause.fill")
    }
}
#endif
