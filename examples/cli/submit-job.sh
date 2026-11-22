#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Example: Submit a training job using Gryvia CLI

set -e

echo "Submitting LLM training job..."

gryvia submit -f - <<EOF
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: llm-training
  namespace: ml-training
spec:
  type: training
  distributed:
    framework: pytorch
    enabled: true
    nodes: 4
    gpusPerNode: 8

  gpus: 32   # 4 nodes x 8 GPUs
  gpuType: H100
  resources:
    requests:
      cpu: "96"
      memory: 1Ti

  image: nvcr.io/nvidia/pytorch:24.01-py3

  command:
    - torchrun
    - --nproc_per_node=8
    - --nnodes=4
    - --master_port=29500
    - train_llm.py
    - --model=llama-70b
    - --batch-size=4

  env:
    - name: NCCL_IB_HCA
      value: "mlx5_0,mlx5_1"
    - name: NCCL_NET_GDR_LEVEL
      value: "5"
EOF

echo ""
echo "✓ Job submitted!"
echo ""
echo "Monitor with:"
echo "  gryvia status llm-training"
echo "  gryvia logs llm-training --follow"
