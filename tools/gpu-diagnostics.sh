#!/bin/bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# GPU Diagnostics Tool for Gryvia
# Comprehensive GPU health and performance checking

set -euo pipefail

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m'

echo -e "${BLUE}╔════════════════════════════════════════════════════════════════╗${NC}"
echo -e "${BLUE}║          Gryvia GPU Diagnostics Tool                      ║${NC}"
echo -e "${BLUE}╚════════════════════════════════════════════════════════════════╝${NC}"
echo ""

# Check if running in Kubernetes
if [ -f /var/run/secrets/kubernetes.io/serviceaccount/token ]; then
    IN_CLUSTER=true
else
    IN_CLUSTER=false
fi

# Function to run nvidia-smi
check_nvidia_smi() {
    echo -e "\n${YELLOW}[1/8] Checking NVIDIA Driver Installation...${NC}"

    if command -v nvidia-smi &> /dev/null; then
        echo -e "${GREEN}✓ nvidia-smi found${NC}"
        nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -1 | \
            xargs -I {} echo -e "${GREEN}✓ Driver version: {}${NC}"
    else
        echo -e "${RED}✗ nvidia-smi not found${NC}"
        return 1
    fi
}

# Function to check GPU health
check_gpu_health() {
    echo -e "\n${YELLOW}[2/8] Checking GPU Health Status...${NC}"

    # Get GPU count
    GPU_COUNT=$(nvidia-smi --query-gpu=count --format=csv,noheader | head -1)
    echo -e "${GREEN}✓ Found $GPU_COUNT GPU(s)${NC}"

    # Check each GPU
    nvidia-smi --query-gpu=index,name,temperature.gpu,power.draw,power.limit,utilization.gpu,utilization.memory,memory.used,memory.total \
        --format=csv,noheader | while IFS=',' read -r idx name temp power_draw power_limit util_gpu util_mem mem_used mem_total; do

        echo ""
        echo "  GPU $idx: $name"
        echo "  ├─ Temperature: ${temp% *}°C"

        # Temperature check
        temp_val=${temp% *}
        if [ "$temp_val" -gt 85 ]; then
            echo -e "  ${RED}  └─ WARNING: High temperature!${NC}"
        elif [ "$temp_val" -gt 75 ]; then
            echo -e "  ${YELLOW}  └─ CAUTION: Elevated temperature${NC}"
        else
            echo -e "  ${GREEN}  └─ Temperature OK${NC}"
        fi

        echo "  ├─ Power: ${power_draw} / ${power_limit}"
        echo "  ├─ GPU Utilization: ${util_gpu}"
        echo "  ├─ Memory Utilization: ${util_mem}"
        echo "  └─ Memory: ${mem_used} / ${mem_total}"
    done
}

# Function to check CUDA
check_cuda() {
    echo -e "\n${YELLOW}[3/8] Checking CUDA Installation...${NC}"

    if command -v nvcc &> /dev/null; then
        CUDA_VERSION=$(nvcc --version | grep "release" | sed 's/.*release //' | sed 's/,.*//')
        echo -e "${GREEN}✓ CUDA version: $CUDA_VERSION${NC}"
    else
        echo -e "${YELLOW}⚠ nvcc not found (CUDA toolkit not installed)${NC}"
    fi

    # Check CUDA libraries
    if [ -d "/usr/local/cuda/lib64" ]; then
        echo -e "${GREEN}✓ CUDA libraries found${NC}"
    else
        echo -e "${YELLOW}⚠ CUDA libraries not found in standard location${NC}"
    fi
}

# Function to check GPU topology
check_topology() {
    echo -e "\n${YELLOW}[4/8] Checking GPU Topology...${NC}"

    nvidia-smi topo -m 2>/dev/null || echo -e "${YELLOW}⚠ Topology information not available${NC}"
}

# Function to check NCCL
check_nccl() {
    echo -e "\n${YELLOW}[5/8] Checking NCCL Installation...${NC}"

    NCCL_PATHS=(
        "/usr/lib/x86_64-linux-gnu/libnccl.so"
        "/usr/local/lib/libnccl.so"
        "/opt/conda/lib/libnccl.so"
    )

    NCCL_FOUND=false
    for path in "${NCCL_PATHS[@]}"; do
        if [ -f "$path" ]; then
            echo -e "${GREEN}✓ NCCL found: $path${NC}"
            NCCL_FOUND=true
            break
        fi
    done

    if [ "$NCCL_FOUND" = false ]; then
        echo -e "${YELLOW}⚠ NCCL not found in standard locations${NC}"
    fi
}

# Function to check NVLink
check_nvlink() {
    echo -e "\n${YELLOW}[6/8] Checking NVLink Status...${NC}"

    if nvidia-smi nvlink --status &> /dev/null; then
        echo -e "${GREEN}✓ NVLink available${NC}"
        nvidia-smi nvlink --status | grep -E "(Link|Active)" || true
    else
        echo -e "${YELLOW}⚠ NVLink not available or not active${NC}"
    fi
}

# Function to check GPU memory errors
check_gpu_errors() {
    echo -e "\n${YELLOW}[7/8] Checking GPU Memory Errors...${NC}"

    GPU_COUNT=$(nvidia-smi --query-gpu=count --format=csv,noheader | head -1)

    for i in $(seq 0 $((GPU_COUNT - 1))); do
        ECC_ERRORS=$(nvidia-smi --query-gpu=ecc.errors.corrected.aggregate.total --format=csv,noheader -i $i 2>/dev/null || echo "N/A")

        if [ "$ECC_ERRORS" != "N/A" ] && [ "$ECC_ERRORS" -gt 0 ]; then
            echo -e "${YELLOW}⚠ GPU $i: $ECC_ERRORS corrected ECC errors${NC}"
        elif [ "$ECC_ERRORS" = "N/A" ]; then
            echo -e "  GPU $i: ECC not supported"
        else
            echo -e "${GREEN}✓ GPU $i: No ECC errors${NC}"
        fi
    done
}

# Function to run quick benchmark
run_benchmark() {
    echo -e "\n${YELLOW}[8/8] Running Quick GPU Benchmark...${NC}"

    if command -v python3 &> /dev/null; then
        local bench_file
        bench_file=$(mktemp /tmp/gpu_bench.XXXXXX.py)
        cat > "$bench_file" << 'EOF'
import torch
import time

if torch.cuda.is_available():
    device = torch.device("cuda:0")
    size = 10000

    # Matrix multiplication benchmark
    a = torch.randn(size, size, device=device)
    b = torch.randn(size, size, device=device)

    # Warmup
    _ = torch.matmul(a, b)
    torch.cuda.synchronize()

    # Benchmark
    start = time.time()
    for _ in range(10):
        c = torch.matmul(a, b)
        torch.cuda.synchronize()
    elapsed = time.time() - start

    tflops = (2 * size**3 * 10) / (elapsed * 1e12)
    print(f"Performance: {tflops:.2f} TFLOPS")
    print(f"Memory allocated: {torch.cuda.memory_allocated(0) / 1e9:.2f} GB")
    print(f"Max memory allocated: {torch.cuda.max_memory_allocated(0) / 1e9:.2f} GB")
else:
    print("CUDA not available")
EOF

        python3 "$bench_file" 2>/dev/null && echo -e "${GREEN}✓ Benchmark completed${NC}" || \
            echo -e "${YELLOW}⚠ PyTorch not available, skipping benchmark${NC}"
        rm -f "$bench_file"
    else
        echo -e "${YELLOW}⚠ Python not available, skipping benchmark${NC}"
    fi
}

# Function to generate report
generate_report() {
    echo -e "\n${BLUE}╔════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${BLUE}║                    Diagnostic Summary                         ║${NC}"
    echo -e "${BLUE}╚════════════════════════════════════════════════════════════════╝${NC}"

    TIMESTAMP=$(date +"%Y-%m-%d %H:%M:%S")
    HOSTNAME=$(hostname)

    cat > /tmp/gpu_diagnostics_$HOSTNAME.txt << EOF
Gryvia GPU Diagnostics Report
==================================
Timestamp: $TIMESTAMP
Hostname: $HOSTNAME

Driver Information:
$(nvidia-smi --query-gpu=driver_version --format=csv 2>/dev/null || echo "N/A")

GPU Information:
$(nvidia-smi --query-gpu=index,name,pci.bus_id,compute_cap --format=csv 2>/dev/null || echo "N/A")

GPU Status:
$(nvidia-smi --query-gpu=temperature.gpu,power.draw,utilization.gpu,memory.used --format=csv 2>/dev/null || echo "N/A")

Topology:
$(nvidia-smi topo -m 2>/dev/null || echo "N/A")

ECC Errors:
$(nvidia-smi --query-gpu=index,ecc.errors.corrected.aggregate.total,ecc.errors.uncorrected.aggregate.total --format=csv 2>/dev/null || echo "N/A")
EOF

    echo -e "\n${GREEN}✓ Full report saved to: /tmp/gpu_diagnostics_$HOSTNAME.txt${NC}"
}

# Main execution
main() {
    check_nvidia_smi || exit 1
    check_gpu_health
    check_cuda
    check_topology
    check_nccl
    check_nvlink
    check_gpu_errors
    run_benchmark
    generate_report

    echo ""
    echo -e "${GREEN}╔════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║              Diagnostics Completed Successfully               ║${NC}"
    echo -e "${GREEN}╚════════════════════════════════════════════════════════════════╝${NC}"
}

# Run diagnostics
main "$@"
