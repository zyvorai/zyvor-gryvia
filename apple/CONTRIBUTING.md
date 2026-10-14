# Contributing

Run `bash scripts/verify.sh` before proposing a change. Changes to native code also require `bash scripts/mac-smoke.sh` on macOS. Web changes should run `npm run test:web`. Include real-hardware evidence when claiming GPU behavior. Keep the feature matrix explicit about tested, hardware-pending and unsupported capabilities. Do not commit local configs, credentials, databases, build output or model artifacts.
