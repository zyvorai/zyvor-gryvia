// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package decoder

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unsafe"
)

// cGPUEventSize is sizeof(struct gpu_event) in ebpf/headers/gpu_common.h
// (asserted there with _Static_assert).
const cGPUEventSize = 72

func TestGPUEventLayout(t *testing.T) {
	if got := binary.Size(GPUEvent{}); got != cGPUEventSize {
		t.Fatalf("binary.Size(GPUEvent) = %d, want %d", got, cGPUEventSize)
	}
	// Go's in-memory layout must agree too (all fields naturally aligned).
	if got := unsafe.Sizeof(GPUEvent{}); got != cGPUEventSize {
		t.Fatalf("unsafe.Sizeof(GPUEvent) = %d, want %d", got, cGPUEventSize)
	}
}

// TestGPUEventDecodeOffsets builds a buffer with the byte offsets the C
// struct produces and checks every field lands where C put it.
func TestGPUEventDecodeOffsets(t *testing.T) {
	buf := make([]byte, cGPUEventSize)
	le := binary.LittleEndian
	le.PutUint64(buf[0:], 111)     // timestamp
	le.PutUint32(buf[8:], 4242)    // pid
	le.PutUint32(buf[12:], 3)      // gpu_id
	buf[16] = GPUEvtNCCLOp         // event_type
	buf[17] = MemDirD2H            // direction
	buf[18] = NCCLOpAllGather      // nccl_op
	buf[19] = 0                    // _pad
	le.PutUint32(buf[20:], 0)      // _pad2
	le.PutUint64(buf[24:], 1<<20)  // bytes
	le.PutUint64(buf[32:], 987654) // latency_ns
	le.PutUint32(buf[40:], 1)      // src_rank
	le.PutUint32(buf[44:], 2)      // dst_rank
	le.PutUint32(buf[48:], 7)      // collective_id
	le.PutUint32(buf[52:], 8)      // world_size
	copy(buf[56:], "python3")      // comm[16]

	var ev GPUEvent
	if err := binary.Read(bytes.NewReader(buf), le, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Timestamp != 111 || ev.PID != 4242 || ev.GPUID != 3 ||
		ev.EventType != GPUEvtNCCLOp || ev.Direction != MemDirD2H ||
		ev.NCCLOp != NCCLOpAllGather || ev.Bytes != 1<<20 ||
		ev.LatencyNs != 987654 || ev.SrcRank != 1 || ev.DstRank != 2 ||
		ev.CollectiveID != 7 || ev.WorldSize != 8 || ev.CommString() != "python3" {
		t.Fatalf("unexpected decode: %+v", ev)
	}
}

// TestNCCLOpValues pins the enum values to enum nccl_op_type in gpu_common.h.
func TestNCCLOpValues(t *testing.T) {
	want := map[string]uint8{
		"AllReduce": 0, "AllGather": 1, "Broadcast": 2, "Reduce": 3,
		"ReduceScatter": 4, "Send": 5, "Recv": 6, "Group": 7,
	}
	for name, v := range want {
		if got := NCCLOpName(v); got != name {
			t.Errorf("NCCLOpName(%d) = %q, want %q", v, got, name)
		}
	}
}
