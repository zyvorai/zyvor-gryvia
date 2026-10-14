// SPDX-License-Identifier: BUSL-1.1
// Copyright © 2026 Zyvor AI Labs Private Limited
import Foundation

/// Bounded process output with explicit overflow reporting and retained batch tails.
public final class BufferedLog: @unchecked Sendable {
    private let lock = NSLock()
    private var lines: [String] = []
    private var partial = ""
    private var dropped = 0
    private let capacity: Int

    public init(capacity: Int = 500) { self.capacity = max(1, capacity) }
    public func append(_ data: Data) {
        lock.lock(); defer { lock.unlock() }
        partial += String(decoding: data, as: UTF8.self)
        while let index = partial.firstIndex(of: "\n") {
            enqueue(String(partial[..<index]))
            partial.removeSubrange(...index)
        }
        if partial.utf8.count > 8000 { enqueue(partial); partial = "" }
    }
    private func enqueue(_ text: String) {
        // 2000 Unicode scalars use at most 8000 UTF-8 bytes, within API limits.
        lines.append(String(String.UnicodeScalarView(text.unicodeScalars.prefix(2000))))
        if lines.count > capacity { dropped += lines.count - capacity; lines.removeFirst(lines.count - capacity) }
    }
    public func finish() {
        lock.lock(); defer { lock.unlock() }
        if !partial.isEmpty { enqueue(partial); partial = "" }
    }
    public func take(maxLines: Int = 100, maxBytes: Int = 8192) -> [String] {
        lock.lock(); defer { lock.unlock() }
        guard maxLines > 0, maxBytes >= 8192 else { return [] }
        var output: [String] = []
        var bytes = 0
        if dropped > 0 {
            let marker = "[Gryvia: dropped \(dropped) buffered log lines due to output overflow]"
            output.append(marker); bytes = marker.utf8.count; dropped = 0
        }
        var count = 0
        for line in lines {
            guard output.count < maxLines, bytes + line.utf8.count <= maxBytes else { break }
            output.append(line); bytes += line.utf8.count; count += 1
        }
        lines.removeFirst(count)
        return output
    }
}
