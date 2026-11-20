// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package netcost

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/kube"
)

// The test cluster: node n1 (zone a) hosts pods of tenants alpha and beta plus kube-system; n2 (zone b)
// and n3 (zone a) host more pods. All JSON is crafted; nothing here ran against a real API server.
const nodesJSON = `{"items":[
 {"metadata":{"name":"n1","labels":{"topology.kubernetes.io/zone":"a"}},"status":{"addresses":[{"type":"InternalIP","address":"192.168.0.1"}]}},
 {"metadata":{"name":"n2","labels":{"topology.kubernetes.io/zone":"b"}},"status":{"addresses":[{"type":"InternalIP","address":"192.168.0.2"}]}},
 {"metadata":{"name":"n3","labels":{"topology.kubernetes.io/zone":"a"}},"status":{"addresses":[{"type":"InternalIP","address":"192.168.0.3"}]}},
 {"metadata":{"name":"n4"},"status":{"addresses":[{"type":"InternalIP","address":"192.168.0.4"}]}}]}`

const nsJSON = `{"items":[
 {"metadata":{"name":"tenant-alpha","labels":{"gryvia.io/tenant":"alpha"}}},
 {"metadata":{"name":"tenant-beta"}},
 {"metadata":{"name":"kube-system"}},
 {"metadata":{"name":"sneaky","labels":{"gryvia.io/tenant":"alpha"}}}]}`

func pod(ns, name, ip, node string, extra string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":%q},"spec":{"nodeName":%q%s},"status":{"podIP":%q,"phase":"Running"}}`,
		name, ns, node, extra, ip)
}

func dir(t *testing.T, pods ...string) *Directory {
	t.Helper()
	d := NewDirectory()
	if err := d.Set([]byte(`{"items":[`+strings.Join(pods, ",")+`]}`), []byte(nodesJSON), []byte(nsJSON)); err != nil {
		t.Fatal(err)
	}
	return d
}

func stdDir(t *testing.T) *Directory {
	return dir(t,
		pod("tenant-alpha", "a1", "10.0.1.1", "n1", ""),
		pod("tenant-alpha", "a2", "10.0.1.2", "n1", ""),    // same node as a1
		pod("tenant-alpha", "a3", "10.0.2.1", "n2", ""),    // other zone
		pod("tenant-alpha", "a4", "10.0.3.1", "n3", ""),    // same zone, other node
		pod("tenant-beta", "b1", "10.0.1.9", "n1", ""),     // other tenant, same node
		pod("tenant-beta", "b2", "10.0.2.9", "n2", ""),     // other tenant, other zone
		pod("kube-system", "dns", "10.0.3.9", "n3", ""),    // in-cluster non-tenant
		pod("tenant-alpha", "n4pod", "10.0.4.1", "n4", ""), // node without zone label
		pod("tenant-alpha", "hn", "192.168.0.1", "n1", `,"hostNetwork":true`),
		pod("sneaky", "s", "10.0.5.5", "n1", ""), // label says alpha
		pod("tenant-alpha", "dupA", "10.0.6.6", "n1", ""),
		pod("tenant-beta", "dupB", "10.0.6.6", "n1", ""))
}

func TestClassifyMatrix(t *testing.T) {
	d := stdDir(t)
	cases := []struct {
		name          string
		local, remote string
		ok            bool
		why           string
		peer          PeerClass
		zone          ZoneClass
		tenant        string
	}{
		{"same tenant same zone", "10.0.1.1", "10.0.3.1", true, "", PeerSameTenant, ZoneSame, "alpha"},
		{"same tenant cross zone", "10.0.1.1", "10.0.2.1", true, "", PeerSameTenant, ZoneCross, "alpha"},
		{"other tenant cross zone", "10.0.1.1", "10.0.2.9", true, "", PeerOtherTenant, ZoneCross, "alpha"},
		{"other tenant same node kept", "10.0.1.1", "10.0.1.9", true, "", PeerOtherTenant, ZoneSameNode, "alpha"},
		{"same tenant same node excluded", "10.0.1.1", "10.0.1.2", false, SkipSameNode, "", "", ""},
		{"kube-system pod same zone", "10.0.1.1", "10.0.3.9", true, "", PeerCluster, ZoneSame, "alpha"},
		{"node ip peer", "10.0.1.1", "192.168.0.2", true, "", PeerCluster, ZoneCross, "alpha"},
		{"own node ip is not same-node pod", "10.0.1.1", "192.168.0.1", true, "", PeerCluster, ZoneSame, "alpha"},
		{"peer without zone label", "10.0.1.1", "10.0.4.1", true, "", PeerSameTenant, ZoneUnknown, "alpha"},
		{"external", "10.0.1.1", "8.8.8.8", true, "", PeerExternal, ZoneInternet, "alpha"},
		{"unknown private", "10.0.1.1", "10.99.0.1", true, "", PeerUnknown, ZoneUnknown, "alpha"},
		{"cgnat is not internet", "10.0.1.1", "100.100.1.1", true, "", PeerUnknown, ZoneUnknown, "alpha"},
		{"loopback", "127.0.0.1", "127.0.0.1", false, SkipLoopback, "", "", ""},
		{"local not on this node", "10.0.2.1", "8.8.8.8", false, SkipUnattributed, "", "", ""},
		{"local unknown ip (SNAT)", "10.77.7.7", "8.8.8.8", false, SkipUnattributed, "", "", ""},
		{"local is a node ip", "192.168.0.1", "8.8.8.8", false, SkipUnattributed, "", "", ""},
		{"hostNetwork pod resolves to node", "192.168.0.1", "10.0.3.1", false, SkipUnattributed, "", "", ""},
		{"local non-tenant pod", "10.0.3.9", "8.8.8.8", false, SkipUnattributed, "", "", ""},
		{"ambiguous local ip", "10.0.6.6", "8.8.8.8", false, SkipUnattributed, "", "", ""},
		{"label wins over name", "10.0.5.5", "8.8.8.8", true, "", PeerExternal, ZoneInternet, "alpha"},
		{"mixed families are invalid", "fd00::1", "8.8.8.8", false, SkipInvalid, "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, ok, why := Classify(d, "n1", c.local, c.remote)
			if ok != c.ok || why != c.why {
				t.Fatalf("ok=%v why=%q, want ok=%v why=%q (%+v)", ok, why, c.ok, c.why, v)
			}
			if ok && (v.Peer != c.peer || v.Zone != c.zone || v.Tenant != c.tenant) {
				t.Fatalf("got %+v want %s/%s/%s", v, c.tenant, c.peer, c.zone)
			}
		})
	}
	if _, ok, _ := Classify(d, "", "10.0.1.1", "8.8.8.8"); ok {
		t.Fatal("empty node must fail closed")
	}
}

func TestDirectoryFailsClosedWhenStaleOrEmpty(t *testing.T) {
	if _, ok := NewDirectory().Lookup("10.0.1.1"); ok {
		t.Fatal("an empty directory resolves nothing")
	}
	d := stdDir(t)
	now := time.Now()
	d.now = func() time.Time { return now.Add(3 * time.Minute) }
	if _, ok := d.Lookup("10.0.1.1"); ok {
		t.Fatal("a stale directory must resolve nothing")
	}
}

func TestDirectoryRefreshFollowsPages(t *testing.T) {
	g := fakeGetter{}
	err := stdDir(t).Refresh(context.Background(), &g)
	if err == nil {
		t.Fatal("refresh against an unreachable getter must fail")
	}
	g = fakeGetter{pods: []string{
		`{"metadata":{"continue":"tok"},"items":[` + pod("tenant-alpha", "a1", "10.0.1.1", "n1", "") + `]}`,
		`{"metadata":{},"items":[` + pod("tenant-beta", "b1", "10.0.1.9", "n1", "") + `]}`}}
	d := NewDirectory()
	if err := d.Refresh(context.Background(), &g); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Lookup("10.0.1.9"); !ok || g.calls != 4 || !strings.Contains(g.paths[1], "continue=tok") {
		t.Fatalf("paging broken: calls=%d paths=%v", g.calls, g.paths)
	}
}

type fakeGetter struct {
	pods  []string
	calls int
	paths []string
}

func (g *fakeGetter) Get(_ context.Context, path string) ([]byte, error) {
	g.paths = append(g.paths, path)
	g.calls++
	switch {
	case strings.HasPrefix(path, "/api/v1/pods"):
		if len(g.pods) == 0 {
			return nil, fmt.Errorf("down")
		}
		p := g.pods[0]
		g.pods = g.pods[1:]
		return []byte(p), nil
	case strings.HasPrefix(path, "/api/v1/nodes"):
		return []byte(nodesJSON), nil
	default:
		return []byte(nsJSON), nil
	}
}

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestStructLayoutMatchesKernel(t *testing.T) {
	if n := binary.Size(costKey6{}); n != 32 {
		t.Fatalf("cost_key6 is 32 bytes, got %d", n)
	}
	if n := binary.Size(costKey{}); n != 8 {
		t.Fatalf("cost_key is 8 bytes, got %d", n)
	}
	if n := binary.Size(costValue{}); n != 32 {
		t.Fatalf("cost_value is 32 bytes, got %d", n)
	}
}

func find(bs []Bucket, tenant string, p PeerClass, z ZoneClass) Totals {
	for _, b := range bs {
		if b.Tenant == tenant && b.Peer == p && b.Zone == z {
			return b.Totals
		}
	}
	return Totals{}
}

var t0 = time.Date(2026, 3, 10, 10, 20, 0, 0, time.UTC)

// A connection A(n1, alpha) -> B(n2, beta) is seen at both ends: node n1 sees egress of A and node n2 sees the
// same bytes as ingress of B. Only egress is billed, so the bytes are billed once, to alpha.
func TestBothEndsBilledOnce(t *testing.T) {
	d := stdDir(t)
	n1, n2 := NewMeter(d, "n1"), NewMeter(d, "n2")
	// the request A -> B (1000 B) and the reply B -> A (300 B)
	n1.Observe([]Sample{{Local: addr("10.0.1.1"), Remote: addr("10.0.2.9"), BytesSent: 1000, BytesRecv: 300}}, t0)
	n2.Observe([]Sample{{Local: addr("10.0.2.9"), Remote: addr("10.0.1.1"), BytesSent: 300, BytesRecv: 1000}}, t0)
	a := find(n1.Snapshot(), "alpha", PeerOtherTenant, ZoneCross)
	b := find(n2.Snapshot(), "beta", PeerOtherTenant, ZoneCross)
	if a.Egress != 1000 || b.Egress != 300 {
		t.Fatalf("each tenant is billed for what its own pods sent: alpha=%+v beta=%+v", a, b)
	}
	if a.Egress+b.Egress != 1300 {
		t.Fatal("total egress across nodes must equal the bytes sent once")
	}
	if a.Ingress != 300 || b.Ingress != 1000 {
		t.Fatal("ingress is recorded but is never part of egress")
	}
}

// A packet only forwarded through n2 (its source pod lives on n1) must not be attributed by n2.
func TestForeignPodTrafficIgnored(t *testing.T) {
	m := NewMeter(stdDir(t), "n2")
	m.Observe([]Sample{{Local: addr("10.0.1.1"), Remote: addr("8.8.8.8"), BytesSent: 5000}}, t0)
	if len(m.Snapshot()) != 0 || m.Stats().Skipped[SkipUnattributed] != 5000 {
		t.Fatalf("%+v %+v", m.Snapshot(), m.Stats())
	}
}

func TestDeltasAreIdempotent(t *testing.T) {
	m := NewMeter(stdDir(t), "n1")
	s := Sample{Local: addr("10.0.1.1"), Remote: addr("8.8.8.8"), BytesSent: 1000}
	m.Observe([]Sample{s}, t0)
	m.Observe([]Sample{s}, t0.Add(15*time.Second)) // same cumulative value: nothing new
	m.Observe([]Sample{s, s}, t0.Add(30*time.Second))
	if e := find(m.Snapshot(), "alpha", PeerExternal, ZoneInternet).Egress; e != 1000 {
		t.Fatalf("re-observing must add 0, got %d", e)
	}
	s.BytesSent = 1600
	m.Observe([]Sample{s}, t0.Add(45*time.Second))
	if e := find(m.Snapshot(), "alpha", PeerExternal, ZoneInternet).Egress; e != 1600 {
		t.Fatalf("delta 600 expected, got %d", e)
	}
	// counter reset (kernel LRU eviction / program reload): the new value is the delta
	s.BytesSent = 200
	m.Observe([]Sample{s}, t0.Add(60*time.Second))
	if e := find(m.Snapshot(), "alpha", PeerExternal, ZoneInternet).Egress; e != 1800 {
		t.Fatalf("reset must add the new value, got %d", e)
	}
	// evicted pair (missing from a scan) that returns starts again from 0
	m.Observe(nil, t0.Add(75*time.Second))
	s.BytesSent = 50
	m.Observe([]Sample{s}, t0.Add(90*time.Second))
	if e := find(m.Snapshot(), "alpha", PeerExternal, ZoneInternet).Egress; e != 1850 {
		t.Fatalf("got %d", e)
	}
}

// tcp_trace connect/close events carry no reliable bytes; they must never add to the totals, alone or next
// to byte deltas.
func TestFlowEventsNeverBilled(t *testing.T) {
	m := NewMeter(stdDir(t), "n1")
	m.Observe([]Sample{{Local: addr("10.0.1.1"), Remote: addr("8.8.8.8"), BytesSent: 700}}, t0)
	for i := 0; i < 5; i++ {
		m.NoteFlowEvent() // connect + close (+ retransmit observation) of the same connection
	}
	m.Observe([]Sample{{Local: addr("10.0.1.1"), Remote: addr("8.8.8.8"), BytesSent: 700}}, t0.Add(time.Minute))
	if e := find(m.Snapshot(), "alpha", PeerExternal, ZoneInternet).Egress; e != 700 {
		t.Fatalf("got %d", e)
	}
	if m.Stats().FlowEventsIgnored != 5 {
		t.Fatal("ignored events must be counted")
	}
}

func TestLoopbackAndSameNodeExcluded(t *testing.T) {
	m := NewMeter(stdDir(t), "n1")
	m.Observe([]Sample{
		{Local: addr("127.0.0.1"), Remote: addr("127.0.0.1"), BytesSent: 10},
		{Local: addr("10.0.1.1"), Remote: addr("10.0.1.2"), BytesSent: 20},  // same tenant, same node
		{Local: addr("10.0.1.1"), Remote: addr("10.0.1.9"), BytesSent: 40}}, // other tenant, same node
		t0)
	bs := m.Snapshot()
	if len(bs) != 1 || find(bs, "alpha", PeerOtherTenant, ZoneSameNode).Egress != 40 {
		t.Fatalf("%+v", bs)
	}
}

func TestBoundedBuckets(t *testing.T) {
	m := NewMeter(stdDir(t), "n1")
	for i := 0; i < MaxBuckets; i++ {
		m.buckets[Key{Hour: t0.Add(time.Duration(i) * time.Hour), Tenant: "x"}] = &Totals{}
	}
	m.Observe([]Sample{{Local: addr("10.0.1.1"), Remote: addr("8.8.8.8"), BytesSent: 9}}, t0)
	if len(m.buckets) != MaxBuckets || m.Stats().DroppedBucketBytes != 9 {
		t.Fatal("bucket limit not enforced")
	}
}

// ---- publisher --------------------------------------------------------------------------------------------

type fakeAPI struct {
	store map[string]map[string]any // path -> object
	posts int
}

func newAPI() *fakeAPI { return &fakeAPI{store: map[string]map[string]any{}} }

func (f *fakeAPI) Get(_ context.Context, path string) ([]byte, error) {
	o, ok := f.store[path]
	if !ok {
		return nil, &kube.StatusError{Code: http.StatusNotFound}
	}
	return json.Marshal(o)
}

func (f *fakeAPI) MergePatch(_ context.Context, path string, body []byte) ([]byte, error) {
	o, ok := f.store[path]
	if !ok {
		return nil, &kube.StatusError{Code: http.StatusNotFound}
	}
	var patch struct {
		Spec map[string]any `json:"spec"`
	}
	_ = json.Unmarshal(body, &patch)
	sp := o["spec"].(map[string]any)
	for k, v := range patch.Spec {
		sp[k] = v
	}
	return body, nil
}

func (f *fakeAPI) Do(_ context.Context, method, path, _ string, body []byte) ([]byte, error) {
	var o map[string]any
	_ = json.Unmarshal(body, &o)
	name := o["metadata"].(map[string]any)["name"].(string)
	full := path + "/" + name
	if _, dup := f.store[full]; dup {
		return nil, &kube.StatusError{Code: http.StatusConflict}
	}
	f.posts++
	f.store[full] = o
	return body, nil
}

func (f *fakeAPI) spec(t *testing.T, path string) map[string]any {
	o, ok := f.store[path]
	if !ok {
		t.Fatalf("no object %s in %v", path, keys(f.store))
	}
	return o["spec"].(map[string]any)
}

func keys(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

const recPath = "/apis/gryvia.io/v1alpha1/namespaces/tenant-alpha/gryvianetworkusagerecords/netusage-n1-2026031010-external-internet"

func newPub(api API, m *Meter, now time.Time) *Publisher {
	return &Publisher{API: api, Meter: m, Node: "n1", Now: func() time.Time { return now }}
}

func TestPublishCreatesThenUpdatesOneRecordPerBucket(t *testing.T) {
	api := newAPI()
	m := NewMeter(stdDir(t), "n1")
	p := newPub(api, m, t0)
	m.Observe([]Sample{{Local: addr("10.0.1.1"), Remote: addr("8.8.8.8"), BytesSent: 1000, BytesRecv: 50}}, t0)
	if n := p.PublishOnce(context.Background()); n != 1 || api.posts != 1 {
		t.Fatalf("n=%d posts=%d", n, api.posts)
	}
	sp := api.spec(t, recPath)
	if sp["egressBytes"] != float64(1000) || sp["ingressBytes"] != float64(50) || sp["final"] != false ||
		sp["source"] != "collector" || sp["tenant"] != "alpha" || sp["hour"] != "2026-03-10T10:00:00Z" {
		t.Fatalf("%v", sp)
	}
	// publishing again without new bytes changes nothing; new bytes overwrite with the running total
	p.PublishOnce(context.Background())
	m.Observe([]Sample{{Local: addr("10.0.1.1"), Remote: addr("8.8.8.8"), BytesSent: 1500, BytesRecv: 50}}, t0.Add(time.Minute))
	p.PublishOnce(context.Background())
	if api.posts != 1 || api.spec(t, recPath)["egressBytes"] != float64(1500) {
		t.Fatalf("posts=%d %v", api.posts, api.spec(t, recPath))
	}
}

func TestPublishRestartAddsToStoredBaseAndNeverLowers(t *testing.T) {
	api := newAPI()
	m := NewMeter(stdDir(t), "n1")
	m.Observe([]Sample{{Local: addr("10.0.1.1"), Remote: addr("8.8.8.8"), BytesSent: 1000}}, t0)
	newPub(api, m, t0).PublishOnce(context.Background())

	// the collector restarts: fresh meter (counters restart at 0), fresh publisher, same store
	m2 := NewMeter(stdDir(t), "n1")
	m2.Observe([]Sample{{Local: addr("10.0.1.1"), Remote: addr("8.8.8.8"), BytesSent: 200}}, t0.Add(5*time.Minute))
	p2 := newPub(api, m2, t0.Add(5*time.Minute))
	p2.PublishOnce(context.Background())
	p2.PublishOnce(context.Background()) // idempotent within one run
	if got := api.spec(t, recPath)["egressBytes"]; got != float64(1200) || api.posts != 1 {
		t.Fatalf("stored 1000 + this run 200 = 1200, got %v (posts %d)", got, api.posts)
	}
}

func TestPublishFinalClosesEvictsAndIsNeverModified(t *testing.T) {
	api := newAPI()
	m := NewMeter(stdDir(t), "n1")
	m.Observe([]Sample{{Local: addr("10.0.1.1"), Remote: addr("8.8.8.8"), BytesSent: 1000}}, t0)
	end := t0.Truncate(time.Hour).Add(time.Hour)
	p := newPub(api, m, end.Add(time.Minute)) // inside the grace period: still open
	p.PublishOnce(context.Background())
	if api.spec(t, recPath)["final"] != false {
		t.Fatal("must stay open during the grace period")
	}
	p.Now = func() time.Time { return end.Add(FinalGrace + time.Second) }
	p.PublishOnce(context.Background())
	if api.spec(t, recPath)["final"] != true || len(m.Snapshot()) != 0 {
		t.Fatal("hour must be closed and evicted from memory")
	}
	// a restarted collector holding new bytes for the closed hour must not touch the final record
	m2 := NewMeter(stdDir(t), "n1")
	m2.Observe([]Sample{{Local: addr("10.0.1.1"), Remote: addr("8.8.8.8"), BytesSent: 999}}, t0)
	newPub(api, m2, t0).PublishOnce(context.Background())
	if api.spec(t, recPath)["egressBytes"] != float64(1000) {
		t.Fatal("a final record is never modified")
	}
}

func TestPublishSkipsUnattributedAndBadTenantNames(t *testing.T) {
	api := newAPI()
	m := NewMeter(stdDir(t), "n1")
	m.Observe([]Sample{{Local: addr("10.0.3.9"), Remote: addr("8.8.8.8"), BytesSent: 1}}, t0) // kube-system
	m.buckets[Key{Hour: t0.Truncate(time.Hour), Tenant: "../evil", Peer: PeerExternal, Zone: ZoneInternet}] = &Totals{Egress: 5}
	if n := newPub(api, m, t0).PublishOnce(context.Background()); n != 0 || len(api.store) != 0 {
		t.Fatal("nothing may be written")
	}
}

func TestRecordNameIsBounded(t *testing.T) {
	n := recordName(strings.Repeat("x", 200), Key{Hour: t0, Peer: PeerExternal, Zone: ZoneInternet})
	if len(n) > 100 || !dns1123.MatchString(strings.Split(n, "-")[1]) {
		t.Fatal(n)
	}
}
