#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Quantize a fine-tuned model to 4-bit weights (AWQ or GPTQ) with llm-compressor, for cheaper vLLM serving.

  python3 quantize.py --model /models/finetuned/qwen3-8b/abc123 --method awq --calibration /data/chat.jsonl

Calibration samples come from a JSONL file ({"messages": [...]} rendered with the tokenizer's chat template, or
{"text": "..."}) or a Hugging Face dataset id. The result is saved in the compressed-tensors format next to the input
(<model>-awq or <model>-gptq), which vLLM loads with --quantization=compressed-tensors (it also detects it from
config.json). Reports path, subPath, format, quantization, method and scheme to the workflow.
"""
import argparse
import json
import os
import sys

from outputs import is_rank_zero, write_outputs

METHODS = {
    # method: (default scheme, schemes llm-compressor accepts for it here)
    "awq": ("W4A16_ASYM", ("W4A16_ASYM", "W4A16")),
    "gptq": ("W4A16", ("W4A16", "W8A16", "W4A16_ASYM")),
}


def parse_args(argv=None):
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--model", required=True, help="directory of the (fine-tuned) model to quantize")
    p.add_argument("--method", choices=sorted(METHODS), default="awq")
    p.add_argument("--scheme", default="", help="weight scheme (default W4A16_ASYM for awq, W4A16 for gptq)")
    p.add_argument("--calibration", required=True, help="JSONL path or Hugging Face dataset id")
    p.add_argument("--calibration-split", default="train")
    p.add_argument("--samples", type=int, default=256, help="calibration samples")
    p.add_argument("--max-seq-len", type=int, default=2048)
    p.add_argument("--output", default="", help="output directory (default <model>-<method>)")
    p.add_argument("--pvc-root", default="/models")
    a = p.parse_args(argv)
    default, allowed = METHODS[a.method]
    a.scheme = a.scheme or default
    if a.scheme not in allowed:
        p.error(f"--scheme {a.scheme} is not supported with --method {a.method} (use one of {', '.join(allowed)})")
    if a.samples < 1:
        p.error("--samples must be at least 1")
    a.output = a.output or quantized_dir(a.model, a.method)
    return a


def quantized_dir(model, method):
    return model.rstrip("/") + "-" + method


def calibration_texts(path, limit, render):
    """Up to limit training texts from a JSONL file: "text" as is, "messages" through render (the chat template)."""
    out = []
    with open(path) as f:
        for n, line in enumerate(f, 1):
            if not line.strip():
                continue
            rec = json.loads(line)
            if isinstance(rec, dict) and isinstance(rec.get("text"), str):
                out.append(rec["text"])
            elif isinstance(rec, dict) and isinstance(rec.get("messages"), list):
                out.append(render(rec["messages"]))
            else:
                raise ValueError(f"{path}:{n}: each record needs a 'messages' list or a 'text' field")
            if len(out) >= limit:
                break
    if not out:
        raise ValueError(f"{path} has no records")
    return out


def recipe(method, scheme):
    """The llm-compressor modifier for the method; lm_head stays in full precision."""
    if method == "awq":
        from llmcompressor.modifiers.awq import AWQModifier
        return [AWQModifier(targets=["Linear"], scheme=scheme, ignore=["lm_head"])]
    from llmcompressor.modifiers.quantization import GPTQModifier
    return [GPTQModifier(targets=["Linear"], scheme=scheme, ignore=["lm_head"])]


def main(argv=None):
    a = parse_args(argv)
    from datasets import Dataset, load_dataset
    from llmcompressor import oneshot
    from transformers import AutoModelForCausalLM, AutoTokenizer

    tok = AutoTokenizer.from_pretrained(a.model)
    if os.path.isfile(a.calibration):
        texts = calibration_texts(a.calibration, a.samples,
                                  lambda m: tok.apply_chat_template(m, tokenize=False))
    else:
        ds = load_dataset(a.calibration, split=f"{a.calibration_split}[:{a.samples}]")
        col = "text" if "text" in ds.column_names else None
        texts = [r[col] if col else tok.apply_chat_template(r["messages"], tokenize=False) for r in ds]
    ds = Dataset.from_dict({"text": texts}).map(
        lambda r: tok(r["text"], truncation=True, max_length=a.max_seq_len, add_special_tokens=False),
        remove_columns=["text"])

    model = AutoModelForCausalLM.from_pretrained(a.model, torch_dtype="auto", device_map="auto")
    oneshot(model=model, dataset=ds, recipe=recipe(a.method, a.scheme), max_seq_length=a.max_seq_len,
            num_calibration_samples=len(texts))

    if not is_rank_zero():
        return 0
    os.makedirs(a.output, exist_ok=True)
    model.save_pretrained(a.output, save_compressed=True)
    tok.save_pretrained(a.output)
    write_outputs({
        "path": a.output,
        "subPath": os.path.relpath(a.output, a.pvc_root),
        "format": "compressed-tensors",
        "quantization": "compressed-tensors",
        "method": a.method,
        "scheme": a.scheme,
    })
    return 0


if __name__ == "__main__":
    sys.exit(main())
