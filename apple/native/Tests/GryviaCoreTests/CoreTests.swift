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
