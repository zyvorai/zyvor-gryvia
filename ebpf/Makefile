# Gryvia eBPF programs: CO-RE, no generated vmlinux.h (see headers/gryvia_core.h).
CLANG   ?= clang
BPFTOOL ?= bpftool

# Target architecture for bpf_tracing.h / syscall numbers (x86 or arm64).
# Override with `make ARCH=arm64` to cross-compile.
UNAME_M := $(shell uname -m)
ARCH    ?= $(shell echo $(UNAME_M) | sed -e 's/x86_64/x86/' -e 's/aarch64/arm64/')

# <asm/types.h> etc. live in the Debian/Ubuntu multiarch include dir (which
# clang -target bpf does not search).  Use the host's when it exists; for a
# cross build fall back to the target arch's directory.
ifeq ($(ARCH),arm64)
MULTIARCH := aarch64-linux-gnu
else
MULTIARCH := x86_64-linux-gnu
endif
MULTIARCH_INC := $(firstword $(wildcard /usr/include/$(UNAME_M)-linux-gnu /usr/include/$(MULTIARCH)))
ARCH_INC := $(if $(MULTIARCH_INC),-idirafter $(MULTIARCH_INC))

CFLAGS  = -O2 -g -target bpf -D__TARGET_ARCH_$(ARCH) \
          -I. $(ARCH_INC) \
          -Wall -Werror

PROGS = tcp_trace packet_filter latency_probe syscall_monitor dns_tracker \
        nccl_trace gpu_mem_trace rdma_trace training_pattern datapipe_bottleneck gradient_compress \
        container_escape crypto_detect exfil_detect privesc_monitor driver_fim \
        tcp_tuning connpool_analyze numa_path sockops_optimize trace_correlator \
        latency_breakdown cost_tracker fingerprint \
        straggler rdma_health gds_trace overlap roce_cnp infer_latency \
        ucx_gloo pfc_pause quota_pace weight_exfil ibv_verbs \
        xdp_mux nccl_transport p2p_fallback capture_gate \
        roce_ecn gpu_oom graph_stall gdr_fail infer_ttft weight_mmap gpu_dev ucx_complete

# Where `make check` pins programs while verifying them (removed afterwards).
CHECK_DIR ?= /sys/fs/bpf/gryvia-check-$(shell echo $$PPID)

.PHONY: all clean check

all: $(PROGS:%=%.o)

HDRS = headers/gryvia_core.h headers/common.h headers/gpu_common.h headers/security_common.h headers/fabric_signal.h headers/xdp_chain.h

%.o: %.c $(HDRS)
	$(CLANG) $(CFLAGS) -c $< -o $@

# Run `make -k` first.  Load every built object through the kernel verifier (needs root: the
# programs are pinned under a private bpffs dir that is always removed).
# Programs of type uprobe/tracepoint/xdp/... are loaded, never attached.
check:
	@set -e; rc=0; \
	for o in $(PROGS:%=%.o); do \
		if [ ! -f $$o ]; then echo "MISSING   $$o (build failed? run: make -k)"; rc=1; continue; fi; \
		d=$(CHECK_DIR)-$${o%.o}; \
		if sudo $(BPFTOOL) prog loadall $$o $$d >/tmp/gryvia-check.$$$$.log 2>&1; then \
			echo "LOAD OK   $$o"; \
		else \
			echo "LOAD FAIL $$o"; sed 's/^/    /' /tmp/gryvia-check.$$$$.log | tail -40; rc=1; \
		fi; \
		sudo rm -rf $$d; rm -f /tmp/gryvia-check.$$$$.log; \
	done; exit $$rc

clean:
	rm -f *.o
