// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package netcost attributes measured network bytes to tenants and prices-ready buckets.
//
// Byte source: the traffic_costs map of the cost_tracker tcx program (cumulative per-(local IP, remote IP)
// bytes_sent / bytes_recv, egress keyed by source, ingress keyed by destination). It is the only source of
// bytes. flow_event.Bytes of tcp_trace is NOT used: nothing in the kernel program increments the counters it
// reads at close, so it is always 0 today, and adding it would double count anyway.
//
// Everything here is userspace, pure and fail closed: a local address that is not exactly one non-hostNetwork
// pod of this node in a tenant namespace is never billed. Unverified on a real cluster: the pieces are tested
// with fakes (crafted pod/node JSON, canned counter samples).
package netcost

import (
	"net/netip"
	"strings"
)

// PeerClass is the relation of the other end to the tenant.
type PeerClass string

const (
	PeerSameTenant  PeerClass = "same-tenant"
	PeerOtherTenant PeerClass = "other-tenant"
	PeerCluster     PeerClass = "cluster" // in-cluster, not a tenant namespace (kube-system, nodes, ...)
	PeerExternal    PeerClass = "external"
	PeerUnknown     PeerClass = "unknown" // private address that is neither a pod nor a node
)

// ZoneClass is the zone relation between the tenant pod and its peer.
type ZoneClass string

const (
	ZoneSame     ZoneClass = "same-zone"
	ZoneCross    ZoneClass = "cross-zone"
	ZoneSameNode ZoneClass = "same-node"
	ZoneUnknown  ZoneClass = "unknown-zone"
	ZoneInternet ZoneClass = "internet"
)

// Endpoint is what the directory knows about an IP address.
type Endpoint struct {
	Kind      string // "pod" or "node"
	Namespace string
	Tenant    string // "" outside tenant namespaces
	Node      string
	Zone      string // "" when the node has no topology.kubernetes.io/zone label
}

// Resolver looks an address up. ok is false for unknown or ambiguous addresses (fail closed).
type Resolver interface {
	Lookup(ip string) (Endpoint, bool)
}

// Verdict is the classification of one (local, remote) pair.
type Verdict struct {
	Tenant string
	Peer   PeerClass
	Zone   ZoneClass
}

// Skip reasons (returned when Classify reports ok == false).
const (
	SkipUnattributed = "unattributed" // local is not a known single pod of a tenant on this node
	SkipLoopback     = "loopback"
	SkipSameNode     = "same-node"
	SkipInvalid      = "invalid"
)

// Classify decides how bytes between local (a pod address seen by this node's collector) and remote are
// accounted. It returns ok=false with a reason when they must not be billed at all.
//
// Rules: loopback is excluded; a local address that is not a tenant pod scheduled on this node is
// unattributed; pod-to-pod on the same node is excluded unless the peer belongs to another tenant (then it is
// kept with zone class same-node, so a provider may still meter tenant-to-tenant traffic).
func Classify(r Resolver, node, local, remote string) (Verdict, bool, string) {
	lip, err1 := netip.ParseAddr(local)
	rip, err2 := netip.ParseAddr(remote)
	// Both ends must be valid, plain (no zone, not IPv4-mapped IPv6) and of the same family.
	if err1 != nil || err2 != nil || lip.Zone() != "" || rip.Zone() != "" ||
		lip.Is4In6() || rip.Is4In6() || lip.Is4() != rip.Is4() {
		return Verdict{}, false, SkipInvalid
	}
	if lip.IsLoopback() || rip.IsLoopback() {
		return Verdict{}, false, SkipLoopback
	}
	src, ok := r.Lookup(local)
	if !ok || src.Kind != "pod" || src.Tenant == "" || src.Node != node || node == "" {
		return Verdict{}, false, SkipUnattributed
	}
	v := Verdict{Tenant: src.Tenant}
	dst, known := r.Lookup(remote)
	switch {
	case known && dst.Kind == "pod" && dst.Tenant != "":
		if dst.Tenant == src.Tenant {
			v.Peer = PeerSameTenant
		} else {
			v.Peer = PeerOtherTenant
		}
	case known:
		v.Peer = PeerCluster
	case isPrivate(rip):
		v.Peer = PeerUnknown
	default:
		v.Peer = PeerExternal
	}
	if v.Peer == PeerExternal {
		v.Zone = ZoneInternet
		return v, true, ""
	}
	if !known {
		v.Zone = ZoneUnknown
		return v, true, ""
	}
	if dst.Kind == "pod" && dst.Node == src.Node {
		if v.Peer != PeerOtherTenant {
			return Verdict{}, false, SkipSameNode
		}
		v.Zone = ZoneSameNode
		return v, true, ""
	}
	switch {
	case src.Zone == "" || dst.Zone == "":
		v.Zone = ZoneUnknown
	case src.Zone == dst.Zone:
		v.Zone = ZoneSame
	default:
		v.Zone = ZoneCross
	}
	return v, true, ""
}

// isPrivate is true for RFC 1918, CGNAT, link-local and other non-public unicast IPv4 ranges: an address
// there that is neither a pod nor a node may be another VPC or a service VIP, so it is never called "internet".
//
// IPv6: fc00::/7 (ULA), fe80::/10 (link-local), multicast and the unspecified address are private; every
// other address is treated as global unicast (external) unless it is a known pod or node.
func isPrivate(ip netip.Addr) bool {
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if ip.Is4() {
		v4 := ip.As4()
		return v4[0] == 100 && v4[1]&0xC0 == 64 // 100.64.0.0/10
	}
	return false
}

// tenantOf maps a namespace to its tenant: the gryvia.io/tenant label (set by the tenant controller) wins,
// the tenant-<name> naming convention is the fallback. Anything else is not a tenant namespace.
func tenantOf(labels map[string]string, ns string) string {
	if t := labels["gryvia.io/tenant"]; t != "" {
		return t
	}
	if strings.HasPrefix(ns, "tenant-") && len(ns) > len("tenant-") {
		return strings.TrimPrefix(ns, "tenant-")
	}
	return ""
}
