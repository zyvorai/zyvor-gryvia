// SPDX-License-Identifier: BUSL-1.1
// Copyright © 2026 Zyvor AI Labs Private Limited
import XCTest
@testable import GryviaCore
final class CoreTests:XCTestCase {
    func testRejectRemoteHTTP() { XCTAssertThrowsError(try API(base:"http://example.com",token:String(repeating:"x",count:32))) }
    func testAcceptLoopback() { XCTAssertNoThrow(try API(base:"http://127.0.0.1:8443",token:String(repeating:"x",count:32))) }
    func testRejectShortToken() { XCTAssertThrowsError(try API(base:"https://example.com",token:"short")) }
    func testRejectCredentialsAndPath() {
        XCTAssertThrowsError(try API(base:"https://user:pw@example.com",token:String(repeating:"x",count:32)))
        XCTAssertThrowsError(try API(base:"https://example.com/api",token:String(repeating:"x",count:32)))
    }
    func testClaimDecode() throws {
        let data=Data(#"{"job":{"id":"j","lease":"l","expires":100,"spec":{"runtime":"demo","memoryGiB":4,"priority":50,"retries":0,"timeoutSeconds":10,"args":[],"mode":"exclusive"}}}"#.utf8)
        XCTAssertEqual(try JSONDecoder().decode(ClaimResponse.self,from:data).job?.spec.memoryGiB,4)
    }
    func testHardwareBudget() { let r=Hardware.report(budget:1,runtimes:["demo"]); XCTAssertEqual(r["budgetGiB"] as? Double,1) }
}

final class JobStateStyleTests: XCTestCase {
    func testSymbols() {
        XCTAssertEqual(JobStateStyle.symbol("running"), "play.circle.fill")
        XCTAssertEqual(JobStateStyle.symbol("nonsense"), "questionmark.circle")
    }
}

final class BrandTests: XCTestCase {
    func testAccentIsZyvorBlue() {
        XCTAssertEqual(Brand.accentHex, 0x0088FF)
        XCTAssertEqual(Brand.accents.count, 8)
    }
}

final class GPUInfoTests: XCTestCase {
    func testParsesAGXProperties() {
        let props: [String: Any] = ["gpu-core-count": 12, "model": "Apple M6",
            "PerformanceStatistics": ["Device Utilization %": 73, "Renderer Utilization %": 70, "Tiler Utilization %": 71, "In use system memory": 1_073_741_824]]
        let info = GPUInfo.parse(props)
        XCTAssertEqual(info?.cores, 12)
        XCTAssertEqual(info?.model, "Apple M6")
        XCTAssertEqual(info?.utilizationPct, 73)
        XCTAssertEqual(info?.inUseMiB, 1024)
    }
    func testClampsAndRejectsNonGPU() {
        XCTAssertNil(GPUInfo.parse(["model": "x"]))
        let info = GPUInfo.parse(["gpu-core-count": 8, "PerformanceStatistics": ["Device Utilization %": 250]])
        XCTAssertEqual(info?.utilizationPct, 100)
    }
    func testHardwareReportCarriesGPUOnAppleSilicon() {
        let r = Hardware.report(budget: 1, runtimes: ["demo"])
        if GPUInfo.current() != nil { XCTAssertNotNil(r["gpuCores"]); XCTAssertNotNil(r["gpuUtilPct"]) }
    }
}


final class PresenceTests: XCTestCase {
    func testBatteryParsing() {
        let b = PresenceInfo.battery(from: ["Type": "InternalBattery", "Power Source State": "Battery Power", "Current Capacity": 42, "Max Capacity": 100])
        XCTAssertEqual(b?.onBattery, true)
        XCTAssertEqual(b?.pct, 42)
        let ac = PresenceInfo.battery(from: ["Type": "InternalBattery", "Power Source State": "AC Power", "Current Capacity": 80, "Max Capacity": 100])
        XCTAssertEqual(ac?.onBattery, false)
        XCTAssertNil(PresenceInfo.battery(from: ["Type": "UPS"]))
    }
    func testDictionaryOmitsUnknownValues() {
        let p = PresenceInfo(idleSeconds: nil, locked: true, displaySleep: false, onBattery: false, batteryPct: nil, lowPowerMode: false, micInUse: false)
        XCTAssertNil(p.dictionary["idleSeconds"])
        XCTAssertEqual(p.dictionary["locked"] as? Bool, true)
    }
    func testCurrentDoesNotCrashAndReportsIdle() {
        let p = PresenceInfo.current()
        #if os(macOS)
        XCTAssertNotNil(p.idleSeconds)
        #endif
        XCTAssertTrue(Hardware.report(budget: 1, runtimes: [])["presence"] is [String: Any])
    }
}


final class PowerFileTests: XCTestCase {
    func write(_ json: String) -> String {
        let path = NSTemporaryDirectory() + "power-\(UUID().uuidString).json"
        try? json.write(toFile: path, atomically: true, encoding: .utf8)
        return path
    }
    func testFreshFileIsReadAndStaleOrMissingIsNot() {
        let now = Date()
        let fresh = write("{\"at\": \(now.timeIntervalSince1970 - 2), \"packageW\": 12.5, \"gpuW\": 4.2}")
        XCTAssertEqual(PowerFile.read(path: fresh, now: now)?["packageW"] as? Double, 12.5)
        let stale = write("{\"at\": \(now.timeIntervalSince1970 - 60), \"packageW\": 12.5}")
        XCTAssertNil(PowerFile.read(path: stale, now: now))
        XCTAssertNil(PowerFile.read(path: "/nonexistent/gryvia-power.json", now: now))
        let absurd = write("{\"at\": \(now.timeIntervalSince1970), \"packageW\": 99999}")
        XCTAssertNil(PowerFile.read(path: absurd, now: now))
    }
}
