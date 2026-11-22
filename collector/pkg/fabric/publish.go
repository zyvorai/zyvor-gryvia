// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/kube"
)

// PublishInterval is how often fabric status is written to the cluster.
const PublishInterval = 30 * time.Second

// MaxPublishedJobs bounds the jobs handled per cycle (and so the API calls).
const MaxPublishedJobs = 256

const fabricSignalGroupVersion = "gryvia.io/v1alpha1"

// KubeAPI is the part of *kube.Client the publisher and the quota sync use.
type KubeAPI interface {
	Get(ctx context.Context, path string) ([]byte, error)
	MergePatch(ctx context.Context, path string, body []byte) ([]byte, error)
}

// k8sName is a conservative DNS-1123 subdomain check; anything else is never
// put into a request path.
var k8sName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// Publisher copies the folded fabric status of attributed jobs into the status
// subresource of an EXISTING GryviaFabricSignal (spec.jobRef == job name, same
// namespace). It creates nothing, never touches spec, and every error is logged
// and dropped: a failing API server cannot affect the collector.
//
// With several collectors (one per node) hosting pods of one job, the default mode merge-patches
// the top-level status with only the fields THIS node measured (never zeros or nulls for what it
// did not measure, except to clear a value this node itself sent earlier); the last writer of a
// field still wins. PerNode mode instead server-side-applies this node's own status.nodes[] entry
// under its own field manager, and the ai-operator (opt-in --merge-fabric-signals) folds the
// fresh entries into the top-level status. The status is advisory.
type Publisher struct {
	API    KubeAPI
	Folder *Folder
	Log    Logger
	// Inference adds the engine latency fields (-infer-metrics) to every write.
	Inference bool
	// PerNode switches to status.nodes[] server-side apply; it needs Node and an API that also
	// implements Applier (*kube.Client does). Without both, the publisher stays in legacy mode.
	PerNode bool
	Node    string
	Now     func() time.Time // nil = time.Now

	sent    map[string]map[string]struct{} // legacy: object path -> fields this node last sent
	applied map[string][2]string           // per-node: status path -> {namespace, name} with a live entry
}

// Applier is the extra request the per-node mode needs (kube.Client.Do).
type Applier interface {
	Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error)
}

// MeasuredFields holds the GryviaFabricSignal status fields one node measured for one job. A
// nil pointer means "this node did not measure it" and is never sent (as 0 or as null), so a
// node that saw nothing cannot erase what another node of the same job measured. Fields
// without a measured flag in Status (rank, p99s, rates, counts, skew, scoreDelta) count as
// measured when non-zero: for the degradation metrics 0 and "absent" mean the same thing.
type MeasuredFields struct {
	StragglerRank          *int32   `json:"stragglerRank,omitempty"`
	NCCLP99Ms              *float64 `json:"ncclP99ms,omitempty"`
	RDMARetryRate          *float64 `json:"rdmaRetryRate,omitempty"`
	GDSHitRatio            *float64 `json:"gdsHitRatio,omitempty"`
	OverlapIdleRatio       *float64 `json:"overlapIdleRatio,omitempty"`
	CNPRate                *float64 `json:"cnpRate,omitempty"`
	InferWaitP99Ms         *float64 `json:"inferWaitP99ms,omitempty"`
	PFCRate                *float64 `json:"pfcRate,omitempty"`
	ExfilEvents            *int64   `json:"exfilEvents,omitempty"`
	CollectiveMaxSkewMs    *float64 `json:"collectiveMaxSkewMs,omitempty"`
	GPUIdleDuringCommRatio *float64 `json:"gpuIdleDuringCommRatio,omitempty"`
	SMActiveDuringCompute  *float64 `json:"smActiveDuringCompute,omitempty"`
	GPUCorrelationCoverage *float64 `json:"gpuCorrelationCoverage,omitempty"`
	ScoreDelta             *float64 `json:"scoreDelta,omitempty"`

	// Inference engine fields (-infer-metrics).
	Engine          *string  `json:"engine,omitempty"`
	TTFTP99Ms       *float64 `json:"ttftP99ms,omitempty"`
	ITLP99Ms        *float64 `json:"itlP99ms,omitempty"`
	QueueTimeP99Ms  *float64 `json:"queueTimeP99ms,omitempty"`
	E2EP99Ms        *float64 `json:"e2eP99ms,omitempty"`
	QueueTimeMeanMs *float64 `json:"queueTimeMeanMs,omitempty"`
	E2EMeanMs       *float64 `json:"e2eMeanMs,omitempty"`
	RequestsWaiting *int64   `json:"requestsWaiting,omitempty"`
	KVCacheUsage    *float64 `json:"kvCacheUsage,omitempty"`
}

func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func finitePtr(v *float64) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil
	}
	c := *v
	return &c
}

// posPtr returns a pointer to the finite value v when it is above zero, else nil (not measured).
func posPtr(v float64) *float64 {
	if v = finite(v); v > 0 {
		return &v
	}
	return nil
}

func (m *MeasuredFields) setInference(in *Inference) {
	if in == nil {
		return
	}
	m.TTFTP99Ms, m.ITLP99Ms = finitePtr(in.TTFTP99MS), finitePtr(in.ITLP99MS)
	m.QueueTimeP99Ms, m.E2EP99Ms = finitePtr(in.QueueP99MS), finitePtr(in.E2EP99MS)
	m.QueueTimeMeanMs, m.E2EMeanMs = finitePtr(in.QueueMeanMS), finitePtr(in.E2EMeanMS)
	if w := finitePtr(in.RequestsWaiting); w != nil {
		n := int64(math.Round(math.Max(0, math.Min(*w, 1e12))))
		m.RequestsWaiting = &n
	}
	if kv := finitePtr(in.KVCacheUsage); kv != nil {
		c := math.Max(0, math.Min(1, *kv))
		m.KVCacheUsage = &c
	}
	if in.Engine != "" {
		e := in.Engine
		m.Engine = &e
	}
}

// Measured extracts the fields this node measured from a folded status.
func Measured(st Status, withInference bool) MeasuredFields {
	var m MeasuredFields
	if withInference {
		m.setInference(st.Inference)
	}
	if st.StragglerHits > 0 {
		r := int32(math.MaxInt32)
		if st.StragglerRank <= math.MaxInt32 {
			r = int32(st.StragglerRank)
		}
		m.StragglerRank = &r
	}
	m.NCCLP99Ms = posPtr(st.NCCLP99MS)
	m.RDMARetryRate = posPtr(st.RDMARetryRate)
	if st.GDSMeasured {
		v := clamp01(finite(st.GDSHitRatio))
		m.GDSHitRatio = &v
	}
	m.OverlapIdleRatio = posPtr(st.OverlapIdleRatio)
	m.CNPRate = posPtr(st.CNPRate)
	m.InferWaitP99Ms = posPtr(st.InferWaitP99MS)
	m.PFCRate = posPtr(st.PFCRate)
	if st.ExfilEvents > 0 {
		n := int64(math.MaxInt64)
		if st.ExfilEvents <= math.MaxInt64 {
			n = int64(st.ExfilEvents)
		}
		m.ExfilEvents = &n
	}
	m.CollectiveMaxSkewMs = posPtr(st.CollectiveMaxSkewMS)
	if st.GPUCorrelationMeasured {
		idle, cov := clamp01(finite(st.GPUIdleDuringCommRatio)), clamp01(finite(st.GPUCorrelationCoverage))
		m.GPUIdleDuringCommRatio, m.GPUCorrelationCoverage = &idle, &cov
		if st.GPUComputeMeasured {
			v := clamp01(finite(st.SMActiveDuringCompute))
			m.SMActiveDuringCompute = &v
		}
	}
	if d := math.Max(0, math.Min(1, finite(st.ScoreDelta))); d > 0 {
		m.ScoreDelta = &d
	}
	return m
}

// fieldMap renders m as a JSON object (only the measured fields).
func (m MeasuredFields) fieldMap() (map[string]any, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	err = json.Unmarshal(b, &out)
	return out, err
}

// legacyBody renders the merge-patch body: the measured fields, updatedAt, and an explicit
// null for each field THIS publisher sent earlier for the same object and no longer measures
// (so a recovered value is cleared by the node that set it, and by no one else).
func legacyBody(m MeasuredFields, updatedAt time.Time, prev map[string]struct{}) ([]byte, map[string]struct{}, error) {
	fields, err := m.fieldMap()
	if err != nil {
		return nil, nil, err
	}
	sent := make(map[string]struct{}, len(fields))
	for k := range fields {
		sent[k] = struct{}{}
	}
	for k := range prev {
		if _, ok := fields[k]; !ok {
			fields[k] = nil
		}
	}
	fields["updatedAt"] = updatedAt.UTC().Format(time.RFC3339)
	b, err := json.Marshal(map[string]any{"status": fields})
	return b, sent, err
}

func statusPatchBody(st Status, withInference bool) ([]byte, error) {
	b, _, err := legacyBody(Measured(st, withInference), st.UpdatedAt, nil)
	return b, err
}

// StatusPatchBody renders the merge-patch for one job's status (without the engine fields and
// without any explicit nulls: only what the node measured).
func StatusPatchBody(st Status) ([]byte, error) {
	return statusPatchBody(st, false)
}

// NodeEntryTTLSeconds is the ttlSeconds written into per-node entries: the fold window plus
// several missed publish cycles; the ai-operator merger ignores an entry older than this.
const NodeEntryTTLSeconds = 300

// nodeEntry is one element of status.nodes[].
type nodeEntry struct {
	Node        string `json:"node"`
	MeasuredAt  string `json:"measuredAt"`
	TTLSeconds  int32  `json:"ttlSeconds"`
	SampleCount int64  `json:"sampleCount"`
	MeasuredFields
}

// FieldManager is the server-side-apply field manager of a node's collector.
func FieldManager(node string) string {
	m := "gryvia-collector-" + node
	if len(m) > 128 {
		m = m[:128]
	}
	return m
}

// applyBody renders the server-side-apply body owning ONLY this node's status.nodes[] entry
// (listMapKey node). A nil entry renders an apply with no status, which releases (deletes) it.
func applyBody(ns, name string, e *nodeEntry) ([]byte, error) {
	obj := map[string]any{
		"apiVersion": fabricSignalGroupVersion,
		"kind":       "GryviaFabricSignal",
		"metadata":   map[string]any{"name": name, "namespace": ns},
	}
	if e != nil {
		obj["status"] = map[string]any{"nodes": []any{e}}
	}
	return json.Marshal(obj)
}

// Publishable reports whether a folder key names a real job: never the
// "_unattributed" bucket, never node-level "_node/*" keys.
func Publishable(k JobKey) bool {
	if k.Job == "" || strings.HasPrefix(k.Namespace, "_") || strings.HasPrefix(k.Job, "_") {
		return false
	}
	return k8sName.MatchString(k.Namespace) && k8sName.MatchString(k.Job)
}

type signalList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			JobRef string `json:"jobRef"`
		} `json:"spec"`
	} `json:"items"`
}

// PublishOnce runs one cycle and returns the number of objects patched.
func (p *Publisher) PublishOnce(ctx context.Context) int {
	if p == nil || p.API == nil || p.Folder == nil {
		return 0
	}
	log := p.Log
	if log == nil {
		log = nopLogger{}
	}
	snap := p.Folder.Snapshot()
	keys := make([]JobKey, 0, len(snap))
	for k := range snap {
		if Publishable(k) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Namespace != keys[j].Namespace {
			return keys[i].Namespace < keys[j].Namespace
		}
		return keys[i].Job < keys[j].Job
	})
	truncated := len(keys) > MaxPublishedJobs
	if truncated {
		keys = keys[:MaxPublishedJobs]
	}

	base := "/apis/" + fabricSignalGroupVersion + "/namespaces/"
	byNS := map[string]map[string][]string{} // namespace -> jobRef -> object names
	listed := map[string]bool{}
	patched := 0
	applier, perNode := p.applier()
	seen := map[string]bool{}
	unknownNS := map[string]bool{} // namespaces whose object list failed this cycle: do not release there
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	for _, k := range keys {
		if ctx.Err() != nil {
			return patched
		}
		if !listed[k.Namespace] {
			listed[k.Namespace] = true
			raw, err := p.API.Get(ctx, base+url.PathEscape(k.Namespace)+"/gryviafabricsignals")
			if err != nil {
				log.Warnw("fabric status: list GryviaFabricSignal failed", "namespace", k.Namespace, "error", err)
				unknownNS[k.Namespace] = true
				continue
			}
			var l signalList
			if err := json.Unmarshal(raw, &l); err != nil {
				log.Warnw("fabric status: bad GryviaFabricSignal list", "namespace", k.Namespace, "error", err)
				unknownNS[k.Namespace] = true
				continue
			}
			m := map[string][]string{}
			for _, it := range l.Items {
				if it.Spec.JobRef != "" && k8sName.MatchString(it.Metadata.Name) {
					m[it.Spec.JobRef] = append(m[it.Spec.JobRef], it.Metadata.Name)
				}
			}
			byNS[k.Namespace] = m
		}
		names := byNS[k.Namespace][k.Job]
		if len(names) == 0 {
			continue // nothing to update: never create
		}
		st := snap[k]
		for _, name := range names {
			path := base + url.PathEscape(k.Namespace) + "/gryviafabricsignals/" + url.PathEscape(name) + "/status"
			var err error
			if perNode {
				err = p.applyNode(ctx, applier, path, k.Namespace, name, st, now())
			} else {
				err = p.patchLegacy(ctx, path, st)
			}
			if err != nil {
				var se *kube.StatusError
				if errors.As(err, &se) && se.Code == http.StatusNotFound {
					delete(p.applied, path)
					continue // deleted between list and patch
				}
				log.Warnw("fabric status: patch failed", "namespace", k.Namespace, "name", name, "error", err)
				continue
			}
			patched++
			seen[path] = true
		}
	}
	if perNode && !truncated && ctx.Err() == nil {
		p.releaseGone(ctx, applier, seen, unknownNS, log)
	}
	return patched
}

func (p *Publisher) applier() (Applier, bool) {
	if !p.PerNode || !k8sName.MatchString(p.Node) {
		return nil, false
	}
	a, ok := p.API.(Applier)
	return a, ok
}

func (p *Publisher) patchLegacy(ctx context.Context, path string, st Status) error {
	if p.sent == nil || len(p.sent) > 4096 {
		p.sent = map[string]map[string]struct{}{}
	}
	body, sent, err := legacyBody(Measured(st, p.Inference), st.UpdatedAt, p.sent[path])
	if err != nil {
		return err
	}
	if _, err := p.API.MergePatch(ctx, path, body); err != nil {
		return err
	}
	p.sent[path] = sent
	return nil
}

func (p *Publisher) apply(ctx context.Context, a Applier, path string, body []byte) error {
	q := "?fieldManager=" + url.QueryEscape(FieldManager(p.Node)) + "&force=true"
	_, err := a.Do(ctx, http.MethodPatch, path+q, "application/apply-patch+yaml", body)
	return err
}

func (p *Publisher) applyNode(ctx context.Context, a Applier, path, ns, name string, st Status, now time.Time) error {
	e := &nodeEntry{
		Node:           p.Node,
		MeasuredAt:     now.UTC().Format(time.RFC3339),
		TTLSeconds:     NodeEntryTTLSeconds,
		SampleCount:    int64(st.SampleCount),
		MeasuredFields: Measured(st, p.Inference),
	}
	body, err := applyBody(ns, name, e)
	if err != nil {
		return err
	}
	if err := p.apply(ctx, a, path, body); err != nil {
		return err
	}
	if p.applied == nil {
		p.applied = map[string][2]string{}
	}
	p.applied[path] = [2]string{ns, name}
	return nil
}

// releaseGone drops this node's entry from objects it wrote earlier but no longer has signals for
// (the job left this node); without it the entry would linger until its TTL.
func (p *Publisher) releaseGone(ctx context.Context, a Applier, seen, unknownNS map[string]bool, log Logger) {
	for path, nn := range p.applied {
		if seen[path] || unknownNS[nn[0]] || ctx.Err() != nil {
			continue
		}
		body, err := applyBody(nn[0], nn[1], nil)
		if err == nil {
			err = p.apply(ctx, a, path, body)
		}
		var se *kube.StatusError
		if err != nil && !(errors.As(err, &se) && se.Code == http.StatusNotFound) {
			log.Warnw("fabric status: release node entry failed", "namespace", nn[0], "name", nn[1], "error", err)
			continue
		}
		delete(p.applied, path)
	}
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
