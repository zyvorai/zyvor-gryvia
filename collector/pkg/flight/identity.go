// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package flight

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/kube"
)

// The pod UID is a canonical UUID; systemd cgroup names use underscores for
// the dashes. Match exactly so trailing text is never absorbed into the UID.
var podUID = regexp.MustCompile(`pod([0-9a-fA-F]{8}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{12})`)

type podList struct {
	Items []struct {
		Metadata struct {
			UID       string            `json:"uid"`
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			NodeName    string `json:"nodeName"`
			HostNetwork bool   `json:"hostNetwork"`
		} `json:"spec"`
		Status struct {
			Phase    string `json:"phase"`
			QOSClass string `json:"qosClass"`
			PodIP    string `json:"podIP"`
		} `json:"status"`
	} `json:"items"`
}

// PodInfo is a running pod of a job on this node, with what is needed to find its cgroup.
type PodInfo struct {
	Identity
	UID string `json:"uid"`
	QOS string `json:"qos"` // Guaranteed, Burstable, BestEffort; empty when the API did not say
}

// Resolver caches the Kubernetes pod list. It never guesses an identity when
// the PID's cgroup is unavailable or the pod is absent from the current list.
type Resolver struct {
	mu      sync.RWMutex
	byUID   map[string]Identity
	byJob   map[string][]PodInfo
	byIP    map[string]Identity // pod IP -> identity (pods with a job label, not on the host network)
	proc    string
	node    string
	updated time.Time
}

func NewResolver(node, proc string) *Resolver {
	if proc == "" {
		proc = "/proc"
	}
	// Inside a pod, the container's own /proc uses a different PID namespace
	// than the PIDs the kernel reports, so falling back to it would attribute
	// events to the wrong process. Only fall back when not running in a
	// cluster (then /proc is the host's); otherwise Resolve fails closed.
	if _, err := os.Stat(proc); err != nil && os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
		proc = "/proc"
	}
	return &Resolver{byUID: map[string]Identity{}, byJob: map[string][]PodInfo{}, proc: proc, node: node}
}
func normalizeUID(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "-")) }

func (r *Resolver) SetPods(raw []byte) error {
	var list podList
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	next := make(map[string]Identity, len(list.Items))
	nextJobs := map[string][]PodInfo{}
	nextIP := make(map[string]Identity, len(list.Items))
	for _, p := range list.Items {
		if p.Spec.NodeName != r.node {
			continue
		}
		job := p.Metadata.Labels["gryvia.io/job"]
		if job == "" {
			continue
		}
		rank := p.Metadata.Labels["gryvia.io/rank"]
		if rank == "" {
			rank = p.Metadata.Labels["apps.kubernetes.io/pod-index"]
		}
		id := Identity{Namespace: p.Metadata.Namespace, Job: job, Pod: p.Metadata.Name, Node: r.node, Rank: rank}
		next[normalizeUID(p.Metadata.UID)] = id
		if p.Status.Phase == "Running" {
			k := key(id.Namespace, id.Job)
			nextJobs[k] = append(nextJobs[k], PodInfo{Identity: id, UID: normalizeUID(p.Metadata.UID), QOS: p.Status.QOSClass})
		}
		// Host-network pods share the node's address: an IP would not identify them.
		if ip := net.ParseIP(p.Status.PodIP); ip != nil && !p.Spec.HostNetwork {
			nextIP[ip.String()] = id
		}
	}
	r.mu.Lock()
	r.byUID = next
	r.byJob = nextJobs
	r.byIP = nextIP
	r.updated = time.Now()
	r.mu.Unlock()
	return nil
}

// Pods returns the Running pods of the job on this node from the last snapshot
// (nil when the snapshot is older than a minute: the list is then unknown, not empty).
func (r *Resolver) Pods(ns, job string) []PodInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if time.Since(r.updated) > time.Minute {
		return nil
	}
	return append([]PodInfo(nil), r.byJob[key(ns, job)]...)
}

// Jobs lists the (namespace, job) pairs that have a Running pod on this node.
func (r *Resolver) Jobs() [][2]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if time.Since(r.updated) > time.Minute {
		return nil
	}
	out := make([][2]string, 0, len(r.byJob))
	for _, pods := range r.byJob {
		if len(pods) > 0 {
			out = append(out, [2]string{pods[0].Namespace, pods[0].Job})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0]+"/"+out[i][1] < out[j][0]+"/"+out[j][1] })
	return out
}

func (r *Resolver) Resolve(pid uint32) (Identity, bool) {
	if pid == 0 {
		return Identity{}, false
	}
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/cgroup", r.proc, pid))
	if err != nil {
		return Identity{}, false
	}
	m := podUID.FindSubmatch(b)
	if m == nil {
		return Identity{}, false
	}
	r.mu.RLock()
	id, ok := r.byUID[normalizeUID(string(m[1]))]
	if time.Since(r.updated) > time.Minute {
		ok = false
	}
	r.mu.RUnlock()
	return id, ok
}

// ResolveIP identifies the job pod on this node that owns ip (dotted IPv4). It fails closed like
// Resolve: an unknown address or a pod list older than a minute yields false.
func (r *Resolver) ResolveIP(ip string) (Identity, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byIP[ip]
	if !ok || time.Since(r.updated) > time.Minute {
		return Identity{}, false
	}
	return id, true
}

// JobOfPod returns the gryvia.io/job of a pod on this node (false when the pod
// is unknown, has no job label, or the pod list is stale). It never guesses.
func (r *Resolver) JobOfPod(namespace, pod string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if time.Since(r.updated) > time.Minute {
		return "", false
	}
	for _, id := range r.byUID {
		if id.Namespace == namespace && id.Pod == pod {
			return id.Job, true
		}
	}
	return "", false
}

// Run refreshes the node's pods every 15 seconds. The API server TLS CA and
// service account token are used; failed refreshes retain the last snapshot.
func (r *Resolver) Run(ctx context.Context, onError func(error)) {
	client, err := kube.NewInCluster()
	if err != nil {
		onError(err)
		return
	}
	refresh := func() {
		q := url.Values{"fieldSelector": {"spec.nodeName=" + r.node}}
		// The client re-reads the projected service account token on every request.
		b, err := client.Get(ctx, "/api/v1/pods?"+q.Encode())
		if err == nil {
			err = r.SetPods(b)
		}
		if err != nil {
			onError(err)
		}
	}
	refresh()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}
