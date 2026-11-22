#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""LoRA (or QLoRA) supervised fine-tuning of a downloaded base model with TRL and PEFT.

  torchrun --nproc_per_node=$MODEL_GPUS finetune_lora.py --base /models/cache/Qwen/Qwen3-8B/<sha> \\
      --dataset /data/chat.jsonl --output-root /models/finetuned --name qwen3-8b

The dataset is a JSONL file (one {"messages": [...]} or {"text": "..."} per line) or a Hugging Face dataset id.
LoRA merges the adapter into the base weights and saves safetensors that vLLM loads directly; --qlora trains a
4-bit base and saves only the adapter (serve it with vLLM --enable-lora). Rank 0 reports the outputs path,
subPath, format, adapter and train_loss to the workflow.
"""
import argparse
import inspect
import json
import os
import sys

from outputs import is_rank_zero, write_outputs


def output_dir(root, name, revision):
    return os.path.join(root, name, (revision or "main")[:12])


def filter_kwargs(cls, kwargs):
    """Keep the keyword arguments cls accepts, so the script works across TRL versions that renamed options."""
    params = inspect.signature(cls).parameters
    if any(p.kind == p.VAR_KEYWORD for p in params.values()):
        return dict(kwargs)
    return {k: v for k, v in kwargs.items() if k in params}


def validate_jsonl(path, limit=1000):
    """Fail early on a malformed dataset: every line must be an object with "messages" or "text"."""
    with open(path) as f:
        for n, line in enumerate(f, 1):
            if n > limit:
                break
            if not line.strip():
                continue
            rec = json.loads(line)
            if not isinstance(rec, dict) or not ("messages" in rec or "text" in rec):
                raise ValueError(f"{path}:{n}: each record needs a 'messages' list or a 'text' field")


def parse_args(argv=None):
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--base", required=True, help="local directory of the base model")
    p.add_argument("--dataset", required=True, help="JSONL path or Hugging Face dataset id")
    p.add_argument("--dataset-split", default="train")
    p.add_argument("--output-root", default="/models/finetuned")
    p.add_argument("--name", default=os.environ.get("MODEL_SLUG", "model"))
    p.add_argument("--revision", default=os.environ.get("MODEL_REVISION", ""))
    p.add_argument("--pvc-root", default="/models")
    p.add_argument("--epochs", type=float, default=1.0)
    p.add_argument("--max-steps", type=int, default=-1)
    p.add_argument("--lr", type=float, default=2e-4)
    p.add_argument("--batch-size", type=int, default=4)
    p.add_argument("--grad-accum", type=int, default=4)
    p.add_argument("--max-seq-len", type=int, default=2048)
    p.add_argument("--lora-r", type=int, default=16)
    p.add_argument("--lora-alpha", type=int, default=32)
    p.add_argument("--lora-dropout", type=float, default=0.05)
    p.add_argument("--qlora", action="store_true", help="4-bit base (bitsandbytes); saves the adapter only")
    p.add_argument("--cpu", action="store_true", help="float32 on CPU (CI with tiny models)")
    return p.parse_args(argv)


def main(argv=None):
    a = parse_args(argv)
    import torch
    from datasets import load_dataset
    from peft import LoraConfig
    from transformers import AutoModelForCausalLM, AutoTokenizer
    from trl import SFTConfig, SFTTrainer

    if os.path.isfile(a.dataset):
        validate_jsonl(a.dataset)
        ds = load_dataset("json", data_files=a.dataset, split="train")
    else:
        ds = load_dataset(a.dataset, split=a.dataset_split)

    dtype = torch.float32 if a.cpu else torch.bfloat16
    model_kwargs = {"torch_dtype": dtype}
    if a.qlora:
        from transformers import BitsAndBytesConfig
        model_kwargs["quantization_config"] = BitsAndBytesConfig(
            load_in_4bit=True, bnb_4bit_quant_type="nf4", bnb_4bit_compute_dtype=torch.bfloat16)
    tok = AutoTokenizer.from_pretrained(a.base)
    if tok.pad_token is None:
        tok.pad_token = tok.eos_token
    model = AutoModelForCausalLM.from_pretrained(a.base, **model_kwargs)

    out = output_dir(a.output_root, a.name, a.revision)
    work = out + ".work"
    cfg = SFTConfig(**filter_kwargs(SFTConfig, {
        "output_dir": work,
        "num_train_epochs": a.epochs,
        "max_steps": a.max_steps,
        "learning_rate": a.lr,
        "per_device_train_batch_size": a.batch_size,
        "gradient_accumulation_steps": a.grad_accum,
        "bf16": not a.cpu,
        "use_cpu": a.cpu,
        "logging_steps": 10,
        "save_strategy": "no",
        "report_to": [],
        "max_length": a.max_seq_len,
        "max_seq_length": a.max_seq_len,
    }))
    lora = LoraConfig(r=a.lora_r, lora_alpha=a.lora_alpha, lora_dropout=a.lora_dropout,
                      target_modules="all-linear", task_type="CAUSAL_LM")
    trainer = SFTTrainer(**filter_kwargs(SFTTrainer, {
        "model": model, "args": cfg, "train_dataset": ds, "peft_config": lora,
        "processing_class": tok, "tokenizer": tok,
    }))
    result = trainer.train()

    if not is_rank_zero():
        return 0
    os.makedirs(out, exist_ok=True)
    if a.qlora:
        trainer.model.save_pretrained(out)
    else:
        merged = trainer.model.merge_and_unload()
        merged.save_pretrained(out, safe_serialization=True)
    tok.save_pretrained(out)
    write_outputs({
        "path": out,
        "subPath": os.path.relpath(out, a.pvc_root),
        "format": "safetensors",
        "adapter": "true" if a.qlora else "false",
        "train_loss": f"{result.training_loss:.6f}",
    })
    return 0


if __name__ == "__main__":
    sys.exit(main())
