// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package netcost

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Directory is the address -> endpoint cache built from the API server (pods, nodes, namespaces). It is
// fail closed: an address held by more than one pod (IP reuse during churn), a hostNetwork pod (shares the
// node IP) and a cache older than MaxAge all resolve to "unknown".
type Directory struct {
	mu     sync.RWMutex
	byIP   map[string]Endpoint
	ambig  map[string]bool
	nodes  map[string]string // node name -> zone
	tenant map[string]string // namespace -> tenant
	// updated is when all three lists were last refreshed successfully.
	updated time.Time
	MaxAge  time.Duration
	now     func() time.Time
}

// NewDirectory returns an empty directory (which resolves nothing).
func NewDirectory() *Directory {
	return &Directory{byIP: map[string]Endpoint{}, ambig: map[string]bool{}, MaxAge: 2 * time.Minute, now: time.Now}
}

type metaOf struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels"`
}

// Set replaces the directory content from three raw API lists.
func (d *Directory) Set(podsRaw, nodesRaw, nsRaw []byte) error {
	var pods struct {
		Items []struct {
			Metadata metaOf `json:"metadata"`
			Spec     struct {
				NodeName    string `json:"nodeName"`
				HostNetwork bool   `json:"hostNetwork"`
			} `json:"spec"`
			Status struct {
				PodIP  string `json:"podIP"`
				PodIPs []struct {
					IP string `json:"ip"`
				} `json:"podIPs"`
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	var nodes struct {
		Items []struct {
			Metadata metaOf `json:"metadata"`
			Status   struct {
				Addresses []struct {
					Type    string `json:"type"`
					Address string `json:"address"`
				} `json:"addresses"`
			} `json:"status"`
		} `json:"items"`
	}
	var nss struct {
		Items []struct {
			Metadata metaOf `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(podsRaw, &pods); err != nil {
		return err
	}
	if err := json.Unmarshal(nodesRaw, &nodes); err != nil {
		return err
	}
	if err := json.Unmarshal(nsRaw, &nss); err != nil {
		return err
	}
	tenant := make(map[string]string, len(nss.Items))
	for _, n := range nss.Items {
		tenant[n.Metadata.Name] = tenantOf(n.Metadata.Labels, n.Metadata.Name)
	}
	zones := make(map[string]string, len(nodes.Items))
	byIP := map[string]Endpoint{}
	ambig := map[string]bool{}
	add := func(raw string, e Endpoint) {
		ip, ok := canonicalIP(raw)
		if !ok {
			return
		}
		if _, dup := byIP[ip]; dup {
			ambig[ip] = true
			return
		}
		byIP[ip] = e
	}
	for _, n := range nodes.Items {
		zones[n.Metadata.Name] = n.Metadata.Labels["topology.kubernetes.io/zone"]
	}
	// Node addresses first: a hostNetwork pod's IP is the node IP and must resolve to the node.
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			if a.Type == "InternalIP" || a.Type == "ExternalIP" {
				add(a.Address, Endpoint{Kind: "node", Node: n.Metadata.Name, Zone: zones[n.Metadata.Name]})
			}
		}
	}
	for _, p := range pods.Items {
		if p.Spec.HostNetwork || p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		e := Endpoint{Kind: "pod", Namespace: p.Metadata.Namespace, Node: p.Spec.NodeName,
			Tenant: tenant[p.Metadata.Namespace], Zone: zones[p.Spec.NodeName]}
		// podIP and podIPs (dual-stack: one IPv4 and one IPv6) name the same pod; an address is added
		// once per pod so a pod is never ambiguous with itself.
		own := map[string]bool{}
		for _, raw := range append([]string{p.Status.PodIP}, podIPList(p.Status.PodIPs)...) {
			if c, ok := canonicalIP(raw); ok && !own[c] {
				own[c] = true
				add(c, e)
			}
		}
	}
	d.mu.Lock()
	d.byIP, d.ambig, d.nodes, d.tenant, d.updated = byIP, ambig, zones, tenant, d.now()
	d.mu.Unlock()
	return nil
}

func podIPList(l []struct {
	IP string `json:"ip"`
}) []string {
	out := make([]string, 0, len(l))
	for _, x := range l {
		out = append(out, x.IP)
	}
	return out
}

// canonicalIP renders an address in the form the Meter looks it up with: dotted quad for IPv4, the compressed
// lower-case form for IPv6 (so "2001:DB8:0::1" and "2001:db8::1" are the same key). An IPv4-mapped IPv6 address
// becomes its IPv4 form; anything unparsable is refused.
func canonicalIP(s string) (string, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return "", false
	}
	return a.Unmap().String(), true
}

// Lookup implements Resolver. An IPv6 address is matched in canonical form.
func (d *Directory) Lookup(ip string) (Endpoint, bool) {
	if c, ok := canonicalIP(ip); ok {
		ip = c
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.updated.IsZero() || d.now().Sub(d.updated) > d.MaxAge || d.ambig[ip] {
		return Endpoint{}, false
	}
	e, ok := d.byIP[ip]
	return e, ok
}

// Getter is the read part of the kube client.
type Getter interface {
	Get(ctx context.Context, path string) ([]byte, error)
}

// listPage bounds one list response: kube.Client refuses bodies over 8 MiB, so large clusters are paged.
const listPage = 500

// fetchAll follows list continuation tokens and returns one merged {"items":[...]} document.
func fetchAll(ctx context.Context, g Getter, path string) ([]byte, error) {
	var items []json.RawMessage
	cont := ""
	for i := 0; i < 1000; i++ {
		q := url.Values{"limit": {strconv.Itoa(listPage)}}
		if cont != "" {
			q.Set("continue", cont)
		}
		raw, err := g.Get(ctx, path+"?"+q.Encode())
		if err != nil {
			return nil, err
		}
		var page struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		if page.Metadata.Continue == "" {
			return json.Marshal(map[string]any{"items": items})
		}
		cont = page.Metadata.Continue
	}
	return nil, errors.New("netcost: list did not terminate")
}

// Refresh lists pods, nodes and namespaces and replaces the directory. On any error the previous content is
// kept (and ages out through MaxAge).
func (d *Directory) Refresh(ctx context.Context, g Getter) error {
	pods, err := fetchAll(ctx, g, "/api/v1/pods")
	if err != nil {
		return err
	}
	nodes, err := fetchAll(ctx, g, "/api/v1/nodes")
	if err != nil {
		return err
	}
	nss, err := fetchAll(ctx, g, "/api/v1/namespaces")
	if err != nil {
		return err
	}
	return d.Set(pods, nodes, nss)
}

// Run refreshes every 15 s until ctx is done.
func (d *Directory) Run(ctx context.Context, g Getter, onError func(error)) {
	do := func() {
		if err := d.Refresh(ctx, g); err != nil && onError != nil {
			onError(err)
		}
	}
	do()
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			do()
		}
	}
}
