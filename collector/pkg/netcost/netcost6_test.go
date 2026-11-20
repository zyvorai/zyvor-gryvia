// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package netcost

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The IPv6 cluster: nodes carry a v6 InternalIP as well; pods are dual-stack (podIP = v4, podIPs = v4 + v6) or
// v6-only. All JSON is crafted; nothing here ran against a real API server.
const nodes6JSON = `{"items":[
 {"metadata":{"name":"n1","labels":{"topology.kubernetes.io/zone":"a"}},"status":{"addresses":[{"type":"InternalIP","address":"192.168.0.1"},{"type":"InternalIP","address":"fd00:0:0:1::1"}]}},
 {"metadata":{"name":"n2","labels":{"topology.kubernetes.io/zone":"b"}},"status":{"addresses":[{"type":"InternalIP","address":"192.168.0.2"},{"type":"InternalIP","address":"fd00:0:0:1::2"}]}}]}`

func pod6(ns, name, node, extra string, ips ...string) string {
	var l []string
	for _, ip := range ips {
		l = append(l, fmt.Sprintf(`{"ip":%q}`, ip))
	}
	return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":%q},"spec":{"nodeName":%q%s},"status":{"podIP":%q,"podIPs":[%s],"phase":"Running"}}`,
		name, ns, node, extra, ips[0], strings.Join(l, ","))
}

func dir6(t *testing.T, pods ...string) *Directory {
	t.Helper()
	d := NewDirectory()
	if err := d.Set([]byte(`{"items":[`+strings.Join(pods, ",")+`]}`), []byte(nodes6JSON), []byte(nsJSON)); err != nil {
		t.Fatal(err)
	}
	return d
}

func stdDir6(t *testing.T) *Directory {
	return dir6(t,
		pod6("tenant-alpha", "a1", "n1", "", "10.0.1.1", "fd00:10::1"),          // dual-stack
		pod6("tenant-alpha", "a2", "n1", "", "fd00:10::2"),                      // v6-only, same node
		pod6("tenant-alpha", "a3", "n2", "", "10.0.2.1", "FD00:10:0:0:0:0:2:1"), // other zone (non-canonical text)
		pod6("tenant-beta", "b1", "n1", "", "fd00:10::9"),                       // other tenant, same node
		pod6("tenant-beta", "b2", "n2", "", "fd00:10:0:0:0:0:2:9"),              // other tenant, other zone
		pod6("kube-system", "dns", "n2", "", "fd00:10::53"),
		pod6("tenant-alpha", "hn", "n1", `,"hostNetwork":true`, "fd00:0:0:1::1"),
		pod6("tenant-alpha", "dupA", "n1", "", "fd00:10::66"),
		pod6("tenant-beta", "dupB", "n1", "", "fd00:10::66"), // same v6 address on two pods
	)
}

func TestClassifyIPv6Matrix(t *testing.T) {
	d := stdDir6(t)
	cases := []struct {
		name          string
		local, remote string
		ok            bool
		why           string
		peer          PeerClass
		zone          ZoneClass
		tenant        string
	}{
		{"same tenant cross zone", "fd00:10::1", "fd00:10:0:0:0:0:2:1", true, "", PeerSameTenant, ZoneCross, "alpha"},
		{"v6-only pod to other tenant, other zone", "fd00:10::2", "fd00:10::2:9", true, "", PeerOtherTenant, ZoneCross, "alpha"},
		{"other tenant same node kept", "fd00:10::1", "fd00:10::9", true, "", PeerOtherTenant, ZoneSameNode, "alpha"},
		{"same tenant same node excluded", "fd00:10::1", "fd00:10::2", false, SkipSameNode, "", "", ""},
		{"kube-system pod", "fd00:10::1", "fd00:10::53", true, "", PeerCluster, ZoneCross, "alpha"},
		{"node ip peer", "fd00:10::1", "fd00:0:0:1::2", true, "", PeerCluster, ZoneCross, "alpha"},
		{"global unicast is external", "fd00:10::1", "2001:4860:4860::8888", true, "", PeerExternal, ZoneInternet, "alpha"},
		{"unknown ULA is not internet", "fd00:10::1", "fd12:3456::1", true, "", PeerUnknown, ZoneUnknown, "alpha"},
		{"link-local peer is not internet", "fd00:10::1", "fe80::1", true, "", PeerUnknown, ZoneUnknown, "alpha"},
		{"multicast peer is not internet", "fd00:10::1", "ff02::1", true, "", PeerUnknown, ZoneUnknown, "alpha"},
		{"loopback", "::1", "::1", false, SkipLoopback, "", "", ""},
		{"local not on this node", "fd00:10::2:1", "2001:4860:4860::8888", false, SkipUnattributed, "", "", ""},
		{"local unknown ip (SNAT/NPTv6)", "fd00:99::1", "2001:4860:4860::8888", false, SkipUnattributed, "", "", ""},
		{"local is a node ip", "fd00:0:0:1::1", "2001:4860:4860::8888", false, SkipUnattributed, "", "", ""},
		{"hostNetwork pod resolves to node", "fd00:0:0:1::1", "fd00:10::9", false, SkipUnattributed, "", "", ""},
		{"local non-tenant pod", "fd00:10::53", "2001:4860:4860::8888", false, SkipUnattributed, "", "", ""},
		{"ambiguous local ip", "fd00:10::66", "2001:4860:4860::8888", false, SkipUnattributed, "", "", ""},
		{"ipv4-mapped is invalid", "::ffff:10.0.1.1", "::ffff:8.8.8.8", false, SkipInvalid, "", "", ""},
		{"zoned address is invalid", "fe80::1%eth0", "fe80::2", false, SkipInvalid, "", "", ""},
		{"garbage", "not-an-ip", "fd00:10::1", false, SkipInvalid, "", "", ""},
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
	// The dual-stack pod is one pod: both its addresses resolve, and neither is ambiguous with itself.
	for _, ip := range []string{"10.0.1.1", "fd00:10::1", "FD00:10:0:0:0:0:0:1"} {
		if e, ok := d.Lookup(ip); !ok || e.Kind != "pod" || e.Tenant != "alpha" {
			t.Errorf("%s: %+v %v", ip, e, ok)
		}
	}
	// Stale cache fails closed for v6 too.
	now := time.Now()
	d.now = func() time.Time { return now.Add(3 * time.Minute) }
	if _, ok, why := Classify(d, "n1", "fd00:10::1", "2001:4860:4860::8888"); ok || why != SkipUnattributed {
		t.Errorf("stale cache: ok=%v why=%q", ok, why)
	}
}

func TestMeterIPv6AndMixedScan(t *testing.T) {
	m := NewMeter(stdDir6(t), "n1")
	m.Observe([]Sample{
		{Local: addr("10.0.1.1"), Remote: addr("8.8.8.8"), BytesSent: 100, BytesRecv: 7},
		{Local: addr("fd00:10::1"), Remote: addr("2001:4860:4860::8888"), BytesSent: 4000, BytesRecv: 500},
		{Local: addr("fd00:10::1"), Remote: addr("fd00:10:0:0:0:0:2:1"), BytesSent: 30},
		{Local: addr("fd00:10::2"), Remote: addr("fd00:10::1"), BytesSent: 60},           // same tenant, same node: excluded
		{Local: addr("fd00:10::66"), Remote: addr("2001:4860:4860::8888"), BytesSent: 9}, // ambiguous
		{Local: addr("::1"), Remote: addr("::1"), BytesSent: 5},
	}, t0)
	if b := find(m.Snapshot(), "alpha", PeerExternal, ZoneInternet); b.Egress != 4100 || b.Ingress != 507 {
		t.Errorf("v4 and v6 internet egress add up: %+v", b)
	}
	if b := find(m.Snapshot(), "alpha", PeerSameTenant, ZoneCross); b.Egress != 30 {
		t.Errorf("%+v", b)
	}
	sk := m.Stats().Skipped
	if sk[SkipSameNode] != 60 || sk[SkipUnattributed] != 9 || sk[SkipLoopback] != 5 {
		t.Errorf("skipped: %v", sk)
	}
	// Idempotent: the same cumulative v6 sample adds nothing; growth adds only the delta; a reset adds the new value.
	s := Sample{Local: addr("fd00:10::1"), Remote: addr("2001:4860:4860::8888"), BytesSent: 4000, BytesRecv: 500}
	m.Observe([]Sample{s}, t0.Add(15*time.Second))
	s.BytesSent = 4500
	m.Observe([]Sample{s}, t0.Add(30*time.Second))
	s.BytesSent = 20 // counter reset (LRU eviction / reload)
	m.Observe([]Sample{s}, t0.Add(45*time.Second))
	if b := find(m.Snapshot(), "alpha", PeerExternal, ZoneInternet); b.Egress != 4100+500+20 {
		t.Errorf("egress %+v", b)
	}
}

// The same pair is different for v4 and v6 (distinct pairKeys) and the pair bound covers both families.
func TestMeterPairBoundCoversBothFamilies(t *testing.T) {
	m := NewMeter(stdDir6(t), "n1")
	if MaxPairs <= 0 {
		t.Fatal()
	}
	a4, a6 := Sample{Local: addr("10.0.1.1"), Remote: addr("8.8.8.8"), BytesSent: 1}, Sample{Local: addr("fd00:10::1"), Remote: addr("2001:4860:4860::8888"), BytesSent: 2}
	m.Observe([]Sample{a4, a6, a4, a6}, t0)
	if b := find(m.Snapshot(), "alpha", PeerExternal, ZoneInternet); b.Egress != 3 {
		t.Errorf("%+v", b)
	}
}
