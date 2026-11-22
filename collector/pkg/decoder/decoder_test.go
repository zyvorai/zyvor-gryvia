// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package decoder

import (
	"encoding/binary"
	"testing"
)

// TestDecodeFlowEventLayout builds a buffer with the byte offsets the C
// struct flow_event (ebpf/headers/common.h, 64 bytes, explicit padding)
// produces and checks that decode() reads every field from the right place.
func TestDecodeFlowEventLayout(t *testing.T) {
	buf := make([]byte, 64)
	le := binary.LittleEndian
	le.PutUint64(buf[0:], 111)         // timestamp
	le.PutUint32(buf[8:], 0x0a000001)  // src_ip
	le.PutUint32(buf[12:], 0x0a000002) // dst_ip
	le.PutUint16(buf[16:], 1234)       // src_port
	le.PutUint16(buf[18:], 443)        // dst_port
	buf[20] = 6                        // protocol
	buf[21] = 1                        // verdict
	le.PutUint32(buf[24:], 4096)       // bytes
	le.PutUint64(buf[32:], 987654)     // latency_ns
	le.PutUint32(buf[40:], 4242)       // pid
	copy(buf[44:], "curl")             // comm

	d := &Decoder{}
	ev, err := d.decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Timestamp != 111 || ev.SrcIP != 0x0a000001 || ev.DstIP != 0x0a000002 ||
		ev.SrcPort != 1234 || ev.DstPort != 443 || ev.Protocol != 6 || ev.Verdict != 1 ||
		ev.Bytes != 4096 || ev.LatencyNs != 987654 || ev.PID != 4242 || ev.Comm != "curl" {
		t.Fatalf("unexpected decode: %+v", ev)
	}
}

// TestRawFlowEventSize pins the Go mirror to the C static asserts in
// ebpf/headers/common.h (sizeof == 64).
func TestRawFlowEventSize(t *testing.T) {
	if got := binary.Size(rawFlowEvent{}); got != 64 {
		t.Fatalf("rawFlowEvent is %d bytes, C struct flow_event is 64", got)
	}
}
