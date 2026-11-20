// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package decoder provides security event decoding from eBPF ring buffers.
package decoder

import (
	"bytes"
	"encoding/binary"
	"errors"

	"github.com/cilium/ebpf/ringbuf"
)

// Security event type constants; values match enum sec_event_type in
// ebpf/headers/security_common.h.
const (
	SecEvtContainerEscape     uint8 = 1
	SecEvtPrivilegeEscalation uint8 = 2
	SecEvtCryptoMining        uint8 = 3
	SecEvtDataExfiltration    uint8 = 4
	SecEvtDriverTampering     uint8 = 5
	SecEvtSuspiciousExec      uint8 = 6
	SecEvtNamespaceBreach     uint8 = 7
)

// Security severity constants; values match enum sec_severity in
// ebpf/headers/security_common.h (higher is more severe).
const (
	SeverityInfo     uint8 = 0
	SeverityLow      uint8 = 1
	SeverityMedium   uint8 = 2
	SeverityHigh     uint8 = 3
	SeverityCritical uint8 = 4
)

// SecurityEventSize is sizeof(struct security_event) in the eBPF programs.
const SecurityEventSize = 336

// SecurityEvent matches struct security_event from ebpf/headers/security_common.h
// (336 bytes, all padding explicit; see TestSecurityEventLayout).
type SecurityEvent struct {
	Timestamp uint64
	PID       uint32
	UID       uint32
	GID       uint32
	EventType uint8
	Severity  uint8
	SyscallNr uint16
	CgroupID  uint64
	Comm      [16]byte
	Path      [256]byte
	SrcIP     uint32
	DstIP     uint32
	DstPort   uint16
	Pad       uint16
	Pad2      uint32 // explicit padding before Bytes (C: _pad2)
	Bytes     uint64
	OldUID    uint32
	NewUID    uint32
}

// CommString returns the process name as a Go string.
func (e *SecurityEvent) CommString() string {
	return string(bytes.TrimRight(e.Comm[:], "\x00"))
}

// PathString returns the file path as a Go string.
func (e *SecurityEvent) PathString() string {
	return string(bytes.TrimRight(e.Path[:], "\x00"))
}

// SecurityEventTypeName returns a human-readable event type name.
func SecurityEventTypeName(t uint8) string {
	switch t {
	case SecEvtContainerEscape:
		return "container_escape"
	case SecEvtPrivilegeEscalation:
		return "privilege_escalation"
	case SecEvtCryptoMining:
		return "crypto_mining"
	case SecEvtDataExfiltration:
		return "data_exfiltration"
	case SecEvtDriverTampering:
		return "driver_tampering"
	case SecEvtSuspiciousExec:
		return "suspicious_exec"
	case SecEvtNamespaceBreach:
		return "namespace_breach"
	default:
		return "unknown"
	}
}

// SeverityName returns a human-readable severity name.
func SeverityName(s uint8) string {
	switch s {
	case SeverityCritical:
		return "critical"
	case SeverityHigh:
		return "high"
	case SeverityMedium:
		return "medium"
	case SeverityLow:
		return "low"
	case SeverityInfo:
		return "info"
	default:
		return "unknown"
	}
}

// SecurityDecoder decodes security events from eBPF ring buffers.
type SecurityDecoder struct {
	events chan SecurityEvent
}

// NewSecurityDecoder creates a SecurityDecoder with the given channel buffer size.
func NewSecurityDecoder(bufSize int) *SecurityDecoder {
	if bufSize <= 0 {
		bufSize = 4096
	}
	return &SecurityDecoder{
		events: make(chan SecurityEvent, bufSize),
	}
}

// DecodeRingBuf reads security events from a ring buffer reader and publishes
// them on the events channel. It blocks until the reader is closed.
func (d *SecurityDecoder) DecodeRingBuf(reader *ringbuf.Reader) {
	for {
		record, err := reader.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			continue
		}

		var ev SecurityEvent
		if err := binary.Read(bytes.NewReader(record.RawSample), binary.LittleEndian, &ev); err != nil {
			continue
		}

		select {
		case d.events <- ev:
		default:
			// Drop event if channel is full to avoid blocking the reader.
		}
	}
}

// Events returns the read-only channel on which decoded security events are published.
func (d *SecurityDecoder) Events() <-chan SecurityEvent {
	return d.events
}

// Close closes the events channel.
func (d *SecurityDecoder) Close() {
	close(d.events)
}
