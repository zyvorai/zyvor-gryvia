# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
# Homebrew cask template. Publish it in a tap (zyvorai/homebrew-tap) after the release assets exist:
#   1. replace `version` and `sha256` from release/Gryvia-VERSION.dmg.sha256
#   2. `brew install --cask zyvorai/tap/gryvia`
# The DMG is ad hoc signed unless the release was built with Developer ID secrets; see docs/RELEASE.md.
cask "gryvia" do
  version "0.3.0"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"

  url "https://github.com/zyvorai/zyvor-gryvia-apple/releases/download/v#{version}/Gryvia-#{version}.dmg"
  name "Gryvia"
  desc "Schedule AI workloads across Apple Silicon Macs with unified-memory admission control"
  homepage "https://zyvorai.github.io/zyvor-gryvia-apple/"

  depends_on macos: ">= :sequoia"
  depends_on arch: :arm64

  app "Gryvia.app"

  zap trash: [
    "~/Library/Application Support/Gryvia",
    "~/Library/Logs/Gryvia",
    "~/Library/LaunchAgents/dev.zyvor.gryvia.agent.plist",
    "~/Library/LaunchAgents/dev.zyvor.gryvia.server.plist",
  ]
end
