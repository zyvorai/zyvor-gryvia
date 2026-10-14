// SPDX-License-Identifier: BUSL-1.1
// Copyright © 2026 Zyvor AI Labs Private Limited
import XCTest
@testable import GryviaCore

final class BufferedLogTests: XCTestCase {
    func testBatchTailIsRetained() {
        let log = BufferedLog()
        log.append(Data("one\ntwo\nthree\nfour\n".utf8))
        XCTAssertEqual(log.take(maxLines: 2), ["one", "two"])
        XCTAssertEqual(log.take(maxLines: 2), ["three", "four"])
        XCTAssertEqual(log.take(), [])
    }
    func testPartialLineFinished() {
        let log = BufferedLog()
        log.append(Data("par".utf8)); log.append(Data("tial".utf8))
        XCTAssertEqual(log.take(), [])
        log.finish()
        XCTAssertEqual(log.take(), ["partial"])
        log.finish(); XCTAssertEqual(log.take(), [])
    }
    func testOverflowIsExplicit() {
        let log = BufferedLog(capacity: 2)
        log.append(Data("one\ntwo\nthree\n".utf8))
        let lines = log.take()
        XCTAssertTrue(lines[0].contains("dropped 1"))
        XCTAssertEqual(Array(lines.dropFirst()), ["two", "three"])
    }
    func testUnicodeBatchByteLimit() {
        let log = BufferedLog()
        let text = String(repeating: "😀", count: 2000)
        log.append(Data((text + "\n" + text + "\n").utf8))
        XCTAssertEqual(log.take(maxBytes: 8192), [text])
        XCTAssertEqual(log.take(maxBytes: 8192), [text])
    }
}
