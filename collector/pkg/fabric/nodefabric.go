// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fabric

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/kube"
)

// Per-node fabric health for fabric-aware scheduling (opt-in, -publish-node-fabric).
//
// The collector writes ONE cluster-scoped GryviaNodeFabric named after its node
// (NODE_NAME) every NodePublishInterval. The ai-operator reads it and ignores it
// once measuredAt + ttlSeconds has passed, so a dead or stopped collector stops
// influencing placement by itself. The value is the worst ScoreDelta of any
// signal group seen on this node in the fold window; signal quality depends on
// probes that are unverified on real GPU/RDMA hardware.

const (
	// NodePublishInterval is how often the node object is refreshed.
	NodePublishInterval = 30 * time.Second
	// NodeFabricTTL is the ttlSeconds written into the object: several missed
	// refreshes and a full fold window, then the reader ignores it.
	NodeFabricTTL = 5 * time.Minute
	// MaxNodeReasons bounds spec.reasons (also the CRD's maxItems).
	MaxNodeReasons = 8

	nodeFabricPath = "/apis/" + fabricSignalGroupVersion + "/gryvianodefabrics"
)

// Reason is one contribution to ScoreDelta.
type Reason struct {
	Text    string
	Penalty float64
}

// Reasons lists the terms that make ScoreDelta non-zero, mirroring ScoreDelta
// exactly (same thresholds and weights), most significant first.
func Reasons(st Status) []Reason {
	var r []Reason
	if st.NCCLP99MS > p99SlowMS {
		r = append(r, Reason{fmt.Sprintf("nccl straggler p99 %.0fms > %.0fms", st.NCCLP99MS, p99SlowMS), penaltyP99})
	}
	if st.RDMARetryRate > retryRateHigh {
		r = append(r, Reason{fmt.Sprintf("rdma retry rate %.1f%% > %.0f%%", st.RDMARetryRate*100, retryRateHigh*100), penaltyRDMA})
	}
	if st.GDSMeasured && st.GDSHitRatio < gdsHitLow {
		r = append(r, Reason{fmt.Sprintf("gds direct ratio %.0f%% < %.0f%%", st.GDSHitRatio*100, gdsHitLow*100), penaltyGDS})
	}
	if st.StragglerHits > stragglerHitsHi {
		r = append(r, Reason{fmt.Sprintf("%d straggler hits > %d", st.StragglerHits, stragglerHitsHi), penaltyStraggler})
	}
	if st.OverlapIdleRatio > overlapIdleHigh {
		r = append(r, Reason{fmt.Sprintf("gpu idle in collective %.0f%% > %.0f%%", st.OverlapIdleRatio*100, overlapIdleHigh*100), penaltyOverlap})
	}
	if st.CNPRate > cnpRateHigh {
		r = append(r, Reason{fmt.Sprintf("roce cnp %.0f/s > %d/s", st.CNPRate, cnpRateHigh), penaltyCNP})
	}
	if st.PFCRate > pfcRateHigh {
		r = append(r, Reason{fmt.Sprintf("pfc pause %.0f/s > %d/s", st.PFCRate, pfcRateHigh), penaltyPFC})
	}
	sort.SliceStable(r, func(i, j int) bool { return r[i].Penalty > r[j].Penalty })
	return r
}

// NodeHealth folds a snapshot into the node's health: the worst per-group
// ScoreDelta and the reasons behind it (prefixed with the group it came from).
// An empty snapshot is healthy (0, nil).
func NodeHealth(snap map[JobKey]Status) (float64, []string) {
	keys := make([]JobKey, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Namespace != keys[j].Namespace {
			return keys[i].Namespace < keys[j].Namespace
		}
		return keys[i].Job < keys[j].Job
	})
	worst := 0.0
	var worstKey JobKey
	for _, k := range keys {
		if d := finite(snap[k].ScoreDelta); d > worst {
			worst, worstKey = d, k
		}
	}
	worst = math.Max(0, math.Min(1, worst))
	if worst == 0 {
		return 0, nil
	}
	var out []string
	for _, r := range Reasons(snap[worstKey]) {
		if len(out) == MaxNodeReasons {
			break
		}
		out = append(out, fmt.Sprintf("%s [%s/%s]", r.Text, worstKey.Namespace, worstKey.Job))
	}
	return worst, out
}

// NodeAPI is what the node publisher needs from *kube.Client.
type NodeAPI interface {
	MergePatch(ctx context.Context, path string, body []byte) ([]byte, error)
	Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error)
}

// NodePublisher writes this node's GryviaNodeFabric. It only ever touches the
// object named after its own node, and every error is logged and dropped.
type NodePublisher struct {
	API    NodeAPI
	Folder *Folder
	Node   string
	Log    Logger
	Now    func() time.Time // nil = time.Now
}

type nodeFabricSpec struct {
	NodeName   string   `json:"nodeName"`
	ScoreDelta float64  `json:"scoreDelta"`
	Reasons    []string `json:"reasons"`
	MeasuredAt string   `json:"measuredAt"`
	TTLSeconds int32    `json:"ttlSeconds"`
	ExpiresAt  string   `json:"expiresAt"`
}

// NodeFabricBody renders the merge-patch body (and, with metadata, the create body).
func NodeFabricBody(node string, delta float64, reasons []string, now time.Time, create bool) ([]byte, error) {
	if reasons == nil {
		reasons = []string{} // sent every time so a cleared penalty overwrites old reasons
	}
	if len(reasons) > MaxNodeReasons {
		reasons = reasons[:MaxNodeReasons]
	}
	spec := nodeFabricSpec{
		NodeName:   node,
		ScoreDelta: math.Max(0, math.Min(1, finite(delta))),
		Reasons:    reasons,
		MeasuredAt: now.UTC().Format(time.RFC3339),
		TTLSeconds: int32(NodeFabricTTL / time.Second),
		ExpiresAt:  now.Add(NodeFabricTTL).UTC().Format(time.RFC3339),
	}
	obj := map[string]any{"spec": spec}
	if create {
		obj["apiVersion"] = fabricSignalGroupVersion
		obj["kind"] = "GryviaNodeFabric"
		obj["metadata"] = map[string]any{"name": node}
	}
	return json.Marshal(obj)
}

// PublishOnce refreshes the node object; it reports whether a write succeeded.
func (p *NodePublisher) PublishOnce(ctx context.Context) bool {
	if p == nil || p.API == nil || p.Folder == nil || !k8sName.MatchString(p.Node) {
		return false
	}
	log := p.Log
	if log == nil {
		log = nopLogger{}
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	t := now()
	delta, reasons := NodeHealth(p.Folder.Snapshot())
	path := nodeFabricPath + "/" + url.PathEscape(p.Node)
	body, err := NodeFabricBody(p.Node, delta, reasons, t, false)
	if err != nil {
		log.Warnw("node fabric: encode failed", "error", err)
		return false
	}
	_, err = p.API.MergePatch(ctx, path, body)
	var se *kube.StatusError
	if err != nil && errors.As(err, &se) && se.Code == http.StatusNotFound {
		var cb []byte
		if cb, err = NodeFabricBody(p.Node, delta, reasons, t, true); err == nil {
			_, err = p.API.Do(ctx, http.MethodPost, nodeFabricPath, "application/json", cb)
		}
	}
	if err != nil {
		log.Warnw("node fabric: write failed", "node", p.Node, "error", err)
		return false
	}
	return true
}

// Run publishes every NodePublishInterval until ctx is done.
func (p *NodePublisher) Run(ctx context.Context) {
	p.PublishOnce(ctx)
	t := time.NewTicker(NodePublishInterval)
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
