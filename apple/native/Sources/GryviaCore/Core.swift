// SPDX-License-Identifier: BUSL-1.1
// Copyright © 2026 Zyvor AI Labs Private Limited
import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif
#if canImport(Metal)
import Metal
#endif
#if canImport(IOKit)
import IOKit
#endif

public enum GryviaError: Error, LocalizedError {
    case invalid(String)
    public var errorDescription: String? { if case let .invalid(s) = self { return s }; return nil }
}
public struct Spec: Codable, Sendable {
    public let runtime: String
    public let memoryGiB: Double
    public let priority: Int
    public let retries: Int
    public let timeoutSeconds: Int
    public let model: String?
    public let args: [String]
    public let mode: String
    public let kind: String?
    public let modelId: String?
    public let datasetId: String?
}
public struct Claim: Codable, Sendable { public let id: String; public let lease: String; public let spec: Spec; public let expires: Double }
public struct ClaimResponse: Codable, Sendable { public let job: Claim? }
public struct Renewal: Codable, Sendable { public let cancel: Bool; public let expires: Double; public let reason: String? }
public struct Job: Codable, Sendable, Identifiable {
    public let id: String; public let tenant: String; public let state: String; public let created: Double
    public let started: Double?; public let finished: Double?
    public let node: String?; public let attempt: Int; public let reason: String; public let spec: Spec
}
public struct Node: Codable, Sendable, Identifiable {
    public let id: String; public let budgetGiB: Double; public let runtimes: [String]
    public let online: Bool; public let drain: Bool; public let thermal: String?
    public let gpu: String?; public let gpuModel: String?; public let gpuCores: Int?; public let gpuUtilPct: Double?; public let gpuInUseMiB: Double?
    public let cpuPerformanceCores: Int?; public let cpuEfficiencyCores: Int?; public let totalGiB: Double?
}
public struct Tenant: Codable, Sendable, Identifiable { public let id: String; public let quota: Double; public let weight: Double }
public struct TelemetrySample: Codable, Sendable, Identifiable { public let at: Double; public let util: Double; public let inUseGiB: Double; public let reservedGiB: Double; public var id: Double { at } }
public struct ModelInfo: Codable, Sendable, Identifiable {
    public let id: String; public let runtime: String; public let memoryGiB: Double
    public let status: String?; public let quant: String?; public let paramsB: Double?; public let source: String?
    public let path: String?; public let node: String?; public let sizeBytes: Double?; public let sha256: String?
    public let kind: String?; public let parent: String?; public let endpoints: [String]?; public let deployJob: String?
    public let finalValLoss: Double?; public let trainJob: String?
    public var isReady: Bool { (status ?? "ready") == "ready" && !(path ?? "").isEmpty }
}
public struct ResultBody: Codable, Sendable {
    public let success: Bool?
    public let promptTokensPerSecond: Double?; public let generationTokensPerSecond: Double?
    public let passed: Int?; public let total: Int?; public let bytes: Double?
}
public struct ResultRow: Codable, Sendable, Identifiable {
    public let id: Int; public let at: Double; public let model: String; public let kind: String; public let node: String; public let body: ResultBody
}
public struct DatasetInfo: Codable, Sendable, Identifiable { public let id: String; public let status: String?; public let rows: Int?; public let bytes: Double?; public let node: String? }
public struct Decision: Codable, Sendable, Identifiable { public let seq: Int; public let at: Double; public let job: String?; public let node: String?; public let kind: String; public let detail: String; public var id: Int { seq } }
public struct Snapshot: Codable, Sendable { public let jobs: [Job]; public let nodes: [Node]; public let tenants: [Tenant]; public let decisions: [Decision]?; public let models: [ModelInfo]?; public let datasets: [DatasetInfo]? }
public struct LogLine: Codable, Sendable, Identifiable {
    public let seq: Int; public let at: Double; public let text: String
    public var id: Int { seq }
}
public enum JobStateStyle {
    /// SF Symbol + semantic colour name for a job state; shared by app and tests.
    public static func symbol(_ state: String) -> String {
        switch state {
        case "running": "play.circle.fill"
        case "queued": "clock.fill"
        case "succeeded": "checkmark.circle.fill"
        case "failed": "xmark.octagon.fill"
        case "cancelled", "canceled": "minus.circle.fill"
        default: "questionmark.circle"
        }
    }
}
public struct API: Sendable {
    public let base: URL
    public let token: String
    public init(base: String, token: String) throws {
        guard let u = URL(string: base), let scheme=u.scheme, ["http", "https"].contains(scheme), u.host != nil, u.user == nil, u.password == nil, u.query == nil, u.fragment == nil, u.path == "" || u.path == "/" else { throw GryviaError.invalid("Use a server origin URL") }
        if scheme == "http" && !["localhost","127.0.0.1","::1"].contains(u.host!) { throw GryviaError.invalid("Remote servers require HTTPS") }
        guard token.count >= 24 else { throw GryviaError.invalid("Token must have at least 24 characters") }
        self.base=u; self.token=token
    }
    public func request(_ path: String, body: sending [String: Any]? = nil) async throws -> Data {
        let url=base.appendingPathComponent(path)
        var req=URLRequest(url: url); req.timeoutInterval=2
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        if let body=body { req.httpMethod="POST"; req.setValue("application/json", forHTTPHeaderField: "Content-Type"); req.httpBody=try JSONSerialization.data(withJSONObject: body) }
        let (data,response)=try await URLSession.shared.data(for: req)
        guard let http=response as? HTTPURLResponse, (200..<300).contains(http.statusCode) else {
            let message=(try? JSONSerialization.jsonObject(with:data) as? [String:String])?["error"] ?? "Request failed"
            throw GryviaError.invalid(message)
        }
        return data
    }
    public func logs(_ id: String) async throws -> [LogLine] {
        let encoded=id.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? id
        return try JSONDecoder().decode([LogLine].self, from: await request("api/logs/"+encoded))
    }
    public func results() async throws -> [ResultRow] {
        try JSONDecoder().decode([ResultRow].self, from: await request("api/results"))
    }
    /// Loss/throughput history of a training run: key -> [[step, value]].
    public func runMetrics(_ job: String) async throws -> [String: [[Double]]] {
        try JSONDecoder().decode([String: [[Double]]].self, from: await request("api/runs/" + job + "/metrics"))
    }
    public func telemetry() async throws -> [String: [TelemetrySample]] {
        try JSONDecoder().decode([String: [TelemetrySample]].self, from: await request("api/telemetry"))
    }
    public func state() async throws -> Snapshot { try JSONDecoder().decode(Snapshot.self, from: await request("api/state")) }
}
/// Apple Silicon exposes exactly one GPU per Mac, with N GPU cores. Figures come from the AGXAccelerator
/// IORegistry entry (no sudo). Utilisation is device-wide: it includes other apps, not just Gryvia jobs.
public struct GPUInfo: Equatable, Sendable {
    public var model: String
    public var cores: Int
    public var utilizationPct: Double
    public var rendererPct: Double
    public var tilerPct: Double
    public var inUseMiB: Double

    /// Pure parser so it can be unit-tested with an injected IORegistry dictionary.
    public static func parse(_ props: [String: Any]) -> GPUInfo? {
        guard let cores = (props["gpu-core-count"] as? NSNumber)?.intValue, cores > 0 else { return nil }
        let stats = props["PerformanceStatistics"] as? [String: Any] ?? [:]
        func num(_ key: String) -> Double { (stats[key] as? NSNumber)?.doubleValue ?? 0 }
        var model = "Apple GPU"
        if let m = props["model"] as? String { model = m }
        else if let d = props["model"] as? Data, let m = String(data: d, encoding: .utf8) { model = m.trimmingCharacters(in: .controlCharacters) }
        return GPUInfo(model: model, cores: cores,
                       utilizationPct: min(100, max(0, num("Device Utilization %"))),
                       rendererPct: min(100, max(0, num("Renderer Utilization %"))),
                       tilerPct: min(100, max(0, num("Tiler Utilization %"))),
                       inUseMiB: num("In use system memory") / 1_048_576)
    }

    /// Reads the live values from IOKit; nil on non-Apple-Silicon hosts.
    public static func current() -> GPUInfo? {
        #if canImport(IOKit)
        var iterator: io_iterator_t = 0
        guard IOServiceGetMatchingServices(kIOMainPortDefault, IOServiceMatching("AGXAccelerator"), &iterator) == KERN_SUCCESS else { return nil }
        defer { IOObjectRelease(iterator) }
        var best: GPUInfo?
        while case let entry = IOIteratorNext(iterator), entry != 0 {
            defer { IOObjectRelease(entry) }
            var unmanaged: Unmanaged<CFMutableDictionary>?
            guard IORegistryEntryCreateCFProperties(entry, &unmanaged, kCFAllocatorDefault, 0) == KERN_SUCCESS,
                  let dict = unmanaged?.takeRetainedValue() as? [String: Any],
                  let info = parse(dict) else { continue }
            if best == nil || info.cores > best!.cores { best = info }
        }
        return best
        #else
        return nil
        #endif
    }
}

public enum CPUInfo {
    public static func count(_ name: String) -> Int? {
        var value: Int32 = 0; var size = MemoryLayout<Int32>.size
        return sysctlbyname(name, &value, &size, nil, 0) == 0 ? Int(value) : nil
    }
}

public enum Hardware {
    public static func report(budget: Double, runtimes: [String]) -> [String: Any] {
        let total=Double(ProcessInfo.processInfo.physicalMemory)/pow(1024,3)
        var result: [String:Any] = ["budgetGiB": min(budget,total*0.8), "slots":1, "runtimes":runtimes, "totalGiB":total, "os":ProcessInfo.processInfo.operatingSystemVersionString, "host":ProcessInfo.processInfo.hostName]
        #if os(macOS)
        let state=ProcessInfo.processInfo.thermalState
        result["thermal"] = state == .critical ? "critical" : state == .serious ? "serious" : state == .fair ? "fair" : "nominal"
        #endif
        if let gpu = GPUInfo.current() {
            result["gpuModel"] = gpu.model; result["gpuCores"] = gpu.cores
            result["gpuUtilPct"] = gpu.utilizationPct; result["rendererPct"] = gpu.rendererPct
            result["tilerPct"] = gpu.tilerPct; result["gpuInUseMiB"] = gpu.inUseMiB
        }
        if let p = CPUInfo.count("hw.perflevel0.physicalcpu") { result["cpuPerformanceCores"] = p }
        if let e = CPUInfo.count("hw.perflevel1.physicalcpu") { result["cpuEfficiencyCores"] = e }
        #if canImport(Metal)
        if let device=MTLCreateSystemDefaultDevice() {
            result["gpu"]=device.name
            result["metalWorkingSetGiB"]=Double(device.recommendedMaxWorkingSetSize)/pow(1024,3)
            // This is this agent's allocations, NOT a global free-GPU-memory counter.
            result["agentMetalAllocatedBytes"]=device.currentAllocatedSize
            result["unifiedMemory"]=device.hasUnifiedMemory
        }
        #endif
        return result
    }
}
public struct RuntimeConfig: Codable {
    public let executable: String
    public let prefix: [String]
    public init(executable:String,prefix:[String]) { self.executable=executable; self.prefix=prefix }
    public func command(_ spec: Spec) throws -> (String,[String]) {
        guard executable.hasPrefix("/"), FileManager.default.isExecutableFile(atPath:executable) else { throw GryviaError.invalid("Runtime executable unavailable") }
        return (executable,prefix+spec.args)
    }
}
