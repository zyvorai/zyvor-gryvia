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
    static func main() async {
        do {
            guard CommandLine.arguments.count==2 else { throw GryviaError.invalid("Usage: gryvia-agent /absolute/agent.json; set GRYVIA_AGENT_TOKEN") }
            let config=try JSONDecoder().decode(Config.self,from:Data(contentsOf:URL(fileURLWithPath:CommandLine.arguments[1])))
            guard config.budgetGiB.isFinite && config.budgetGiB>0 else { throw GryviaError.invalid("Invalid memory budget") }
            let api=try API(base:config.server,token:ProcessInfo.processInfo.environment["GRYVIA_AGENT_TOKEN"] ?? "")
            while !Task.isCancelled {
                do {
                    _=try await api.request("api/agent/heartbeat",body:Hardware.report(budget:config.budgetGiB,runtimes:Array(config.runtimes.keys)))
                    let reply=try JSONDecoder().decode(ClaimResponse.self,from:await api.request("api/agent/claim",body:[:]))
                    if let job=reply.job { await execute(job,api:api,config:config) }
                } catch { fputs("Agent: \(error.localizedDescription)\n",stderr) }
                try await Task.sleep(nanoseconds:2_000_000_000)
            }
        } catch { fputs("\(error.localizedDescription)\n",stderr); exit(1) }
    }
    static func execute(_ job:Claim,api:API,config:Config) async {
        let p=Process(); let pipe=Pipe(); let sink=BufferedLog()
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
                _=try await api.request("api/agent/heartbeat",body:Hardware.report(budget:config.budgetGiB,runtimes:Array(config.runtimes.keys)))
                let lines=sink.take()
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
                let lines=sink.take()
                if lines.isEmpty { break }
                _=try await api.request("api/agent/renew",body:["id":job.id,"lease":job.lease])
                _=try await api.request("api/agent/logs",body:["id":job.id,"lease":job.lease,"lines":lines])
            }
        } catch { fputs("Log flush: \(error.localizedDescription)\n",stderr) }
        _=try? await api.request("api/agent/complete",body:["id":job.id,"lease":job.lease,"success":success,"result":["exitCode":exitCode,"durationSeconds":Date().timeIntervalSince(started),"message":detail]])
    }
    static func stop(_ p:Process) async {
        guard p.isRunning else { return }; p.terminate()
        for _ in 0..<10 { if !p.isRunning { return }; try? await Task.sleep(nanoseconds:100_000_000) }
        if p.isRunning { kill(p.processIdentifier,SIGKILL) }
        for _ in 0..<20 { if !p.isRunning { return }; try? await Task.sleep(nanoseconds:100_000_000) }
    }
}
