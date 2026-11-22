// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package decoder

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestSecurityEventLayout builds a buffer with the byte offsets of the C
// struct security_event (ebpf/headers/security_common.h, 336 bytes) and
// checks binary.Read maps every field correctly.
func TestSecurityEventLayout(t *testing.T) {
	if got := binary.Size(SecurityEvent{}); got != SecurityEventSize {
		t.Fatalf("SecurityEvent size = %d, want %d", got, SecurityEventSize)
	}

	buf := make([]byte, SecurityEventSize)
	le := binary.LittleEndian
	le.PutUint64(buf[0:], 111)   // timestamp
	le.PutUint32(buf[8:], 4242)  // pid
	le.PutUint32(buf[12:], 1000) // uid
	le.PutUint32(buf[16:], 1001) // gid
	buf[20] = SecEvtDataExfiltration
	buf[21] = SeverityCritical
	le.PutUint16(buf[22:], 42)     // syscall_nr
	le.PutUint64(buf[24:], 777)    // cgroup_id
	copy(buf[32:], "curl")         // comm
	copy(buf[48:], "core_pattern") // path
	le.PutUint32(buf[304:], 0x0100000a)
	le.PutUint32(buf[308:], 0x08080808)
	le.PutUint16(buf[312:], 443)
	le.PutUint64(buf[320:], 1<<40) // bytes
	le.PutUint32(buf[328:], 1000)  // old_uid
	le.PutUint32(buf[332:], 0)     // new_uid

	var ev SecurityEvent
	if err := binary.Read(bytes.NewReader(buf), binary.LittleEndian, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Timestamp != 111 || ev.PID != 4242 || ev.UID != 1000 || ev.GID != 1001 ||
		ev.EventType != 4 || ev.Severity != 4 || ev.SyscallNr != 42 || ev.CgroupID != 777 ||
		ev.CommString() != "curl" || ev.PathString() != "core_pattern" ||
		ev.SrcIP != 0x0100000a || ev.DstIP != 0x08080808 || ev.DstPort != 443 ||
		ev.Bytes != 1<<40 || ev.OldUID != 1000 || ev.NewUID != 0 {
		t.Fatalf("unexpected decode: %+v", ev)
	}
}

func TestSecurityEnumsMatchC(t *testing.T) {
	// enum sec_event_type / sec_severity in security_common.h
	if SecEvtContainerEscape != 1 || SecEvtPrivilegeEscalation != 2 || SecEvtCryptoMining != 3 ||
		SecEvtDataExfiltration != 4 || SecEvtDriverTampering != 5 || SecEvtSuspiciousExec != 6 ||
		SecEvtNamespaceBreach != 7 {
		t.Fatal("event type constants drifted from C")
	}
	if SeverityInfo != 0 || SeverityLow != 1 || SeverityMedium != 2 || SeverityHigh != 3 || SeverityCritical != 4 {
		t.Fatal("severity constants drifted from C")
	}
	if SeverityName(SeverityCritical) != "critical" || SecurityEventTypeName(SecEvtDriverTampering) != "driver_tampering" {
		t.Fatal("names wrong")
	}
}
