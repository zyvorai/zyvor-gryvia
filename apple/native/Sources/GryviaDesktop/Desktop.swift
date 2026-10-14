// SPDX-License-Identifier: BUSL-1.1
// Copyright © 2026 Zyvor AI Labs Private Limited
#if os(macOS)
import SwiftUI
import Charts
import Security
import UserNotifications
import GryviaCore

/// Token storage in the login Keychain (never UserDefaults).
enum TokenStore {
    private static let service = "dev.zyvor.gryvia.admin"
    static func load() -> String {
        let q: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service,
                                kSecReturnData as String: true, kSecMatchLimit as String: kSecMatchLimitOne]
        var out: AnyObject?
        guard SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess, let d = out as? Data else { return "" }
        return String(decoding: d, as: UTF8.self)
    }
    static func save(_ token: String) {
        let base: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service]
        SecItemDelete(base as CFDictionary)
        guard !token.isEmpty else { return }
        var add = base; add[kSecValueData as String] = Data(token.utf8)
        SecItemAdd(add as CFDictionary, nil)
    }
}

extension Color {
    init(hex: UInt32) {
        self.init(red: Double((hex >> 16) & 0xFF) / 255, green: Double((hex >> 8) & 0xFF) / 255, blue: Double(hex & 0xFF) / 255)
    }
    /// Accent chosen in Settings; defaults to Zyvor blue (#0088ff, shared with the web dashboard via brand/tokens.json).
    static func accent(named name: String) -> Color {
        Color(hex: Brand.accents.first { $0.name == name }?.hex ?? Brand.accentHex)
    }
}

enum Pane: String, CaseIterable, Identifiable {
    case guide = "Get started", live = "Live", overview = "Overview", models = "Model hub", training = "Training", deployments = "Deployments", fleet = "Fleet", workloads = "Workloads", tenants = "Tenants", alerts = "Alerts"
    var id: String { rawValue }
    static let groups: [(String, [Pane])] = [
        ("Monitor", [.live, .overview, .fleet]), ("Build", [.models, .training]),
        ("Run", [.workloads, .deployments]), ("Govern", [.tenants, .alerts])]
    var symbol: String {
        switch self { case .overview: "square.grid.2x2"; case .fleet: "desktopcomputer"; case .workloads: "cpu"; case .tenants: "person.2"; case .alerts: "bell"; case .guide: "questionmark.circle"; case .live: "waveform.path.ecg"; case .models: "cube"; case .training: "chart.line.uptrend.xyaxis"; case .deployments: "arrow.up.forward.app" }
    }
}

@MainActor @Observable final class Fleet {
    var state: Snapshot?
    var error = ""
    var server = UserDefaults.standard.string(forKey: "server") ?? "http://127.0.0.1:8443"
    var token = TokenStore.load() { didSet { if token != oldValue { TokenStore.save(token) } } }
    var model = ""
    var runtime = "demo"
    var memory = 4.0
    var args = "[]"
    var priority = 50
    var lastUpdated: Date?
    var telemetry: [String: [TelemetrySample]] = [:]
    var results: [ResultRow] = []
    var liveActive = false
    var logs: [LogLine] = []
    private var known: [String: String] = [:]

    var connected: Bool { state != nil && error.isEmpty }
    var alerts: [FleetAlert] {
        guard let state else { return [] }
        var out: [FleetAlert] = []
        for n in state.nodes {
            if !n.online { out.append(FleetAlert(level: .error, title: "\(n.id) is offline", detail: "No heartbeat in the last 30 seconds.")) }
            if let t = n.thermal, ["serious", "critical"].contains(t) { out.append(FleetAlert(level: .error, title: "\(n.id) thermal \(t)", detail: "New work is not placed here.")) }
            if n.drain { out.append(FleetAlert(level: .info, title: "\(n.id) is draining", detail: "Resume admission when maintenance is done.")) }
        }
        for j in state.jobs where j.state == "failed" { out.append(FleetAlert(level: .error, title: "\(j.spec.model ?? j.spec.runtime) failed", detail: String(j.id.prefix(10)))) }
        return out
    }
    var running: Int { state?.jobs.filter { $0.state == "running" }.count ?? 0 }
    var queued: Int { state?.jobs.filter { $0.state == "queued" }.count ?? 0 }

    func api() throws -> API { try API(base: server, token: token) }

    func refresh() async {
        do {
            let client = try api()
            let snap = try await client.state()
            telemetry = (try? await client.telemetry()) ?? [:]
            results = (try? await client.results()) ?? []
            notifyTransitions(snap)
            state = snap; error = ""; lastUpdated = Date()
            NSApp.dockTile.badgeLabel = running > 0 ? String(running) : nil
        } catch { self.error = Self.hint(error) }
    }
    func action(_ path: String, _ body: sending [String: Any]) async {
        do { _ = try await api().request(path, body: body); await refresh() } catch { self.error = Self.hint(error) }
    }
    func submit() async {
        do {
            guard let argv = try JSONSerialization.jsonObject(with: Data(args.utf8)) as? [String] else {
                throw GryviaError.invalid("Arguments must be a JSON array of strings, e.g. [\"--prompt\",\"hi\"]")
            }
            await action("api/jobs", ["runtime": runtime, "model": model, "memoryGiB": memory, "priority": priority, "args": argv, "mode": "exclusive"])
        } catch { self.error = Self.hint(error) }
    }
    /// One-click smoke workload: runs the built-in CPU demo runtime for 3 seconds.
    func runDemo() async {
        await action("api/jobs", ["runtime": "demo", "model": "Hello Gryvia", "memoryGiB": 1.0, "priority": 50,
                                  "args": ["--seconds", "3"], "mode": "exclusive", "timeoutSeconds": 60, "labels": ["demo"]])
    }
    /// The registered node that is this Mac (matched by host name), if any.
    var localNode: Node? {
        let host = ProcessInfo.processInfo.hostName.lowercased()
        let short = host.split(separator: ".").first.map(String.init) ?? host
        return state?.nodes.first { n in
            let h = (n.host ?? "").lowercased()
            return h == host || h.split(separator: ".").first.map(String.init) == short || n.id.lowercased() == short
        }
    }
    func setPolicy(node: Node, mode: String) async {
        var body: [String: Any] = ["id": node.id, "mode": mode]
        if let p = node.policy { body["idleMinutes"] = p.idleMinutes; body["allowOnBattery"] = p.allowOnBattery; body["minBatteryPct"] = p.minBatteryPct; body["pauseOnCall"] = p.pauseOnCall; body["preempt"] = p.preempt }
        await action("api/nodes/policy", body)
    }
    func bump(_ job: Job) async { await action("api/priority", ["id": job.id, "priority": min(100, job.spec.priority + 10)]) }
    func retry(_ job: Job) async {
        await action("api/jobs", ["runtime": job.spec.runtime, "model": job.spec.model ?? "", "memoryGiB": job.spec.memoryGiB,
                                  "priority": job.spec.priority, "args": job.spec.args, "mode": job.spec.mode, "tenant": job.tenant])
    }
    func clone(_ job: Job) {
        runtime = job.spec.runtime; model = job.spec.model ?? ""; memory = job.spec.memoryGiB; priority = job.spec.priority
        args = (try? String(data: JSONEncoder().encode(job.spec.args), encoding: .utf8)) ?? "[]"
    }
    func loadLogs(_ id: String) async {
        do { logs = try await api().logs(id) } catch { self.error = Self.hint(error) }
    }
    static func hint(_ e: Error) -> String {
        let m = e.localizedDescription
        if m.lowercased().contains("could not connect") || m.contains("timed out") { return "Cannot reach the server. Is it running? Try `make run-server`." }
        if m.lowercased().contains("unauthorized") || m.lowercased().contains("forbidden") { return "The server rejected this token. Use an admin token from credentials.env." }
        return m
    }
    /// UserNotifications traps when the process is not inside an app bundle (e.g. `swift run`).
    static let canNotify = Bundle.main.bundleIdentifier != nil

    private func notifyTransitions(_ snap: Snapshot) {
        for j in snap.jobs {
            if Self.canNotify, let prev = known[j.id], prev != j.state, ["succeeded", "failed"].contains(j.state) {
                let c = UNMutableNotificationContent()
                c.title = j.state == "succeeded" ? "Job finished" : "Job failed"
                c.body = (j.spec.model ?? j.spec.runtime) + " · " + String(j.id.prefix(10))
                UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: j.id, content: c, trigger: nil))
            }
            known[j.id] = j.state
        }
    }
}

struct GuideView: View {
    @Bindable var fleet: Fleet
    private let terms: [(String, String)] = [
        ("Mac / node", "One machine running the Gryvia agent. Listed under Fleet."),
        ("Workload / job", "One task: a runtime (mlx, llama, coreml, demo), a model label and arguments."),
        ("Reservation", "Memory a job sets aside on a Mac. Fairness bookkeeping, not a live GPU meter."),
        ("Queued → running → succeeded", "A job's life. Failed and cancelled are the other endings."),
        ("Tenant / quota", "A team and its memory allowance, so nobody starves the others."),
        ("Drain", "Stop sending new jobs to a Mac for maintenance. Running jobs finish; Resume undoes it."),
        ("Priority", "0–100. Higher goes first when jobs are waiting."),
    ]
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                Text("What Gryvia does").font(.title2.bold())
                Text("You have Apple Silicon Macs. Gryvia queues AI jobs and decides which Mac runs each one, without overloading memory. You watch and manage it from this app, the web dashboard or the command line.")
                    .foregroundStyle(.secondary)
                Button("Run a demo job now", systemImage: "play.fill") { Task { await fleet.runDemo() } }
                    .buttonStyle(.borderedProminent).disabled(fleet.token.isEmpty)
                Divider()
                Text("Words you will see").font(.headline)
                ForEach(terms, id: \.0) { t in
                    VStack(alignment: .leading, spacing: 2) { Text(t.0).bold(); Text(t.1).foregroundStyle(.secondary) }
                }
                Divider()
                Text("Common tasks").font(.headline)
                Text("• Run something: + Submit in the toolbar, or right-click an old job → Clone.\n• Something stuck: open Alerts; the job's reason says why it waits.\n• Maintenance: Fleet → Drain.\n• Command line: `gryvia.py doctor`, `gryvia.py top`.")
                    .foregroundStyle(.secondary)
            }.padding(28).frame(maxWidth: 720, alignment: .leading)
        }
    }
}


// MARK: - Live: GPUs and real-time scheduling

struct LiveView: View {
    @Bindable var fleet: Fleet
    @Namespace private var lanes

    private func hue(_ id: String) -> Color {
        var h = 0
        for u in id.unicodeScalars { h = (h &* 31 &+ Int(u.value)) % 360 }
        return Color(hue: Double(h) / 360, saturation: 0.65, brightness: 0.82)
    }
    private func reserved(_ node: Node) -> Double {
        (fleet.state?.jobs ?? []).filter { $0.node == node.id && ["running", "cancelling"].contains($0.state) }.reduce(0) { $0 + $1.spec.memoryGiB }
    }
    var online: [Node] { (fleet.state?.nodes ?? []).filter(\.online) }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                summary
                flow
                LazyVGrid(columns: [GridItem(.adaptive(minimum: 320), spacing: 14, alignment: .top)], alignment: .leading, spacing: 14) {
                    ForEach(fleet.state?.nodes ?? []) { gpuCard($0) }
                }
                decisions
            }.padding(24)
        }
        .onAppear { fleet.liveActive = true }
        .onDisappear { fleet.liveActive = false }
    }

    var summary: some View {
        let cores = online.reduce(0) { $0 + ($1.gpuCores ?? 0) }
        let budget = online.reduce(0) { $0 + $1.budgetGiB }
        let used = online.reduce(0) { $0 + reserved($1) }
        let utils = online.compactMap(\.gpuUtilPct)
        let avg = utils.isEmpty ? nil : utils.reduce(0, +) / Double(utils.count)
        return HStack(spacing: 14) {
            tile("\(online.count)", "Macs online")
            tile("\(online.count)", online.count == 1 ? "GPU (1 per Mac)" : "GPUs (1 per Mac)")
            tile(cores == 0 ? "—" : "\(cores)", "GPU cores")
            tile(String(format: "%.0f / %.0f", used, budget), "GiB reserved")
            tile(avg.map { String(format: "%.0f%%", $0) } ?? "—", "GPU busy, avg")
        }
    }
    func tile(_ value: String, _ label: String) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(value).font(.system(.title, design: .rounded, weight: .bold)).monospacedDigit()
            Text(label).font(.caption).foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, alignment: .leading).padding(14)
        .background(Color(nsColor: .controlBackgroundColor), in: .rect(cornerRadius: 14))
        .overlay(RoundedRectangle(cornerRadius: 14).strokeBorder(Color(nsColor: .separatorColor)))
    }

    func chip(_ job: Job, waiting: Bool) -> some View {
        VStack(alignment: .leading, spacing: 1) {
            Text(job.spec.model ?? job.spec.runtime).font(.caption.weight(.semibold))
            Text(String(format: "%.0f GiB · prio %d", job.spec.memoryGiB, job.spec.priority)).font(.caption2).opacity(0.85)
        }
        .padding(.horizontal, 10).padding(.vertical, 5)
        .background(hue(job.id).opacity(waiting ? 0.55 : 1), in: .rect(cornerRadius: 10))
        .foregroundStyle(.white)
        .matchedGeometryEffect(id: job.id, in: lanes)
    }

    var flow: some View {
        let jobs = fleet.state?.jobs ?? []
        let queued = jobs.filter { $0.state == "queued" }.sorted { ($0.spec.priority, $1.created) > ($1.spec.priority, $0.created) }
        return VStack(alignment: .leading, spacing: 10) {
            Text("Scheduling, live").font(.headline)
            HStack(alignment: .top, spacing: 16) {
                VStack(alignment: .leading, spacing: 8) {
                    Text("QUEUE").font(.caption2.weight(.bold)).foregroundStyle(.secondary)
                    if queued.isEmpty { Text("empty").foregroundStyle(.secondary).font(.caption) }
                    ForEach(queued) { chip($0, waiting: true) }
                }.frame(maxWidth: .infinity, minHeight: 110, alignment: .topLeading).padding(12)
                .background(.quaternary.opacity(0.5), in: .rect(cornerRadius: 12))
                VStack(spacing: 6) {
                    Text(queued.isEmpty ? "Queue empty" : "\(queued.count) waiting").font(.callout.bold())
                    Text(queued.first?.reason.isEmpty == false ? queued[0].reason : "admission: priority → fairness → fit")
                        .font(.caption).multilineTextAlignment(.center).foregroundStyle(.secondary)
                }
                .frame(width: 150, height: 110).padding(8)
                .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(style: StrokeStyle(lineWidth: 1.5, dash: [5])).foregroundStyle(.secondary))
                VStack(alignment: .leading, spacing: 8) {
                    Text("MACS").font(.caption2.weight(.bold)).foregroundStyle(.secondary)
                    ForEach(fleet.state?.nodes ?? []) { n in
                        HStack(spacing: 8) {
                            Text(n.id).font(.caption.bold()).frame(width: 90, alignment: .leading)
                            let mine = jobs.filter { $0.node == n.id && ["running", "cancelling"].contains($0.state) }
                            if mine.isEmpty { Text(n.online ? "idle" : "offline").font(.caption).foregroundStyle(.secondary) }
                            ForEach(mine) { chip($0, waiting: false) }
                            Spacer(minLength: 0)
                        }.padding(8).frame(maxWidth: .infinity, minHeight: 40, alignment: .leading)
                        .background(Color(nsColor: .controlBackgroundColor), in: .rect(cornerRadius: 8))
                    }
                }.frame(maxWidth: .infinity, alignment: .topLeading).padding(12)
                .background(.quaternary.opacity(0.5), in: .rect(cornerRadius: 12))
            }
        }
        .animation(.smooth(duration: 0.6), value: jobs.map { $0.id + $0.state + ($0.node ?? "") })
        .padding(16)
        .background(Color(nsColor: .controlBackgroundColor), in: .rect(cornerRadius: 18))
        .overlay(RoundedRectangle(cornerRadius: 18).strokeBorder(Color(nsColor: .separatorColor)))
    }

    func gpuCard(_ n: Node) -> some View {
        let cores = n.gpuCores ?? 0
        let util = n.gpuUtilPct
        let lit = (util ?? 0) / 100 * Double(cores)
        let samples = Array((fleet.telemetry[n.id] ?? []).suffix(60))
        let total = n.totalGiB ?? n.budgetGiB
        return VStack(alignment: .leading, spacing: 8) {
            HStack { Text(n.id).font(.headline); Spacer(); Text(n.online ? "Online" : "Offline").font(.caption).foregroundStyle(n.online ? .green : .secondary) }
            Text([n.gpuModel ?? n.gpu ?? "Apple GPU", cores > 0 ? "\(cores) GPU cores" : nil, n.totalGiB.map { String(format: "%.0f GiB unified memory", $0) }]
                .compactMap { $0 }.joined(separator: " · ")).font(.caption).foregroundStyle(.secondary)
            Text(util.map { String(format: "%.0f%%", $0) } ?? "—").font(.system(.largeTitle, design: .rounded, weight: .bold)).monospacedDigit()
                + Text("  GPU busy").font(.caption).foregroundColor(.secondary)
            if cores > 0 {
                LazyVGrid(columns: [GridItem(.adaptive(minimum: 12, maximum: 16), spacing: 3)], alignment: .leading, spacing: 3) {
                    ForEach(0..<cores, id: \.self) { i in
                        RoundedRectangle(cornerRadius: 2).fill(Double(i) < lit ? Color.accentColor : Color.secondary.opacity(0.18)).frame(height: 12)
                    }
                }.animation(.easeOut(duration: 0.4), value: Int(lit))
            }
            if samples.count > 1 {
                Chart(samples) { s in
                    AreaMark(x: .value("t", s.at), y: .value("busy", s.util)).foregroundStyle(Color.accentColor.opacity(0.15))
                    LineMark(x: .value("t", s.at), y: .value("busy", s.util)).foregroundStyle(Color.accentColor)
                }
                .chartYScale(domain: 0...100).chartXAxis(.hidden).chartYAxis(.hidden).frame(height: 48)
            } else {
                Text("Collecting samples…").font(.caption).foregroundStyle(.secondary)
            }
            GeometryReader { g in
                ZStack(alignment: .leading) {
                    RoundedRectangle(cornerRadius: 6).fill(Color.secondary.opacity(0.18))
                    HStack(spacing: 0) {
                        ForEach((fleet.state?.jobs ?? []).filter { $0.node == n.id && ["running", "cancelling"].contains($0.state) }) { j in
                            Rectangle().fill(hue(j.id)).frame(width: g.size.width * min(1, j.spec.memoryGiB / max(total, 1)))
                        }
                    }.clipShape(RoundedRectangle(cornerRadius: 6))
                    if let mib = n.gpuInUseMiB {
                        Rectangle().fill(.primary).frame(width: 2).offset(x: g.size.width * min(1, mib / 1024 / max(total, 1)))
                    }
                }
            }.frame(height: 10)
            Text(String(format: "%.1f GiB reserved", reserved(n)) + (n.gpuInUseMiB.map { String(format: " · %.1f GiB GPU in use", $0 / 1024) } ?? "") + (n.thermal.map { " · \($0)" } ?? ""))
                .font(.caption).foregroundStyle(.secondary)
        }
        .padding(16).frame(maxWidth: .infinity, alignment: .topLeading)
        .background(Color(nsColor: .controlBackgroundColor), in: .rect(cornerRadius: 18))
        .overlay(RoundedRectangle(cornerRadius: 18).strokeBorder(Color(nsColor: .separatorColor)))
    }

    var decisions: some View {
        let names = Dictionary(uniqueKeysWithValues: (fleet.state?.jobs ?? []).map { ($0.id, $0.spec.model ?? $0.spec.runtime) })
        let items = Array((fleet.state?.decisions ?? []).suffix(20).reversed())
        return VStack(alignment: .leading, spacing: 6) {
            Text("Scheduler decisions").font(.headline)
            if items.isEmpty { Text("No decisions yet.").foregroundStyle(.secondary) }
            ForEach(items) { d in
                HStack(alignment: .firstTextBaseline, spacing: 10) {
                    Text(Date(timeIntervalSince1970: d.at), style: .time).font(.caption).foregroundStyle(.secondary).monospacedDigit().frame(width: 70, alignment: .leading)
                    Text(d.kind.capitalized).font(.caption.bold()).foregroundStyle(d.kind == "admit" ? .green : d.kind == "wait" ? .orange : .primary).frame(width: 60, alignment: .leading)
                    Text("\(names[d.job ?? ""] ?? String((d.job ?? "").prefix(8))) — \(d.detail)").font(.callout)
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading).padding(16)
        .background(Color(nsColor: .controlBackgroundColor), in: .rect(cornerRadius: 18))
        .overlay(RoundedRectangle(cornerRadius: 18).strokeBorder(Color(nsColor: .separatorColor)))
    }
}


// MARK: - Model hub

struct ModelHubView: View {
    @Bindable var fleet: Fleet
    @State private var showOnboard = false

    private func latest(_ id: String, _ kind: String) -> ResultBody? {
        fleet.results.first { $0.model == id && $0.kind == kind && $0.body.success == true }?.body
    }
    private func job(_ m: ModelInfo, kind: String, runtime: String, args: [String]) async {
        await fleet.action("api/jobs", ["runtime": runtime, "kind": kind, "modelId": m.id, "model": "\(kind) \(m.id)",
                                        "memoryGiB": max(1.0, m.memoryGiB), "priority": 55, "mode": "exclusive", "args": args,
                                        "labels": [kind], "node": m.node ?? ""])
    }

    var body: some View {
        let models = fleet.state?.models ?? []
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                HStack {
                    Text("Bring a model onto your Macs, check it fits, benchmark it, and test it.").foregroundStyle(.secondary)
                    Spacer()
                    Button("Onboard a model", systemImage: "arrow.down.circle") { showOnboard = true }.buttonStyle(.borderedProminent)
                }
                if models.isEmpty {
                    ContentUnavailableView("No models yet", systemImage: "cube", description: Text("Press “Onboard a model” to download one onto a Mac."))
                }
                ForEach(models) { m in
                    VStack(alignment: .leading, spacing: 8) {
                        HStack {
                            Text(m.id).font(.headline)
                            Text((m.status ?? "ready").capitalized).font(.caption.weight(.semibold))
                                .padding(.horizontal, 8).padding(.vertical, 2)
                                .background((m.status == "failed" ? Color.red : m.status == "onboarding" ? Color.orange : Color.green).opacity(0.18), in: .capsule)
                            Spacer()
                            Text(String(format: "~%.1f GiB memory", m.memoryGiB)).font(.caption).foregroundStyle(.secondary)
                        }
                        Text([m.runtime, m.quant, m.paramsB.map { "\($0)B params" }, m.sizeBytes.map { String(format: "%.2f GiB on disk", $0 / 1_073_741_824) }, m.node]
                            .compactMap { $0 }.joined(separator: " · ")).font(.caption).foregroundStyle(.secondary)
                        if let b = latest(m.id, "benchmark") {
                            Text(String(format: "%.1f tok/s generation · %.0f tok/s prompt", b.generationTokensPerSecond ?? 0, b.promptTokensPerSecond ?? 0)).font(.callout.monospacedDigit())
                        }
                        if let e = latest(m.id, "evaluate") { Text("Evaluation: \(e.passed ?? 0)/\(e.total ?? 0) passed").font(.callout) }
                        HStack {
                            Button("Benchmark") { Task { await job(m, kind: "benchmark", runtime: "bench", args: ["--model", m.path ?? ""]) } }.disabled(!m.isReady)
                            Button("Quantise to Q4_K_M") { Task { await job(m, kind: "quantize", runtime: "quantize", args: ["--input", m.path ?? "", "--type", "Q4_K_M"]) } }.disabled(!m.isReady)
                            if m.runtime == "llama" && m.isReady {
                                Button("Deploy") { Task { await job(m, kind: "serve", runtime: "serve", args: ["--model", m.path ?? ""]) } }
                                    .disabled(m.status == "deployed")
                            }
                            Button("Copy path", systemImage: "doc.on.doc") { NSPasteboard.general.clearContents(); NSPasteboard.general.setString(m.path ?? "", forType: .string) }.disabled(m.path == nil)
                        }
                    }
                    .padding(16).frame(maxWidth: .infinity, alignment: .leading)
                    .background(Color(nsColor: .controlBackgroundColor), in: .rect(cornerRadius: 14))
                    .overlay(RoundedRectangle(cornerRadius: 14).strokeBorder(Color(nsColor: .separatorColor)))
                }
                if let ds = fleet.state?.datasets, !ds.isEmpty {
                    VStack(alignment: .leading, spacing: 6) {
                        Text("Datasets").font(.headline)
                        ForEach(ds) { d in
                            HStack {
                                Text(d.id).bold()
                                Text("\(d.status ?? "?") · \(d.rows.map { "\($0) rows" } ?? "rows unknown")").foregroundStyle(.secondary)
                                Spacer()
                                if let n = d.node { Text(n).font(.caption).foregroundStyle(.secondary) }
                            }.font(.callout)
                        }
                    }
                    .padding(16).frame(maxWidth: .infinity, alignment: .leading)
                    .background(Color(nsColor: .controlBackgroundColor), in: .rect(cornerRadius: 14))
                    .overlay(RoundedRectangle(cornerRadius: 14).strokeBorder(Color(nsColor: .separatorColor)))
                }
            }.padding(24)
        }
        .sheet(isPresented: $showOnboard) { OnboardSheet(fleet: fleet) }
    }
}

struct OnboardSheet: View {
    @Bindable var fleet: Fleet
    @Environment(\.dismiss) private var dismiss
    @State private var id = ""
    @State private var repo = ""
    @State private var file = ""
    @State private var params = ""
    @State private var quant = "Q4_K_M"
    @State private var maxGiB = 8.0

    var estimate: String {
        guard let p = Double(params), p > 0 else { return "Enter parameters to see a memory estimate" }
        let q = quant.lowercased()
        let table: [(String, Double)] = [("q2", 2.8), ("q3", 3.6), ("q4", 4.8), ("q5", 5.7), ("q6", 6.6), ("q8", 8.5), ("f16", 16), ("f32", 32)]
        let bits = table.first { q.contains($0.0) }?.1 ?? 16
        return String(format: "~%.1f GiB resident (estimate, not measured)", p * 1e9 * bits / 8 * 1.15 / 1_073_741_824 + 0.75)
    }
    var body: some View {
        Form {
            TextField("Model ID", text: $id, prompt: Text("llama-3.2-1b-q4"))
            TextField("Hugging Face repo", text: $repo, prompt: Text("owner/name"))
            TextField("File", text: $file, prompt: Text("model.Q4_K_M.gguf"))
            TextField("Parameters (billions)", text: $params)
            TextField("Quantisation", text: $quant)
            TextField("Download cap (GiB)", value: $maxGiB, format: .number)
            LabeledContent("Memory", value: estimate)
            Text("The file is saved on a Mac under its managed models folder and its SHA-256 is recorded. Only huggingface.co is accepted.")
                .font(.caption).foregroundStyle(.secondary)
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }
                Button("Start download") {
                    Task {
                        var source: [String: Any] = ["kind": "hf", "repo": repo, "file": file, "maxGiB": maxGiB]
                        source["revision"] = "main"
                        var body: [String: Any] = ["id": id, "source": source, "runtime": "llama", "quant": quant]
                        if let p = Double(params) { body["paramsB"] = p }
                        await fleet.action("api/models/onboard", body)
                        if fleet.error.isEmpty { dismiss() }
                    }
                }.buttonStyle(.borderedProminent).disabled(id.isEmpty || repo.isEmpty || file.isEmpty)
            }
        }.formStyle(.grouped).padding().frame(width: 500)
    }
}


// MARK: - Training and Deployments

struct TrainingView: View {
    @Bindable var fleet: Fleet
    @State private var selected: String?
    @State private var metrics: [String: [[Double]]] = [:]
    @State private var showNew = false

    var runs: [Job] { (fleet.state?.jobs ?? []).filter { $0.spec.kind == "train" } }
    func adapter(for job: Job) -> ModelInfo? { (fleet.state?.models ?? []).first { $0.trainJob == job.id } }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                HStack {
                    Text("LoRA fine-tuning on one Mac with MLX. Loss updates as MLX reports it.").foregroundStyle(.secondary)
                    Spacer()
                    Button("New fine-tune", systemImage: "plus") { showNew = true }.buttonStyle(.borderedProminent)
                }
                if runs.isEmpty {
                    ContentUnavailableView("No training runs yet", systemImage: "chart.line.uptrend.xyaxis",
                                           description: Text("Add a dataset in the web dashboard (or `gryvia.py dataset-add`), then start a fine-tune."))
                }
                ForEach(runs) { j in
                    Button { selected = j.id; Task { await load(j.id) } } label: {
                        HStack {
                            VStack(alignment: .leading, spacing: 2) {
                                Text(j.spec.modelId ?? "model").font(.headline)
                                Text("\(String(j.id.prefix(10))) · dataset \(j.spec.datasetId ?? "—")").font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            if let a = adapter(for: j), let v = a.finalValLoss { Text(String(format: "val loss %.3f", v)).font(.callout.monospacedDigit()) }
                            StateBadge(state: j.state)
                        }
                        .padding(14).frame(maxWidth: .infinity)
                        .background(Color(nsColor: selected == j.id ? .selectedContentBackgroundColor : .controlBackgroundColor).opacity(selected == j.id ? 0.25 : 1), in: .rect(cornerRadius: 14))
                        .overlay(RoundedRectangle(cornerRadius: 14).strokeBorder(Color(nsColor: .separatorColor)))
                    }.buttonStyle(.plain)
                }
                if selected != nil { chart }
            }.padding(24)
        }
        .task(id: selected) {
            while !Task.isCancelled, let id = selected { await load(id); try? await Task.sleep(for: .seconds(3)) }
        }
        .sheet(isPresented: $showNew) { NewTrainingSheet(fleet: fleet) }
    }

    func load(_ id: String) async {
        if let api = try? fleet.api(), let m = try? await api.runMetrics(id) { metrics = m }
    }

    struct Point: Identifiable { let id = UUID(); let step: Double; let value: Double; let series: String }
    var points: [Point] {
        var out: [Point] = []
        for (key, label) in [("trainLoss", "Train loss"), ("valLoss", "Validation loss")] {
            for p in metrics[key] ?? [] where p.count == 2 { out.append(Point(step: p[0], value: p[1], series: label)) }
        }
        return out
    }
    var chart: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Loss").font(.headline)
            if points.isEmpty { Text("No loss reported yet.").foregroundStyle(.secondary) }
            else {
                Chart(points) { p in
                    LineMark(x: .value("Step", p.step), y: .value("Loss", p.value)).foregroundStyle(by: .value("Series", p.series))
                    PointMark(x: .value("Step", p.step), y: .value("Loss", p.value)).foregroundStyle(by: .value("Series", p.series))
                }.frame(height: 240)
            }
        }
        .padding(16).frame(maxWidth: .infinity, alignment: .leading)
        .background(Color(nsColor: .controlBackgroundColor), in: .rect(cornerRadius: 14))
        .overlay(RoundedRectangle(cornerRadius: 14).strokeBorder(Color(nsColor: .separatorColor)))
    }
}

struct NewTrainingSheet: View {
    @Bindable var fleet: Fleet
    @Environment(\.dismiss) private var dismiss
    @State private var base = "mlx-community/SmolLM-135M-Instruct-4bit"
    @State private var dataset = ""
    @State private var iters = 100
    @State private var batch = 2
    @State private var layers = 8
    @State private var lr = 0.00001
    var datasets: [DatasetInfo] { (fleet.state?.datasets ?? []).filter { ($0.status ?? "") == "ready" } }
    var body: some View {
        Form {
            TextField("Base model (MLX repo or local path)", text: $base)
            Picker("Dataset", selection: $dataset) {
                if datasets.isEmpty { Text("Add a dataset first").tag("") }
                ForEach(datasets) { d in Text("\(d.id) (\(d.rows.map(String.init) ?? "?") rows)").tag(d.id) }
            }
            Stepper("Iterations \(iters)", value: $iters, in: 1...5000, step: 10)
            Stepper("Batch size \(batch)", value: $batch, in: 1...32)
            Stepper("LoRA layers \(layers)", value: $layers, in: -1...64)
            TextField("Learning rate", value: $lr, format: .number)
            Text("Training runs on this Mac's single GPU and shows up in Live and Workloads like any job.").font(.caption).foregroundStyle(.secondary)
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }
                Button("Start training") {
                    Task {
                        let id = base.map { $0.isLetter || $0.isNumber || "._-".contains($0) ? String($0) : "__" }.joined()
                        if !(fleet.state?.models ?? []).contains(where: { $0.id == id }) {
                            await fleet.action("api/models", ["id": id, "runtime": "mlx", "memoryGiB": 6.0, "source": base, "status": "ready"])
                        }
                        let node = datasets.first { $0.id == dataset }?.node ?? ""
                        await fleet.action("api/jobs", ["runtime": "train", "kind": "train", "modelId": id, "datasetId": dataset, "model": "train \(id)",
                            "memoryGiB": 6.0, "priority": 60, "mode": "exclusive", "timeoutSeconds": 21600, "labels": ["train"], "node": node,
                            "args": ["--model", base, "--data", "{{dataset.path}}", "--iters", String(iters), "--batch-size", String(batch),
                                     "--num-layers", String(layers), "--learning-rate", String(lr)]])
                        if fleet.error.isEmpty { dismiss() }
                    }
                }.buttonStyle(.borderedProminent).disabled(dataset.isEmpty || base.isEmpty)
            }
        }.formStyle(.grouped).padding().frame(width: 520)
        .onAppear { if dataset.isEmpty { dataset = datasets.first?.id ?? "" } }
    }
}

struct DeploymentsView: View {
    @Bindable var fleet: Fleet
    var models: [ModelInfo] {
        (fleet.state?.models ?? []).filter { ($0.runtime == "llama" || ($0.runtime == "mlx" && $0.kind == "fused")) && ($0.path ?? "") != "" }
    }
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                Text("Models served by llama-server or mlx_lm.server on a Mac. Replicas bind to loopback and are reachable from the control-plane Mac.")
                    .foregroundStyle(.secondary)
                if models.isEmpty {
                    ContentUnavailableView("Nothing to deploy yet", systemImage: "arrow.up.forward.app", description: Text("Onboard a GGUF model in the Model hub first."))
                }
                ForEach(models) { m in
                    let live = !(m.endpoints ?? []).isEmpty
                    HStack {
                        VStack(alignment: .leading, spacing: 2) {
                            Text(m.id).font(.headline)
                            Text(live ? (m.endpoints?.last ?? "") : "Stopped").font(.caption.monospaced()).foregroundStyle(.secondary).textSelection(.enabled)
                        }
                        Spacer()
                        Text(live ? "Serving" : "Stopped").font(.caption.weight(.semibold))
                            .padding(.horizontal, 8).padding(.vertical, 2)
                            .background((live ? Color.green : Color.secondary).opacity(0.18), in: .capsule)
                        if live {
                            Button("Stop") { Task { await fleet.action("api/cancel", ["id": m.deployJob ?? ""]) } }
                        } else {
                            Button("Deploy") {
                                Task {
                                    var args = ["--model", m.path ?? ""]
                                    if m.runtime == "mlx" { args += ["--engine", "mlx"] }
                                    await fleet.action("api/jobs", ["runtime": "serve", "kind": "serve", "modelId": m.id, "model": "serve \(m.id)",
                                        "memoryGiB": max(1.0, m.memoryGiB), "priority": 70, "mode": "shared", "timeoutSeconds": 604800,
                                        "args": args, "labels": ["serve"], "node": m.node ?? ""])
                                }
                            }.buttonStyle(.borderedProminent)
                        }
                    }
                    .padding(14).frame(maxWidth: .infinity)
                    .background(Color(nsColor: .controlBackgroundColor), in: .rect(cornerRadius: 14))
                    .overlay(RoundedRectangle(cornerRadius: 14).strokeBorder(Color(nsColor: .separatorColor)))
                }
            }.padding(24)
        }
    }
}

struct FleetAlert: Identifiable {
    enum Level { case error, warn, info }
    let id = UUID(); let level: Level; let title: String; let detail: String
}

struct StateBadge: View {
    let state: String
    var color: Color {
        switch state { case "running": .blue; case "queued": .orange; case "succeeded": .green; case "failed": .red; default: .secondary }
    }
    var body: some View {
        Label(state.capitalized, systemImage: JobStateStyle.symbol(state))
            .font(.caption.weight(.semibold)).foregroundStyle(color)
            .padding(.horizontal, 8).padding(.vertical, 3)
            .background(color.opacity(0.14), in: .capsule)
    }
}

struct Metric: View {
    let label: String, value: Int, symbol: String
    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Label(label, systemImage: symbol).foregroundStyle(.secondary)
            Text("\(value)").font(.system(.largeTitle, design: .rounded, weight: .bold)).monospacedDigit().contentTransition(.numericText())
        }
        .frame(maxWidth: .infinity, alignment: .leading).padding(16)
        .background(Color(nsColor: .controlBackgroundColor), in: .rect(cornerRadius: 16))
        .overlay(RoundedRectangle(cornerRadius: 16).strokeBorder(Color(nsColor: .separatorColor)))
    }
}

struct ContentView: View {
    /// Read the icon from the bundle directly: NSApp.applicationIconImage can serve a stale LaunchServices cache.
    static let brandIcon: NSImage = {
        if let url = Bundle.main.url(forResource: "Gryvia", withExtension: "icns"), let image = NSImage(contentsOf: url) { return image }
        return NSApp.applicationIconImage
    }()
    @Bindable var fleet: Fleet
    @State private var section: Pane? = Pane(rawValue: UserDefaults.standard.string(forKey: "startSection") ?? "") ?? .live
    @AppStorage("welcomeHidden") private var welcomeHidden = false
    @State private var filter = ""
    @State private var selected: Job?
    @State private var showSubmit = false

    var body: some View {
        NavigationSplitView {
            List(selection: $section) {
                Label(Pane.guide.rawValue, systemImage: Pane.guide.symbol).tag(Pane.guide)
                ForEach(Pane.groups, id: \.0) { group in
                    Section(group.0) {
                        ForEach(group.1) { s in
                            HStack {
                                Label(s.rawValue, systemImage: s.symbol)
                                if s == .alerts, !fleet.alerts.isEmpty { Spacer(); Text("\(fleet.alerts.count)").font(.caption2.bold()).padding(.horizontal, 6).background(.red, in: .capsule).foregroundStyle(.white) }
                            }.tag(s)
                        }
                    }
                }
            }
            .safeAreaInset(edge: .top) {
                HStack(spacing: 10) {
                    Image(nsImage: Self.brandIcon).resizable().frame(width: 32, height: 32)
                    Text("Gryvia").font(.title3.bold())
                    Spacer()
                }.padding(.horizontal, 14).padding(.top, 6)
            }
                .navigationTitle("Gryvia")
                .safeAreaInset(edge: .bottom) { connectionFooter }
        } detail: {
            VStack(spacing: 0) {
                if !fleet.error.isEmpty {
                    Label(fleet.error, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
                        .frame(maxWidth: .infinity, alignment: .leading).padding(12).background(.red.opacity(0.1))
                }
                if fleet.token.isEmpty && section != .guide { connectPanel } else { content }
            }
            .toolbar {
                ToolbarItem { Button("Run demo job", systemImage: "play.fill") { Task { await fleet.runDemo() } }.disabled(fleet.token.isEmpty).help("Send a 3-second test job to your Macs") }
                ToolbarItem { Button("Submit", systemImage: "plus") { showSubmit = true }.disabled(fleet.token.isEmpty).help("Submit a workload") }
                ToolbarItem { Button("Refresh", systemImage: "arrow.clockwise") { Task { await fleet.refresh() } } }
            }
            .searchable(text: $filter, prompt: "Filter jobs, nodes, tenants")
        }
        .frame(minWidth: 960, minHeight: 640)
        .sheet(isPresented: $showSubmit) { SubmitSheet(fleet: fleet) }
        .sheet(item: $selected) { job in JobDetail(fleet: fleet, job: job) }
    }

    var connectionFooter: some View {
        HStack(spacing: 8) {
            Circle().fill(fleet.connected ? .green : .secondary).frame(width: 8, height: 8)
            VStack(alignment: .leading, spacing: 0) {
                Text(fleet.connected ? "Connected" : "Not connected").font(.caption.weight(.medium)).lineLimit(1)
                if let t = fleet.lastUpdated { Text(t, style: .relative).font(.caption2).foregroundStyle(.secondary).lineLimit(1) }
            }
            Spacer()
        }.padding(12)
    }

    var connectPanel: some View {
        ContentUnavailableView {
            Label("Connect to a Gryvia server", systemImage: "network")
        } description: {
            VStack(spacing: 10) {
                Text("Paste an admin token. It is stored in your login Keychain.").foregroundStyle(.secondary)
                TextField("Server origin", text: $fleet.server).textFieldStyle(.roundedBorder).frame(width: 320)
                SecureField("Admin token", text: $fleet.token).textFieldStyle(.roundedBorder).frame(width: 320)
            }
        }
    }

    @ViewBuilder var content: some View {
        switch section ?? .live {
        case .overview: overview
        case .fleet: fleetList
        case .workloads: workloads
        case .tenants: tenants
        case .alerts: alertList
        case .guide: GuideView(fleet: fleet)
        case .live: LiveView(fleet: fleet)
        case .models: ModelHubView(fleet: fleet)
        case .training: TrainingView(fleet: fleet)
        case .deployments: DeploymentsView(fleet: fleet)
        }
    }

    var overview: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                if showWelcome { welcomeCard }
                HStack(spacing: 16) {
                    Metric(label: "Macs online", value: fleet.state?.nodes.filter(\.online).count ?? 0, symbol: "desktopcomputer")
                    Metric(label: "Running", value: fleet.running, symbol: "play.circle")
                    Metric(label: "Queued", value: fleet.queued, symbol: "clock")
                    Metric(label: "Completed", value: fleet.state?.jobs.filter { $0.state == "succeeded" }.count ?? 0, symbol: "checkmark.circle")
                }
                Text("Recent workloads").font(.headline)
                jobRows(Array((fleet.state?.jobs ?? []).prefix(8)))
            }.padding(24)
        }
    }

    static func nodeDetail(_ n: Node) -> String {
        var text = String(format: "%.1f GiB", n.budgetGiB) + " · " + n.runtimes.joined(separator: ", ")
        if let thermal = n.thermal { text += " · thermal " + thermal }
        return text
    }

    var showWelcome: Bool {
        guard !welcomeHidden, let st = fleet.state else { return false }
        return !(st.nodes.contains(where: \.online) && !st.jobs.isEmpty && st.jobs.contains { $0.state == "succeeded" })
    }

    var welcomeCard: some View {
        let st = fleet.state
        let hasNode = st?.nodes.contains(where: \.online) ?? false
        let hasJob = !(st?.jobs.isEmpty ?? true)
        let done = st?.jobs.contains { $0.state == "succeeded" } ?? false
        func step(_ ok: Bool, _ title: String, _ detail: String) -> some View {
            HStack(alignment: .top, spacing: 10) {
                Image(systemName: ok ? "checkmark.circle.fill" : "circle").foregroundStyle(ok ? .green : .secondary)
                VStack(alignment: .leading, spacing: 2) { Text(title).bold(); Text(detail).foregroundStyle(.secondary) }
            }
        }
        return VStack(alignment: .leading, spacing: 12) {
            HStack { Text("Welcome to Gryvia: three steps to your first job").font(.headline); Spacer(); Button("Hide") { welcomeHidden = true } }
            step(hasNode, "1. Connect a Mac", "Run the agent on a Mac: `make run-agent` here, or `make deploy` for another Mac. It appears under Fleet.")
            step(hasJob, "2. Run a job", "Press “Run demo job” in the toolbar. It sends a 3-second test workload.")
            step(done, "3. Watch it finish", "Open Workloads: queued → running → succeeded. Click a job to read its logs.")
            Button("Run demo job") { Task { await fleet.runDemo() } }.buttonStyle(.borderedProminent).disabled(!hasNode)
        }
        .padding(16).background(.tint.opacity(0.08), in: .rect(cornerRadius: 16))
    }

    var fleetList: some View {
        List((fleet.state?.nodes ?? []).filter { filter.isEmpty || $0.id.localizedCaseInsensitiveContains(filter) }) { n in
            HStack {
                Image(systemName: "desktopcomputer").foregroundStyle(n.online ? .green : .secondary)
                VStack(alignment: .leading) {
                    Text(n.id).bold()
                    Text(Self.nodeDetail(n))
                        .font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                if let g = n.gate, !g.allowed { Text(g.reason).font(.caption).foregroundStyle(.orange) }
                Picker("Work", selection: Binding(get: { n.policy?.mode ?? "always" }, set: { m in Task { await fleet.setPolicy(node: n, mode: m) } })) {
                    Text("Always").tag("always"); Text("When I'm away").tag("idle"); Text("Paused").tag("paused")
                }.labelsHidden().frame(width: 140)
                Text(n.drain ? "Draining" : (n.online ? "Online" : "Offline")).foregroundStyle(.secondary)
                Button(n.drain ? "Resume" : "Drain") { Task { await fleet.action("api/drain", ["id": n.id, "drain": !n.drain]) } }
            }
        }
    }

    var workloads: some View {
        let jobs = (fleet.state?.jobs ?? []).filter {
            filter.isEmpty || ($0.spec.model ?? $0.spec.runtime).localizedCaseInsensitiveContains(filter)
                || $0.id.localizedCaseInsensitiveContains(filter) || $0.tenant.localizedCaseInsensitiveContains(filter)
                || $0.state.localizedCaseInsensitiveContains(filter)
        }
        return ScrollView { jobRows(jobs).padding(24) }
    }

    func jobRows(_ jobs: [Job]) -> some View {
        VStack(spacing: 0) {
            if jobs.isEmpty { Text("No workloads yet. Press + to submit one.").foregroundStyle(.secondary).padding() }
            ForEach(jobs) { j in
                HStack {
                    VStack(alignment: .leading) {
                        Text(j.spec.model ?? j.spec.runtime).bold()
                        Text(String(j.id.prefix(10)) + " · " + j.tenant + (j.reason.isEmpty ? "" : " · " + j.reason))
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    StateBadge(state: j.state)
                    Text("\(j.spec.memoryGiB, specifier: "%.1f") GiB").foregroundStyle(.secondary).monospacedDigit()
                    if ["queued", "running"].contains(j.state) {
                        Button("Cancel") { Task { await fleet.action("api/cancel", ["id": j.id]) } }
                    }
                }
                .padding(.vertical, 8).contentShape(Rectangle()).onTapGesture { selected = j }
                .contextMenu {
                    Button("Show logs") { selected = j }
                    if j.state == "queued" { Button("Raise priority +10") { Task { await fleet.bump(j) } } }
                    Button("Clone into submit form") { fleet.clone(j); showSubmit = true }
                    if ["failed", "cancelled", "succeeded"].contains(j.state) { Button("Retry") { Task { await fleet.retry(j) } } }
                    if ["queued", "running"].contains(j.state) { Button("Cancel", role: .destructive) { Task { await fleet.action("api/cancel", ["id": j.id]) } } }
                    Button("Copy job ID") { NSPasteboard.general.clearContents(); NSPasteboard.general.setString(j.id, forType: .string) }
                }
                Divider()
            }
        }
    }

    var alertList: some View {
        let items = fleet.alerts
        return Group {
            if items.isEmpty {
                ContentUnavailableView("All clear", systemImage: "checkmark.seal", description: Text("No offline Macs, thermal limits or failed jobs."))
            } else {
                List(items) { a in
                    HStack {
                        Image(systemName: a.level == .error ? "exclamationmark.octagon.fill" : "info.circle.fill").foregroundStyle(a.level == .error ? .red : .secondary)
                        VStack(alignment: .leading) { Text(a.title).bold(); Text(a.detail).font(.caption).foregroundStyle(.secondary) }
                    }
                }
            }
        }
    }

    var tenants: some View {
        List(fleet.state?.tenants ?? []) { t in
            let detail = String(format: "Quota %.1f GiB · weight %.1f", t.quota, t.weight)
            HStack { Text(t.id).bold(); Spacer(); Text(detail).foregroundStyle(.secondary) }
        }
    }
}

struct JobDetail: View {
    @Bindable var fleet: Fleet
    let job: Job
    @Environment(\.dismiss) private var dismiss
    @State private var query = ""
    var shown: [LogLine] { query.isEmpty ? fleet.logs : fleet.logs.filter { $0.text.localizedCaseInsensitiveContains(query) } }
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Text(job.spec.model ?? job.spec.runtime).font(.title2.bold()); StateBadge(state: job.state); Spacer()
                TextField("Search logs", text: $query).textFieldStyle(.roundedBorder).frame(width: 180)
                Button("Copy") { NSPasteboard.general.clearContents(); NSPasteboard.general.setString(shown.map(\.text).joined(separator: "\n"), forType: .string) }
                Button("Done") { dismiss() }
            }
            Text("\(job.id) · tenant \(job.tenant) · node \(job.node ?? "—") · attempt \(job.attempt)").font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
            ScrollView {
                Text(shown.isEmpty ? "No log output yet." : shown.map(\.text).joined(separator: "\n"))
                    .font(.system(.footnote, design: .monospaced)).textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading).padding(10)
            }.background(.quaternary, in: .rect(cornerRadius: 10))
        }
        .padding(20).frame(width: 640, height: 460)
        .task { while !Task.isCancelled { await fleet.loadLogs(job.id); try? await Task.sleep(for: .seconds(2)) } }
    }
}

struct SubmitSheet: View {
    @Bindable var fleet: Fleet
    @Environment(\.dismiss) private var dismiss
    var body: some View {
        Form {
            Picker("Runtime", selection: $fleet.runtime) { ForEach(["demo", "mlx", "llama", "mps", "coreml"], id: \.self) { Text($0) } }
            TextField("Model label", text: $fleet.model)
            TextField("Memory reservation (GiB)", value: $fleet.memory, format: .number)
            Stepper("Priority \(fleet.priority)", value: $fleet.priority, in: 0...100)
            TextField("Arguments (JSON array)", text: $fleet.args)
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }
                Button("Submit") { Task { await fleet.submit(); if fleet.error.isEmpty { dismiss() } } }
                    .buttonStyle(.borderedProminent).keyboardShortcut(.defaultAction)
            }
        }.formStyle(.grouped).padding().frame(width: 460)
    }
}

struct SettingsView: View {
    @AppStorage("accent") private var accent = "blue"
    @AppStorage("refreshSeconds") private var refresh = 5
    var body: some View {
        Form {
            Picker("Accent", selection: $accent) {
                ForEach(Brand.accents, id: \.name) { a in
                    Label(a.name.capitalized, systemImage: "circle.fill").foregroundStyle(Color(hex: a.hex)).tag(a.name)
                }
            }
            Picker("Refresh every", selection: $refresh) { ForEach([2, 5, 15, 60], id: \.self) { Text("\($0) s").tag($0) } }
            Section("About") {
                Text("Gryvia Apple 0.3.0 · Business Source License 1.1")
                Text("Copyright © 2026 Zyvor AI Labs Private Limited").foregroundStyle(.secondary)
            }
        }.formStyle(.grouped).frame(width: 420).padding()
    }
}

@main struct GryviaDesktop: App {
    @State private var fleet = Fleet()
    @AppStorage("accent") private var accent = "blue"
    @AppStorage("refreshSeconds") private var refreshSeconds = 5
    var body: some Scene {
        WindowGroup {
            ContentView(fleet: fleet)
                .tint(Color.accent(named: accent))
                .onAppear { NSApplication.shared.applicationIconImage = ContentView.brandIcon }   // Dock tile: do not depend on the LaunchServices icon cache
                .task {
                    if Fleet.canNotify { _ = try? await UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) }
                    while !Task.isCancelled {
                        if !fleet.token.isEmpty { await fleet.refresh() }
                        try? await Task.sleep(for: .seconds(fleet.liveActive ? 2 : refreshSeconds))
                    }
                }
        }
        Settings { SettingsView() }
        MenuBarExtra {
            Text("\(fleet.running) running · \(fleet.queued) queued")
            Divider()
            ForEach(fleet.state?.nodes ?? []) { n in
                Button((n.online ? "● " : "○ ") + n.id + (n.drain ? " (draining)" : "")) { Task { await fleet.action("api/drain", ["id": n.id, "drain": !n.drain]) } }
            }
            if fleet.state?.nodes.isEmpty == false { Divider() }
            if let me = fleet.localNode {
                let paused = me.policy?.mode == "paused"
                Button(paused ? "Resume fleet work on this Mac" : "Pause fleet work on this Mac") {
                    Task { await fleet.setPolicy(node: me, mode: paused ? "always" : "paused") }
                }
                if let gate = me.gate, !gate.allowed { Text("Not accepting work: \(gate.reason)").font(.caption) }
                Divider()
            }
            Button("Refresh") { Task { await fleet.refresh() } }
            Button("Quit Gryvia") { NSApplication.shared.terminate(nil) }
        } label: {
            Label("\(fleet.running)", systemImage: "cpu")
        }
    }
}
#else
import Foundation
@main struct Unsupported { static func main() { print("GryviaDesktop requires macOS 15 or later") } }
#endif
