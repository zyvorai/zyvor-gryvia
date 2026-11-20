/* SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause */
#ifndef __GPU_COMMON_H__
#define __GPU_COMMON_H__

#include "gryvia_core.h"

#define MAX_RANKS 512
#define NCCL_OP_NAMES_LEN 8

/* GPU event types */
enum gpu_event_type {
    GPU_EVT_NCCL_OP = 1,
    GPU_EVT_MEM_TRANSFER = 2,
    GPU_EVT_RDMA_SEND = 3,
    GPU_EVT_RDMA_RECV = 4,
    GPU_EVT_CUDA_LAUNCH = 5,
    GPU_EVT_CUDA_SYNC = 6,
    /* Types below are NOT collectives/copies: userspace consumers that only
     * switch on types 1-6 correctly ignore them. */
    GPU_EVT_CUDA_ALLOC = 7,     /* bytes = size, direction 0=malloc 1=free */
    GPU_EVT_GRAD_COMPRESS = 8,  /* bytes = actual, src/dst_rank = expected hi/lo */
    GPU_EVT_TRAIN_CYCLE = 9,    /* bytes = data ingested, latency = comm ns,
                                 * collective_id = compute gap in us */
    GPU_EVT_PIPE_STALL = 10,    /* latency = GPU idle ns before a launch */
    GPU_EVT_PIPE_BUSY = 11,     /* latency = GPU busy ns since last launch batch */
};

/* Memory transfer directions */
enum mem_direction {
    MEM_H2D = 0,    /* Host to Device */
    MEM_D2H = 1,    /* Device to Host */
    MEM_D2D = 2,    /* Device to Device */
    MEM_PEER = 3,   /* Peer (cross-GPU) */
    MEM_UNKNOWN = 4, /* cudaMemcpyDefault: direction inferred from pointers */
};

/* NCCL collective operation types */
enum nccl_op_type {
    NCCL_ALLREDUCE = 0,
    NCCL_ALLGATHER = 1,
    NCCL_BROADCAST = 2,
    NCCL_REDUCE = 3,
    NCCL_REDUCESCATTER = 4,
    NCCL_SEND = 5,
    NCCL_RECV = 6,
    NCCL_ALLTOALL = 7,
};

/* GPU event structure shared with userspace */
struct gpu_event {
    __u64 timestamp;
    __u32 pid;
    __u32 gpu_id;
    __u8  event_type;     /* gpu_event_type */
    __u8  direction;      /* mem_direction */
    __u8  nccl_op;        /* nccl_op_type */
    __u8  _pad;
    __u32 _pad2;          /* explicit: bytes is 8-byte aligned at offset 24 */
    __u64 bytes;
    __u64 latency_ns;
    __u32 src_rank;
    __u32 dst_rank;
    __u32 collective_id;
    __u32 world_size;
    char  comm[16];
};

_Static_assert(sizeof(struct gpu_event) == 72, "gpu_event ABI: keep in sync with GPUEvent in collector/pkg/decoder/gpu_decoder.go");
_Static_assert(__builtin_offsetof(struct gpu_event, bytes) == 24, "gpu_event.bytes offset");

/* NCCL in-flight operation tracking */
struct nccl_inflight {
    __u64 start_ns;
    __u64 bytes;          /* count * sizeof(datatype) */
    __u8  op_type;
    __u8  _pad[3];
    __u32 rank;
};

/* ncclDataType_t element sizes (nccl.h): 0=int8 1=uint8 2=int32 3=uint32
 * 4=int64 5=uint64 6=half 7=float 8=double 9=bfloat16 10/11=fp8. */
static __always_inline __u64 nccl_dtype_size(__u64 dtype)
{
    switch (dtype) {
    case 0: case 1: case 10: case 11: return 1;
    case 6: case 9: return 2;
    case 2: case 3: case 7: return 4;
    case 4: case 5: case 8: return 8;
    default: return 4;
    }
}

/* CUDA memory operation tracking */
struct cuda_mem_inflight {
    __u64 start_ns;
    __u64 size;
    __u64 aux;            /* cudaMalloc: the user's void **devPtr */
    __u8  direction;
};

/* RDMA queue pair tracking */
struct rdma_qp_info {
    __u64 bytes_sent;
    __u64 bytes_recv;
    __u64 last_send_ns;
    __u32 retransmits;
    __u32 completions;
};

#endif /* __GPU_COMMON_H__ */
