#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
# Install the optional model runtimes on THIS Mac: MLX + mlx-lm + huggingface_hub in ~/gryvia/venv.
# llama.cpp is expected from Homebrew (brew install llama.cpp); this script tells you if it is missing.
set -euo pipefail
PY="${PYTHON:-$(command -v /opt/homebrew/bin/python3.14 || command -v python3)}"
VENV="${GRYVIA_VENV:-$HOME/gryvia/venv}"
echo "==> python: $("$PY" --version) ($PY)"
"$PY" -m venv "$VENV"
"$VENV/bin/python" -m pip install --quiet --upgrade pip
"$VENV/bin/python" -m pip install --quiet mlx mlx-lm huggingface_hub
"$VENV/bin/python" - <<'PYEOF'
import mlx.core as mx
print("==> mlx OK, default device:", mx.default_device())
import mlx_lm
print("==> mlx-lm OK")
PYEOF
for t in llama-bench llama-cli llama-quantize llama-server; do
  if command -v "$t" >/dev/null || [ -x "/opt/homebrew/bin/$t" ]; then echo "==> $t found"; else echo "==> $t MISSING (brew install llama.cpp)"; fi
done
