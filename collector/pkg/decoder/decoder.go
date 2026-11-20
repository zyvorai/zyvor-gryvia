// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package decoder reads raw perf events from eBPF ring buffers,
// deserialises them into FlowEvent structs, and enriches each event
// with Kubernetes metadata resolved from the local /proc filesystem
// and the downward API.
package decoder

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/cilium/ebpf/perf"
	"go.uber.org/zap"
)

// FlowEvent is the userspace representation of a kernel flow_event.
type FlowEvent struct {
	Timestamp uint64
	SrcIP     uint32
	DstIP     uint32
	SrcPort   uint16
	DstPort   uint16
	Protocol  uint8
	Verdict   uint8
	Bytes     uint32
	LatencyNs uint64
	PID       uint32
	Comm      string

	// Enriched fields (populated after decoding).
	SrcPod     string
	DstPod     string
	SrcService string
	DstService string
	Namespace  string
}

// rawFlowEvent mirrors the kernel struct layout for binary decoding.
type rawFlowEvent struct {
	Timestamp uint64
	SrcIP     uint32
	DstIP     uint32
	SrcPort   uint16
	DstPort   uint16
	Protocol  uint8
	Verdict   uint8
	_         [2]byte // padding (_pad1)
	Bytes     uint32
	_         [4]byte // padding (_pad2): C aligns latency_ns to 8 bytes
	LatencyNs uint64
	PID       uint32
	Comm      [16]byte
	_         [4]byte // tail padding (_pad3): sizeof(struct flow_event) == 64
}

// Decoder reads from one or more perf.Reader instances and emits
// decoded, enriched FlowEvents on a channel.
type Decoder struct {
	readers []*perf.Reader
	log     *zap.SugaredLogger
	eventCh chan FlowEvent
	lost    atomic.Uint64
}

// Lost counts perf samples the kernel could not deliver (buffer overrun), as
// reported by the perf reader. The tcp_trace `drops` map counts the same
// condition from the producer side.
func (d *Decoder) Lost() uint64 { return d.lost.Load() }

// New creates a Decoder that consumes from the given perf readers.
func New(readers []*perf.Reader, log *zap.SugaredLogger) (*Decoder, error) {
	if len(readers) == 0 {
		return nil, fmt.Errorf("no perf readers provided")
	}
	return &Decoder{
		readers: readers,
		log:     log,
		eventCh: make(chan FlowEvent, 4096),
	}, nil
}

// Events returns the channel on which decoded events are published.
func (d *Decoder) Events() <-chan FlowEvent {
	return d.eventCh
}

// Run starts reading from all perf readers.  It blocks until ctx is
// cancelled, at which point it closes the event channel.
func (d *Decoder) Run(ctx context.Context) {
	defer close(d.eventCh)

	for _, r := range d.readers {
		go d.readLoop(ctx, r)
	}

	<-ctx.Done()
}

func (d *Decoder) readLoop(ctx context.Context, reader *perf.Reader) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		record, err := reader.Read()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.log.Warnw("perf read error", "error", err)
			continue
		}

		if record.LostSamples > 0 {
			d.lost.Add(record.LostSamples)
			d.log.Warnw("lost perf samples", "count", record.LostSamples)
			continue
		}

		ev, err := d.decode(record.RawSample)
		if err != nil {
			d.log.Debugw("decode error", "error", err)
			continue
		}

		// Enrich with process/pod info.
		d.enrich(&ev)

		select {
		case d.eventCh <- ev:
		case <-ctx.Done():
			return
		}
	}
}

func (d *Decoder) decode(data []byte) (FlowEvent, error) {
	var raw rawFlowEvent
	reader := bytes.NewReader(data)
	if err := binary.Read(reader, binary.LittleEndian, &raw); err != nil {
		return FlowEvent{}, fmt.Errorf("binary decode: %w", err)
	}

	comm := string(bytes.TrimRight(raw.Comm[:], "\x00"))

	return FlowEvent{
		Timestamp: raw.Timestamp,
		SrcIP:     raw.SrcIP,
		DstIP:     raw.DstIP,
		SrcPort:   raw.SrcPort,
		DstPort:   raw.DstPort,
		Protocol:  raw.Protocol,
		Verdict:   raw.Verdict,
		Bytes:     raw.Bytes,
		LatencyNs: raw.LatencyNs,
		PID:       raw.PID,
		Comm:      comm,
	}, nil
}

// enrich resolves the PID to a pod name using /proc and the Kubernetes
// downward API environment variables.
func (d *Decoder) enrich(ev *FlowEvent) {
	if ev.PID == 0 {
		return
	}

	// Read cgroup to identify the pod.
	cgroupPath := fmt.Sprintf("/proc/%d/cgroup", ev.PID)
	data, err := os.ReadFile(cgroupPath)
	if err != nil {
		return
	}

	// Parse cgroup for pod UID -- k8s cgroups contain "pod<uid>".
	for _, line := range strings.Split(string(data), "\n") {
		if idx := strings.Index(line, "pod"); idx != -1 {
			// Extract a rough pod identifier from the cgroup path.
			parts := strings.Split(line[idx:], "/")
			if len(parts) > 0 {
				ev.SrcPod = parts[0]
			}
			break
		}
	}

	// The collector's downward API namespace is not the workload namespace.
	// A Kubernetes pod cache resolves the workload in the event pipeline.
}

// IPToString converts a uint32 IP (network byte order) to dotted quad.
func IPToString(ip uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d",
		ip&0xFF, (ip>>8)&0xFF, (ip>>16)&0xFF, (ip>>24)&0xFF)
}
