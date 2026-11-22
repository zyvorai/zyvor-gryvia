// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package trace

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/flight"
)

const tid = "0af7651916cd43dd8448eb211c80319c"

// record builds a trace_events record the way the C program lays it out: the trace id is parsed
// from hex into two host-order u64 (first 16 hex digits = hi), IPs are raw packet bytes, ports
// are host order.
func record(hi, lo, span uint64, client, server [4]byte, cport, sport uint16) []byte {
	b := make([]byte, EventSize)
	le := binary.LittleEndian
	le.PutUint64(b[0:], hi)
	le.PutUint64(b[8:], lo)
	le.PutUint64(b[16:], span)
	copy(b[32:], client[:])
	copy(b[36:], server[:])
	le.PutUint16(b[40:], cport)
	le.PutUint16(b[42:], sport)
	return b
}

var (
	client = [4]byte{203, 0, 113, 9}
	server = [4]byte{10, 1, 2, 3}
)

func obs() Observation {
	o, _ := Decode(record(0x0af7651916cd43dd, 0x8448eb211c80319c, 0xb7ad6b7169203331, client, server, 51000, 8000))
	return o
}

func TestDecode(t *testing.T) {
	o, ok := Decode(record(0x0af7651916cd43dd, 0x8448eb211c80319c, 0xb7ad6b7169203331, client, server, 51000, 8000))
	if !ok || o.TraceID != tid || o.SpanID != "b7ad6b7169203331" {
		t.Fatalf("decoded %+v ok=%v", o, ok)
	}
	if o.Client.String() != "203.0.113.9:51000" || o.Server.String() != "10.1.2.3:8000" {
		t.Errorf("endpoints %s -> %s", o.Client, o.Server)
	}
	if _, ok := Decode(make([]byte, EventSize-1)); ok {
		t.Error("short record")
	}
	if _, ok := Decode(record(0, 0, 1, client, server, 1, 2)); ok {
		t.Error("all-zero trace id is invalid")
	}
	if _, ok := Decode(record(1, 0, 0, client, server, 1, 2)); ok {
		t.Error("all-zero span id is invalid")
	}
}

func TestValidID(t *testing.T) {
	for _, ok := range []string{tid, strings.ToUpper(tid), "00000000000000000000000000000001"} {
		if !ValidID(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", tid[:31], tid + "0", "00000000000000000000000000000000", strings.Repeat("g", 32), "0af7651916cd43dd8448eb211c80319\n"} {
		if ValidID(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

type fakeResolver map[string]flight.Identity

func (f fakeResolver) ResolveIP(ip string) (flight.Identity, bool) { id, ok := f[ip]; return id, ok }

var podID = flight.Identity{Namespace: "ml", Job: "serve", Pod: "vllm-0", Node: "node-a"}

func newIdx() (*Index, *time.Time) {
	x := NewIndex()
	now := time.Unix(1_800_000_000, 0)
	x.now = func() time.Time { return now }
	return x, &now
}

func tuple(local [4]byte, lp uint16, peer [4]byte, pp uint16) flight.Tuple {
	return flight.Tuple{SrcIP: local, SrcPort: lp, DstIP: peer, DstPort: pp}
}

func TestIngestKeepsOnlyJobPods(t *testing.T) {
	x, _ := newIdx()
	r := fakeResolver{"10.1.2.3": podID}
	if !x.Ingest(obs(), r) || x.Len() != 1 {
		t.Fatal("request to a job pod must be indexed")
	}
	other := obs()
	other.Server.IP = [4]byte{10, 9, 9, 9}
	if x.Ingest(other, r) || x.Len() != 1 {
		t.Error("request to an unknown destination must be dropped")
	}
}

func TestByTupleBothDirectionsAndWindow(t *testing.T) {
	x, now := newIdx()
	x.Ingest(obs(), fakeResolver{"10.1.2.3": podID})
	at := *now
	// The flow probe sees the connection from the server side: local = pod, peer = client.
	if got, ok := x.ByTuple(tuple(server, 8000, client, 51000), at.Add(3*time.Second)); !ok || got != tid {
		t.Errorf("server-side view: %q %v", got, ok)
	}
	// And from the client side.
	if got, ok := x.ByTuple(tuple(client, 51000, server, 8000), at); !ok || got != tid {
		t.Errorf("client-side view: %q %v", got, ok)
	}
	if _, ok := x.ByTuple(tuple(server, 8000, client, 51001), at); ok {
		t.Error("a different source port is a different connection")
	}
	if _, ok := x.ByTuple(tuple(server, 8000, client, 51000), at.Add(DefaultTupleWindow+time.Second)); ok {
		t.Error("outside the window")
	}
}

func TestByTuplePicksNearestOnKeepAlive(t *testing.T) {
	x, now := newIdx()
	r := fakeResolver{"10.1.2.3": podID}
	first := obs()
	x.Ingest(first, r)
	*now = now.Add(20 * time.Second)
	second := obs()
	second.TraceID = "11111111111111111111111111111111"
	x.Ingest(second, r)
	if got, _ := x.ByTuple(tuple(server, 8000, client, 51000), *now); got != second.TraceID {
		t.Errorf("event at the second request's time got %q", got)
	}
	if got, _ := x.ByTuple(tuple(server, 8000, client, 51000), now.Add(-19*time.Second)); got != tid {
		t.Errorf("event near the first request got %q", got)
	}
}

func TestByPodPort(t *testing.T) {
	x, now := newIdx()
	r := fakeResolver{"10.1.2.3": podID}
	x.Ingest(obs(), r)
	if got, ok := x.ByPodPort("ml", "vllm-0", 8000, *now); !ok || got != tid {
		t.Errorf("got %q %v", got, ok)
	}
	if _, ok := x.ByPodPort("other", "vllm-0", 8000, *now); ok {
		t.Error("namespace must match")
	}
	if _, ok := x.ByPodPort("ml", "vllm-0", 8001, *now); ok {
		t.Error("port must match")
	}
	if _, ok := x.ByPodPort("ml", "vllm-0", 8000, now.Add(DefaultPortWindow+time.Second)); ok {
		t.Error("outside the port window")
	}
	// A second, different request from another client at the same moment: do not guess.
	o2 := obs()
	o2.TraceID, o2.Client.Port = "22222222222222222222222222222222", 51001
	x.Ingest(o2, r)
	if _, ok := x.ByPodPort("ml", "vllm-0", 8000, *now); ok {
		t.Error("competing traces must yield no match")
	}
}

func TestLookupTTLAndCap(t *testing.T) {
	x, now := newIdx()
	x.MaxTuples = 3
	r := fakeResolver{"10.1.2.3": podID}
	for i := 0; i < 5; i++ {
		o := obs()
		o.Client.Port = uint16(40000 + i)
		if i == 0 {
			o.TraceID = "33333333333333333333333333333333"
		}
		x.Ingest(o, r)
	}
	if x.Len() != 3 {
		t.Errorf("len = %d, want 3", x.Len())
	}
	if got := x.Lookup("33333333333333333333333333333333"); len(got) != 0 {
		t.Error("the oldest connection should have been evicted")
	}
	if got := x.Lookup(tid); len(got) != 3 {
		t.Errorf("lookup = %d entries", len(got))
	}
	*now = now.Add(DefaultTTL + time.Second)
	if got := x.Lookup(tid); len(got) != 0 {
		t.Error("expired entries must not be returned")
	}
	// Per-connection cap.
	y, _ := newIdx()
	for i := 0; i < 20; i++ {
		y.Ingest(obs(), r)
	}
	if got := y.Lookup(tid); len(got) != perTuple {
		t.Errorf("per-connection entries = %d", len(got))
	}
}

func handlerFixture(t *testing.T) (*Handler, *time.Time) {
	t.Helper()
	x, now := newIdx()
	x.Ingest(obs(), fakeResolver{"10.1.2.3": podID})
	rec := flight.New("node-a")
	rec.SetCorrelator(x)
	at := *now
	rec.Record(flight.Event{Time: at.Add(time.Second), Identity: podID, Source: "ebpf", Kind: "flow", Bytes: 1200,
		Tuple: &flight.Tuple{SrcIP: server, SrcPort: 8000, DstIP: client, DstPort: 51000}})
	rec.Record(flight.Event{Time: at, Identity: podID, Source: "ebpf", Kind: "inference_accept_wait", DurationNs: 900_000,
		LocalPort: 8000})
	rec.Record(flight.Event{Time: at, Identity: podID, Source: "ebpf", Kind: "flow", Bytes: 5,
		Tuple: &flight.Tuple{SrcIP: server, SrcPort: 8000, DstIP: [4]byte{198, 51, 100, 1}, DstPort: 1}}) // other connection
	return &Handler{Index: x, Recorder: rec, Node: "node-a"}, now
}

func get(h http.Handler, target string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
	return rr
}

func TestHandlerCorrelatesEvents(t *testing.T) {
	h, _ := handlerFixture(t)
	rr := get(h, "/api/v1/flight/trace?traceId="+strings.ToUpper(tid))
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var resp Response
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.TraceID != tid || resp.Node != "node-a" || len(resp.Observations) != 1 ||
		resp.Observations[0].Identity.Namespace != "ml" || resp.Observations[0].SpanID != "b7ad6b7169203331" {
		t.Errorf("resp = %+v", resp)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("events = %+v", resp.Events)
	}
	// Oldest first: the accept wait (port_time) then the flow (tuple).
	if resp.Events[0].Kind != "inference_accept_wait" || resp.Events[0].TraceMatch != "port_time" ||
		resp.Events[1].Kind != "flow" || resp.Events[1].TraceMatch != "tuple" || resp.Events[1].TraceID != tid {
		t.Errorf("events = %+v", resp.Events)
	}
	if strings.Contains(rr.Body.String(), "198.51.100.1") || strings.Contains(rr.Body.String(), "LocalPort") {
		t.Error("the unrelated connection must not appear")
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Error("responses must not be cached")
	}
}

func TestHandlerErrors(t *testing.T) {
	h, _ := handlerFixture(t)
	for target, want := range map[string]int{
		"/api/v1/flight/trace":                                          400,
		"/api/v1/flight/trace?traceId=nothex":                           400,
		"/api/v1/flight/trace?traceId=00000000000000000000000000000000": 400,
		"/api/v1/flight/trace?traceId=22222222222222222222222222222222": 404,
	} {
		rr := get(h, target)
		if rr.Code != want {
			t.Errorf("%s: %d want %d", target, rr.Code, want)
		}
		if strings.Contains(rr.Body.String(), "2222") {
			t.Error("error bodies must not echo the trace id")
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/flight/trace?traceId="+tid, nil))
	if rr.Code != 405 {
		t.Errorf("POST = %d", rr.Code)
	}
}

func TestReportAnnotatesTimelineWithoutExposingTuples(t *testing.T) {
	h, _ := handlerFixture(t)
	rep, ok := h.Recorder.Report("ml", "serve", 200)
	if !ok {
		t.Fatal("no report")
	}
	withTrace := 0
	for _, e := range rep.Events {
		if e.TraceID == tid {
			withTrace++
		}
	}
	if withTrace != 2 {
		t.Errorf("annotated events = %d, want 2", withTrace)
	}
	b, _ := json.Marshal(rep)
	if strings.Contains(string(b), "203.0.113.9") || strings.Contains(string(b), "SrcIP") {
		t.Errorf("the timeline must not expose remote addresses: %s", b)
	}
	// Without a correlator the timeline is exactly as before.
	h.Recorder.SetCorrelator(nil)
	rep, _ = h.Recorder.Report("ml", "serve", 200)
	for _, e := range rep.Events {
		if e.TraceID != "" {
			t.Error("no correlator, no trace ids")
		}
	}
}

func TestTupleFromFlowMatchesIngressOrientation(t *testing.T) {
	// decoder.FlowEvent holds the kernel word 10.1.2.3 as it reads on a little-endian host.
	ip := func(a [4]byte) uint32 { return binary.LittleEndian.Uint32(a[:]) }
	x, now := newIdx()
	x.Ingest(obs(), fakeResolver{"10.1.2.3": podID})
	tp := TupleFromFlow(ip(server), 8000, ip(client), 51000)
	if tp.SrcIP != server || tp.DstIP != client {
		t.Fatalf("tuple = %+v", tp)
	}
	if got, ok := x.ByTuple(tp, *now); !ok || got != tid {
		t.Errorf("flow event did not correlate: %q %v", got, ok)
	}
}
