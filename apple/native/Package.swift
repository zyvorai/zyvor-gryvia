// SPDX-License-Identifier: BUSL-1.1
// Copyright © 2026 Zyvor AI Labs Private Limited
// swift-tools-version: 6.0
import PackageDescription
let package = Package(name: "GryviaApple", platforms: [.macOS(.v15)], products: [
    .executable(name: "gryvia-agent", targets: ["GryviaAgent"]),
    .executable(name: "GryviaDesktop", targets: ["GryviaDesktop"])
], targets: [.target(name: "GryviaCore"), .executableTarget(name: "GryviaAgent", dependencies: ["GryviaCore"]), .executableTarget(name: "GryviaDesktop", dependencies: ["GryviaCore"]), .testTarget(name: "GryviaCoreTests", dependencies: ["GryviaCore"])])
