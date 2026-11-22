#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Convert a merged Hugging Face model directory into one GGUF file for llama.cpp's llama-server.

  python3 convert_gguf.py --model /models/finetuned/qwen3-8b/abc123 --outtype q8_0

Runs llama.cpp's convert_hf_to_gguf.py (from LLAMA_CPP_DIR; Dockerfile.cpu installs it in /opt/llama.cpp) and
writes <model>/model-<outtype>.gguf next to the safetensors, so a GryviaModelRegistry entry whose subPath is the
model directory is mounted at /models and served with `llama-server -m /models/model-<outtype>.gguf`. Running it
again reuses an existing file. Outputs: path (the model directory), subPath (relative to --pvc-root), file (the
GGUF name), format (gguf) and bytes.
"""
import argparse
import os
import subprocess
import sys

from outputs import write_outputs

OUTTYPES = ("f32", "f16", "bf16", "q8_0", "tq1_0", "tq2_0", "auto")


def gguf_name(outtype):
    return f"model-{outtype}.gguf"


def convert_command(llama_cpp_dir, model, outfile, outtype):
    return [sys.executable, os.path.join(llama_cpp_dir, "convert_hf_to_gguf.py"), model,
            "--outtype", outtype, "--outfile", outfile]


def convert(model, outtype, llama_cpp_dir, run=subprocess.run):
    """Return the GGUF path, converting unless it already exists; run is subprocess.run, injectable for tests."""
    if outtype not in OUTTYPES:
        raise ValueError(f"--outtype must be one of {', '.join(OUTTYPES)}")
    if not os.path.isfile(os.path.join(model, "config.json")):
        raise FileNotFoundError(f"{model} has no config.json: not a Hugging Face model directory")
    outfile = os.path.join(model, gguf_name(outtype))
    if os.path.isfile(outfile) and os.path.getsize(outfile) > 0:
        print(f"{outfile} already exists", flush=True)
        return outfile
    tmp = outfile + ".tmp"
    run(convert_command(llama_cpp_dir, model, tmp, outtype), check=True)
    os.replace(tmp, outfile)
    return outfile


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--model", required=True, help="merged model directory (finetune_lora.py output path)")
    p.add_argument("--outtype", default="q8_0", choices=OUTTYPES)
    p.add_argument("--pvc-root", default="/models", help="mount path of the PVC, to report subPath")
    p.add_argument("--llama-cpp-dir", default=os.environ.get("LLAMA_CPP_DIR", "/opt/llama.cpp"))
    a = p.parse_args(argv)
    outfile = convert(a.model, a.outtype, a.llama_cpp_dir)
    write_outputs({
        "path": a.model,
        "subPath": os.path.relpath(a.model, a.pvc_root),
        "file": os.path.basename(outfile),
        "format": "gguf",
        "bytes": str(os.path.getsize(outfile)),
    })
    return 0


if __name__ == "__main__":
    sys.exit(main())
