// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Target is one metrics endpoint to scrape.
type Target struct {
	Name      string // label for logs and /api/v1/inference; unique per scraper
	URL       string
	Engine    string // "" = auto-detect from the exposition
	Namespace string // attribution; "" = "_unattributed"
	Job       string
	Pod       string // discovered targets only
}

var (
	labelName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,62})$`)
	dnsName   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

// MaxTargets bounds static plus discovered targets.
const MaxTargets = 64

// ParseTargets parses a -infer-metrics value: targets separated by ";" (or newlines), each a
// comma-separated list of key=value with keys name, url (required), engine, namespace, job:
//
//	name=vllm,url=http://127.0.0.1:8000/metrics
//	name=triton,url=http://10.0.0.5:8002/metrics,engine=triton,namespace=ml,job=serve
//
// Only http and https URLs without credentials are accepted. namespace and job attribute the
// figures to a GryviaFabricSignal (jobRef = job); without them the target reports only to the
// collector's own /metrics and /api/v1/inference.
func ParseTargets(spec string) ([]Target, error) {
	var out []Target
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(spec, func(r rune) bool { return r == ';' || r == '\n' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var t Target
		for _, kv := range strings.Split(part, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if !ok {
				return nil, fmt.Errorf("infer-metrics target %q: %q is not key=value", part, kv)
			}
			switch strings.TrimSpace(k) {
			case "name":
				t.Name = strings.TrimSpace(v)
			case "url":
				t.URL = strings.TrimSpace(v)
			case "engine":
				t.Engine = strings.ToLower(strings.TrimSpace(v))
			case "namespace":
				t.Namespace = strings.TrimSpace(v)
			case "job":
				t.Job = strings.TrimSpace(v)
			default:
				return nil, fmt.Errorf("infer-metrics target %q: unknown key %q", part, k)
			}
		}
		if err := t.validate(); err != nil {
			return nil, fmt.Errorf("infer-metrics target %q: %w", part, err)
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("infer-metrics: duplicate target name %q", t.Name)
		}
		seen[t.Name] = true
		out = append(out, t)
	}
	if len(out) > MaxTargets {
		return nil, fmt.Errorf("infer-metrics: at most %d targets", MaxTargets)
	}
	return out, nil
}

func (t Target) validate() error {
	if !labelName.MatchString(t.Name) {
		return fmt.Errorf("name must match %s", labelName)
	}
	u, err := url.Parse(t.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("url must be http(s)://host[:port]/path without credentials or fragment")
	}
	switch t.Engine {
	case "", EngineVLLM, EngineTriton, EngineTGI:
	default:
		return fmt.Errorf("engine must be vllm, triton or tgi")
	}
	if (t.Namespace == "") != (t.Job == "") {
		return fmt.Errorf("namespace and job go together")
	}
	if t.Namespace != "" && (!dnsName.MatchString(t.Namespace) || !dnsName.MatchString(t.Job)) {
		return fmt.Errorf("namespace and job must be DNS-1123 names")
	}
	return nil
}

// ParsePorts parses a comma-separated TCP port list (at most 16).
func ParsePorts(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		p, err := strconv.Atoi(f)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("invalid port %q", f)
		}
		out = append(out, p)
	}
	if len(out) > 16 {
		return nil, fmt.Errorf("at most 16 ports")
	}
	return out, nil
}

// PodAPI is the part of the in-cluster client discovery needs.
type PodAPI interface {
	Get(ctx context.Context, path string) ([]byte, error)
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			NodeName    string `json:"nodeName"`
			HostNetwork bool   `json:"hostNetwork"`
			Containers  []struct {
				Ports []struct {
					ContainerPort int    `json:"containerPort"`
					Protocol      string `json:"protocol"`
				} `json:"ports"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"phase"`
			PodIP string `json:"podIP"`
		} `json:"status"`
	} `json:"items"`
}

// Discover lists the Running pods of this node that carry a gryvia.io/job label and declare a
// containerPort from ports, and returns one http://<podIP>:<port>/metrics target per match.
// Pods on the host network are skipped (their IP is the node's, shared by every such pod), and so
// are pods without the job label: an unlabelled pod cannot be attributed to a job.
func Discover(ctx context.Context, api PodAPI, node string, ports []int) ([]Target, error) {
	if node == "" {
		return nil, fmt.Errorf("NODE_NAME is not set")
	}
	q := url.Values{"fieldSelector": {"spec.nodeName=" + node}}
	raw, err := api.Get(ctx, "/api/v1/pods?"+q.Encode())
	if err != nil {
		return nil, err
	}
	return discoverFrom(raw, node, ports)
}

func discoverFrom(raw []byte, node string, ports []int) ([]Target, error) {
	var l podList
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	want := map[int]bool{}
	for _, p := range ports {
		want[p] = true
	}
	var out []Target
	for _, p := range l.Items {
		job := p.Metadata.Labels["gryvia.io/job"]
		ip := net.ParseIP(p.Status.PodIP)
		if p.Spec.NodeName != node || p.Spec.HostNetwork || p.Status.Phase != "Running" || job == "" || ip == nil ||
			!dnsName.MatchString(p.Metadata.Namespace) || !dnsName.MatchString(job) || !labelName.MatchString(p.Metadata.Name) {
			continue
		}
		hostport := func(port int) string { return net.JoinHostPort(ip.String(), strconv.Itoa(port)) }
		got := map[int]bool{}
		for _, c := range p.Spec.Containers {
			for _, cp := range c.Ports {
				if (cp.Protocol == "" || cp.Protocol == "TCP") && want[cp.ContainerPort] {
					got[cp.ContainerPort] = true
				}
			}
		}
		for port := range got {
			out = append(out, Target{
				Name: p.Metadata.Name + ":" + strconv.Itoa(port), URL: "http://" + hostport(port) + "/metrics",
				Namespace: p.Metadata.Namespace, Job: job, Pod: p.Metadata.Name,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > MaxTargets {
		out = out[:MaxTargets]
	}
	return out, nil
}
