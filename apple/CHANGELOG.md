# Changelog

## 0.1.0 — 2026-10-14

Initial engineering beta: durable scheduling/control-plane API, native Swift worker and SwiftUI app, fleet web console, local runtime wrappers, tenant policy, inference routing, allocation metering, launchd deployment and CI. Hardware/runtime validation remains pending; see TEST_RESULTS.md.

## Continuation — 14 October 2026

- Enforce job deadlines on the server, hold reservations during stop acknowledgement, cap timeout lease renewal and apply configured retries.
- Upload logs in bounded batches, retain pending output and report overflow.
- Add eight Python regressions and four native log-buffer XCTest cases.
- Correct local setup URLs to port 8443.
