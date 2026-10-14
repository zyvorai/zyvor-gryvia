// SPDX-License-Identifier: BUSL-1.1
// Copyright © 2026 Zyvor AI Labs Private Limited
import Foundation
#if os(macOS)
import Darwin
#else
import Glibc
#endif
import GryviaCore

// The agent intentionally runs one process at a time for conservative GPU admission.
// Runtime executables are chosen locally, never from a remote command string.
struct Config: Decodable {
    let server: String
    let budgetGiB: Double
    let runtimes: [String:RuntimeConfig]
}
@main struct Agent {
    nonisolated(unsafe) static var attestor: Attestor?
    static func heartbeat(_ config:Config) -> sending [String:Any] {
        var body=Hardware.report(budget:config.budgetGiB,runtimes:Array(config.runtimes.keys))
        if let key=attestor?.publicKeyHex { body["attestKey"]=key }
        return body
    }
    static func main() async {
        do {
            guard CommandLine.arguments.count==2 else { throw GryviaError.invalid("Usage: gryvia-agent /absolute/agent.json; set GRYVIA_AGENT_TOKEN") }
            let config=try JSONDecoder().decode(Config.self,from:Data(contentsOf:URL(fileURLWithPath:CommandLine.arguments[1])))
            guard config.budgetGiB.isFinite && config.budgetGiB>0 else { throw GryviaError.invalid("Invalid memory budget") }
            let api=try API(base:config.server,token:ProcessInfo.processInfo.environment["GRYVIA_AGENT_TOKEN"] ?? "")
            let attestor = try? Attestor()   // nil only if the key file cannot be created; provenance is then simply absent
            Agent.attestor = attestor
            while !Task.isCancelled {
                do {
                    _=try await api.request("api/agent/heartbeat",body:heartbeat(config))
                    let reply=try JSONDecoder().decode(ClaimResponse.self,from:await api.request("api/agent/claim",body:[:]))
                    if let job=reply.job { await execute(job,api:api,config:config) }
                } catch { fputs("Agent: \(error.localizedDescription)\n",stderr) }
                try await Task.sleep(nanoseconds:2_000_000_000)
            }
        } catch { fputs("\(error.localizedDescription)\n",stderr); exit(1) }
    }
    static func execute(_ job:Claim,api:API,config:Config) async {
        let p=Process(); let pipe=Pipe(); let sink=BufferedLog()
        var resultLine: String?   // last structured result line exactly as shipped (after the 2,000-character cap)
        func note(_ lines:[String]) { for l in lines where l.hasPrefix("{\"kind\": \"result\"") { resultLine=l } }
        var success=false; var detail=""; var exitCode:Int32 = -1
        let started=Date()
        do {
            guard let runtime=config.runtimes[job.spec.runtime] else { throw GryviaError.invalid("Runtime not enabled") }
            let (exe,args)=try runtime.command(job.spec)
            p.executableURL=URL(fileURLWithPath:exe); p.arguments=args
            var env=ProcessInfo.processInfo.environment
            env.removeValue(forKey:"GRYVIA_AGENT_TOKEN"); env.removeValue(forKey:"GRYVIA_ADMIN_TOKEN")
            env["GRYVIA_MEMORY_RESERVATION_GIB"]=String(job.spec.memoryGiB)
            if let gib=Hardware.report(budget:config.budgetGiB,runtimes:[])["metalWorkingSetGiB"] as? Double { env["GRYVIA_METAL_WORKING_SET_BYTES"]=String(gib*pow(1024,3)) }
            env["GRYVIA_JOB_ID"]=job.id; p.environment=env
            let dir=FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/Application Support/Gryvia/jobs/\(job.id)")
            try FileManager.default.createDirectory(at:dir,withIntermediateDirectories:true); p.currentDirectoryURL=dir
            p.standardOutput=pipe; p.standardError=pipe
            pipe.fileHandleForReading.readabilityHandler = { h in let d=h.availableData; if !d.isEmpty { sink.append(d) } }
            try p.run()
            // Close the parent's copy so EOF is observable after child exit.
            try? pipe.fileHandleForWriting.close()
            while p.isRunning {
                try await Task.sleep(nanoseconds:2_000_000_000)
                let renew=try JSONDecoder().decode(Renewal.self,from:await api.request("api/agent/renew",body:["id":job.id,"lease":job.lease]))
                _=try await api.request("api/agent/heartbeat",body:heartbeat(config))
                let lines=sink.take(); note(lines)
                if !lines.isEmpty { _=try await api.request("api/agent/logs",body:["id":job.id,"lease":job.lease,"lines":lines]) }
                if renew.cancel || Date().timeIntervalSince(started)>Double(job.spec.timeoutSeconds) {
                    detail=renew.cancel ? (renew.reason == "job timeout" ? "Job timeout" : "Cancelled by user") : "Job timeout"
                    await stop(p); break
                }
            }
            // Poll instead of waitUntilExit(): with a readabilityHandler attached, Foundation's run-loop wait can deadlock
            // after the child has already exited, which left finished jobs unfinished and the node offline.
            while p.isRunning { try? await Task.sleep(nanoseconds:50_000_000) }
            exitCode=p.terminationStatus; success=exitCode==0 && detail.isEmpty
        } catch {
            detail=error.localizedDescription
            // Fail closed immediately on renewal or connectivity failure.
            if p.isRunning { await stop(p) }
        }
        pipe.fileHandleForReading.readabilityHandler=nil
        sink.finish()
        // Bounded batches retain pending lines instead of silently dropping each flush.
        do {
            while true {
                let lines=sink.take(); note(lines)
                if lines.isEmpty { break }
                _=try await api.request("api/agent/renew",body:["id":job.id,"lease":job.lease])
                _=try await api.request("api/agent/logs",body:["id":job.id,"lease":job.lease,"lines":lines])
            }
        } catch { fputs("Log flush: \(error.localizedDescription)\n",stderr) }
        let finished=Date()
        var body:[String:Any]=["id":job.id,"lease":job.lease,"success":success,"result":["exitCode":exitCode,"durationSeconds":finished.timeIntervalSince(started),"message":detail]]
        if let attestor, let spec=job.specSha256 {
            // Signed statement: this node ran this job (spec hash) and produced this result line, with this exit code.
            let payload:[String:Any]=["v":1,"job":job.id,"kind":job.spec.kind ?? "inference","modelId":job.spec.modelId ?? "","datasetId":job.spec.datasetId ?? "",
                "specSha256":spec,"resultSha256":resultLine.map(Attestor.sha256Hex) ?? "","exitCode":Int(exitCode),"success":success,
                "startedAt":started.timeIntervalSince1970,"finishedAt":finished.timeIntervalSince1970,"agent":"gryvia-agent"]
            if let text=try? Attestor.canonical(payload), let sig=try? attestor.sign(text) { body["attestation"]=["payload":text,"signature":sig] }
        }
        _=try? await api.request("api/agent/complete",body:body)
    }
    static func stop(_ p:Process) async {
        guard p.isRunning else { return }; p.terminate()
        for _ in 0..<10 { if !p.isRunning { return }; try? await Task.sleep(nanoseconds:100_000_000) }
        if p.isRunning { kill(p.processIdentifier,SIGKILL) }
        for _ in 0..<20 { if !p.isRunning { return }; try? await Task.sleep(nanoseconds:100_000_000) }
    }
}
