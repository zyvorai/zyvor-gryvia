// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package netcost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/kube"
)

const (
	// PublishInterval is how often open hours are written.
	PublishInterval = 60 * time.Second
	// FinalGrace is how long after the end of an hour its records are closed, so the last scan lands in it.
	FinalGrace = 2 * time.Minute

	groupVersion = "gryvia.io/v1alpha1"
	plural       = "gryvianetworkusagerecords"
)

// API is the part of *kube.Client the publisher uses.
type API interface {
	Get(ctx context.Context, path string) ([]byte, error)
	MergePatch(ctx context.Context, path string, body []byte) ([]byte, error)
	Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error)
}

// Logger is the subset of zap's SugaredLogger used here.
type Logger interface {
	Warnw(msg string, kv ...any)
}

type nopLog struct{}

func (nopLog) Warnw(string, ...any) {}

var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Publisher writes the Meter's buckets as GryviaNetworkUsageRecord objects into tenant-<tenant> namespaces:
// one record per (node, tenant, UTC hour, peerClass, zoneClass).
//
// Restart safety: a bucket lives in memory only. The first time this process publishes a bucket it reads the
// record a previous run may have written and adds its own bytes on top of that stored base (never overwrites
// with a smaller number); a record already final is never touched. Bytes that were counted in memory but not
// yet written when a collector dies, and bytes during downtime, are lost: the record can undercount, never
// double count. The first sight of a pair after a restart counts its whole kernel counter, which is only the
// bytes since this process loaded the program.
type Publisher struct {
	API   API
	Meter *Meter
	Node  string
	Log   Logger
	Now   func() time.Time

	// base is what the store held for a bucket when this process first touched it.
	base map[Key]Totals
	// skipped buckets whose stored record was already final.
	closed map[Key]bool
}

func recordName(node string, k Key) string {
	n := strings.ToLower(node)
	if !dns1123.MatchString(n) || len(n) > 100 {
		h := fnv.New32a()
		_, _ = h.Write([]byte(node))
		n = fmt.Sprintf("n%08x", h.Sum32())
	}
	return fmt.Sprintf("netusage-%s-%s-%s-%s", n, k.Hour.UTC().Format("2006010215"), k.Peer, k.Zone)
}

type recordSpec struct {
	Tenant       string `json:"tenant"`
	Node         string `json:"node"`
	Source       string `json:"source"`
	Hour         string `json:"hour"`
	PeerClass    string `json:"peerClass"`
	ZoneClass    string `json:"zoneClass"`
	EgressBytes  uint64 `json:"egressBytes"`
	IngressBytes uint64 `json:"ingressBytes"`
	Final        bool   `json:"final"`
}

func (p *Publisher) spec(k Key, t Totals, final bool) recordSpec {
	return recordSpec{Tenant: k.Tenant, Node: p.Node, Source: "collector", Hour: k.Hour.UTC().Format(time.RFC3339),
		PeerClass: string(k.Peer), ZoneClass: string(k.Zone), EgressBytes: t.Egress, IngressBytes: t.Ingress, Final: final}
}

func nsOf(tenant string) string { return "tenant-" + tenant }

// PublishOnce writes every open bucket once, closing (final=true) and evicting the hours that ended more than
// FinalGrace ago. It returns the number of records written. Errors are logged and never propagate.
func (p *Publisher) PublishOnce(ctx context.Context) int {
	if p == nil || p.API == nil || p.Meter == nil {
		return 0
	}
	if p.Log == nil {
		p.Log = nopLog{}
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.base == nil {
		p.base, p.closed = map[Key]Totals{}, map[Key]bool{}
	}
	now := p.Now().UTC()
	written := 0
	for _, b := range p.Meter.Snapshot() {
		if ctx.Err() != nil {
			return written
		}
		if !dns1123.MatchString(b.Tenant) {
			continue // never put an unchecked name in a request path
		}
		final := now.After(b.Hour.Add(time.Hour + FinalGrace))
		ok, err := p.write(ctx, b, final)
		if err != nil {
			p.Log.Warnw("network usage: write failed", "tenant", b.Tenant, "hour", b.Hour, "error", err)
			continue
		}
		if ok {
			written++
		}
		if final && err == nil {
			p.Meter.Evict(b.Key)
			delete(p.base, b.Key)
			delete(p.closed, b.Key)
		}
	}
	return written
}

func (p *Publisher) write(ctx context.Context, b Bucket, final bool) (bool, error) {
	if p.closed[b.Key] {
		return false, nil
	}
	ns := nsOf(b.Tenant)
	base := "/apis/" + groupVersion + "/namespaces/" + url.PathEscape(ns) + "/" + plural
	name := recordName(p.Node, b.Key)
	path := base + "/" + name

	stored, loaded := p.base[b.Key]
	exists := false
	if !loaded {
		raw, err := p.API.Get(ctx, path)
		var se *kube.StatusError
		switch {
		case err == nil:
			var cur struct {
				Spec recordSpec `json:"spec"`
			}
			if err := json.Unmarshal(raw, &cur); err != nil {
				return false, err
			}
			if cur.Spec.Final {
				p.closed[b.Key] = true
				return false, nil
			}
			stored = Totals{Egress: cur.Spec.EgressBytes, Ingress: cur.Spec.IngressBytes}
			exists = true
		case errors.As(err, &se) && se.Code == http.StatusNotFound:
		default:
			return false, err
		}
		p.base[b.Key] = stored
	} else {
		exists = true
	}
	total := Totals{Egress: stored.Egress + b.Egress, Ingress: stored.Ingress + b.Ingress}
	spec := p.spec(b.Key, total, final)
	if exists {
		body, _ := json.Marshal(map[string]any{"spec": spec})
		_, err := p.API.MergePatch(ctx, path, body)
		return err == nil, err
	}
	body, _ := json.Marshal(map[string]any{
		"apiVersion": groupVersion, "kind": "GryviaNetworkUsageRecord",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]string{
			"gryvia.io/tenant": b.Tenant, "app.kubernetes.io/managed-by": "gryvia-collector"}},
		"spec": spec})
	_, err := p.API.Do(ctx, http.MethodPost, base, "application/json", body)
	var se *kube.StatusError
	if errors.As(err, &se) && se.Code == http.StatusConflict {
		// another writer created it between our GET and POST: re-read on the next cycle
		delete(p.base, b.Key)
		return false, nil
	}
	return err == nil, err
}

// Run publishes every PublishInterval until ctx is done.
func (p *Publisher) Run(ctx context.Context) {
	t := time.NewTicker(PublishInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.PublishOnce(ctx)
		}
	}
}
