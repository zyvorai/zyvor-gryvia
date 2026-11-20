// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

const (
	// KindSlurm marks usage records of finished Slurm jobs.
	KindSlurm = "slurm"
	// labelUsageKind is the usage kind label (the LLM gateway sets tokens).
	labelUsageKind = "gryvia.io/usage-kind"
	// slurmRecordPrefix names records slurm-<cluster>-<job id>.
	slurmRecordPrefix = "slurm-"
)

// slurmAPIVersions are tried in order; the first one slurmrestd serves is kept. v0.0.44 is what the slurm-operator
// 1.2.3 itself uses with Slurm 26.05.
var slurmAPIVersions = []string{"v0.0.44", "v0.0.43"}

// slurmTerminal are the job states after which a job no longer runs.
var slurmTerminal = map[string]bool{
	"COMPLETED": true, "FAILED": true, "CANCELLED": true, "TIMEOUT": true, "NODE_FAIL": true,
	"OUT_OF_MEMORY": true, "BOOT_FAIL": true, "DEADLINE": true, "PREEMPTED": true,
}

// SlurmAccounting polls slurmrestd for finished jobs in the tenant partitions (gryvia-<tenant>) and writes one final
// GryviaUsageRecord per job (kind slurm) in the tenant namespace, so budgets and chargeback include Slurm.
//
// It reads slurmctld's job list (GET /slurm/<version>/jobs), not slurmdbd: a finished job stays there for slurmctld's
// MinJobAge (300 s by default), so the operator must not be down longer than that or jobs go unrecorded. Records are
// named after the cluster and job ID and created once, so polling again (or another replica after a failover) does
// not double count.
type SlurmAccounting struct {
	Client client.Client
	// Reader reads the token Secret (an uncached reader, so the manager does not cache every Secret); default Client.
	Reader client.Reader
	// Namespace holds the NodeSets and the token Secret.
	Namespace string
	// URL of slurmrestd, for example http://slurm-restapi.slurm.svc:6820.
	URL string
	// TokenSecret and TokenKey hold a JWT for slurmrestd (a Slinky Token CR creates it).
	TokenSecret string
	TokenKey    string
	Interval    time.Duration
	HTTP        *http.Client

	version string
}

// NeedLeaderElection keeps polling on the leader only.
func (a *SlurmAccounting) NeedLeaderElection() bool { return true }

// Start polls until the context ends.
func (a *SlurmAccounting) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("slurm-accounting")
	every := a.Interval
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := a.Sync(ctx); err != nil {
			logger.Error(err, "reading finished Slurm jobs")
		} else if n > 0 {
			logger.Info("recorded finished Slurm jobs", "records", n)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// slurmNumber reads a slurmrestd number: a plain number or {"set": true, "infinite": false, "number": N}.
type slurmNumber struct {
	Value int64
	Set   bool
}

func (n *slurmNumber) UnmarshalJSON(b []byte) error {
	var plain int64
	if err := json.Unmarshal(b, &plain); err == nil {
		n.Value, n.Set = plain, true
		return nil
	}
	var s struct {
		Set      bool  `json:"set"`
		Infinite bool  `json:"infinite"`
		Number   int64 `json:"number"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	n.Value, n.Set = s.Number, s.Set && !s.Infinite
	return nil
}

// SlurmJob is the part of a slurmrestd job the records use.
type SlurmJob struct {
	JobID     int64       `json:"job_id"`
	Name      string      `json:"name"`
	Cluster   string      `json:"cluster"`
	Partition string      `json:"partition"`
	UserName  string      `json:"user_name"`
	Account   string      `json:"account"`
	JobState  []string    `json:"job_state"`
	StartTime slurmNumber `json:"start_time"`
	EndTime   slurmNumber `json:"end_time"`
	NodeCount slurmNumber `json:"node_count"`
	TresAlloc string      `json:"tres_alloc_str"`
}

// Finished reports whether the job ended (a terminal state and not still completing).
func (j SlurmJob) Finished() bool {
	done := false
	for _, s := range j.JobState {
		if s == "COMPLETING" {
			return false
		}
		if slurmTerminal[s] {
			done = true
		}
	}
	return done && j.EndTime.Set && j.EndTime.Value > 0
}

// GPUs reads the GPU count and type from tres_alloc_str ("cpu=2,mem=1G,node=1,gres/gpu=2" or
// "gres/gpu:a100=2"; the typed entry wins).
func (j SlurmJob) GPUs() (int32, string) {
	var total int32
	gpuType := ""
	for _, part := range strings.Split(j.TresAlloc, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			continue
		}
		switch {
		case k == "gres/gpu":
			if total == 0 {
				total = int32(n)
			}
		case strings.HasPrefix(k, "gres/gpu:"):
			gpuType = strings.TrimPrefix(k, "gres/gpu:")
			total = int32(n)
		}
	}
	return total, gpuType
}

// RecordName is the usage record of a job.
func (j SlurmJob) RecordName() string {
	c := strings.Trim(nonDNS.ReplaceAllString(strings.ToLower(j.Cluster), "-"), "-")
	if c == "" {
		c = "cluster"
	}
	return fmt.Sprintf("%s%s-%d", slurmRecordPrefix, c, j.JobID)
}

func (a *SlurmAccounting) httpClient() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (a *SlurmAccounting) token(ctx context.Context) (string, error) {
	s := &corev1.Secret{}
	reader := a.Reader
	if reader == nil {
		reader = a.Client
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.TokenSecret}, s); err != nil {
		return "", fmt.Errorf("slurmrestd token secret %s/%s: %w", a.Namespace, a.TokenSecret, err)
	}
	t := strings.TrimSpace(string(s.Data[a.TokenKey]))
	if t == "" {
		return "", fmt.Errorf("slurmrestd token secret %s/%s has no %q", a.Namespace, a.TokenSecret, a.TokenKey)
	}
	return t, nil
}

// Jobs returns slurmctld's job list, finding the API version on first use.
func (a *SlurmAccounting) Jobs(ctx context.Context) ([]SlurmJob, error) {
	token, err := a.token(ctx)
	if err != nil {
		return nil, err
	}
	versions := slurmAPIVersions
	if a.version != "" {
		versions = []string{a.version}
	}
	var last error
	for _, v := range versions {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.URL, "/")+"/slurm/"+v+"/jobs", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-SLURM-USER-TOKEN", token)
		resp, err := a.httpClient().Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotFound {
			last = fmt.Errorf("slurmrestd does not serve %s", v)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("slurmrestd %s jobs: HTTP %d: %.300s", v, resp.StatusCode, body)
		}
		var out struct {
			Jobs []SlurmJob `json:"jobs"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("slurmrestd %s jobs: %w", v, err)
		}
		a.version = v
		return out.Jobs, nil
	}
	return nil, last
}

// partitions maps each managed NodeSet (partition) to its tenant.
func (a *SlurmAccounting) partitions(ctx context.Context) (map[string]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(NodeSetGVK.GroupVersion().WithKind("NodeSetList"))
	if err := a.Client.List(ctx, list, client.InNamespace(a.Namespace), client.MatchingLabels{kueueManagedByLabel: kueueManagedBy}); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, ns := range list.Items {
		if t := ns.GetLabels()[kueueTenantLabel]; t != "" {
			out[ns.GetName()] = t
		}
	}
	return out, nil
}

// Sync writes records for finished jobs in tenant partitions and returns how many it created.
func (a *SlurmAccounting) Sync(ctx context.Context) (int, error) {
	parts, err := a.partitions(ctx)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return 0, fmt.Errorf("slinky NodeSet CRD not found: is the slurm-operator installed?")
		}
		return 0, err
	}
	if len(parts) == 0 {
		return 0, nil
	}
	jobs, err := a.Jobs(ctx)
	if err != nil {
		return 0, err
	}
	rates := &GryviaUsageRecordReconciler{Client: a.Client}
	created := 0
	for _, j := range jobs {
		tenantName, ok := parts[j.Partition]
		if !ok || !j.Finished() {
			continue
		}
		ns := tenantNamespaceName(tenantName)
		rec := &gryviav1.GryviaUsageRecord{}
		if err := a.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: j.RecordName()}, rec); err == nil {
			continue
		} else if !errors.IsNotFound(err) {
			return created, err
		}
		tenant := &gryviav1.GryviaTenant{}
		if err := a.Client.Get(ctx, client.ObjectKey{Name: tenantName}, tenant); err != nil {
			tenant = nil
		}
		gpus, gpuType := j.GPUs()
		start := time.Unix(j.StartTime.Value, 0).UTC()
		end := time.Unix(j.EndTime.Value, 0).UTC()
		if !j.StartTime.Set || j.StartTime.Value <= 0 || end.Before(start) {
			start = end
		}
		hours := end.Sub(start).Hours() * float64(gpus)
		sku, rate, currency := "", 0.0, ""
		if gpus > 0 {
			if sku, rate, currency, err = rates.resolveRate(ctx, gpuType, tenant); err != nil {
				return created, err
			}
		}
		rec = &gryviav1.GryviaUsageRecord{
			ObjectMeta: metav1.ObjectMeta{
				Name: j.RecordName(), Namespace: ns,
				Labels: map[string]string{labelTenant: tenantName, labelUsageKind: KindSlurm},
				Annotations: map[string]string{
					"gryvia.io/slurm-job-id":    strconv.FormatInt(j.JobID, 10),
					"gryvia.io/slurm-partition": j.Partition,
					"gryvia.io/slurm-user":      j.UserName,
					"gryvia.io/slurm-account":   j.Account,
					"gryvia.io/slurm-state":     strings.Join(j.JobState, ","),
					"gryvia.io/slurm-tres":      j.TresAlloc,
				},
			},
			Spec: gryviav1.GryviaUsageRecordSpec{
				Tenant: tenantName, Job: "slurm:" + j.Name, JobUID: j.RecordName(), Kind: KindSlurm,
				GpuType: gpuType, Sku: sku, Gpus: gpus,
				Start: metav1.NewTime(start), End: &metav1.Time{Time: end},
				GpuHours: hours, Rate: rate, Cost: hours * rate, Currency: currency, Final: true,
			},
		}
		if err := a.Client.Create(ctx, rec); err != nil {
			if errors.IsAlreadyExists(err) {
				continue
			}
			if errors.IsNotFound(err) {
				// The tenant namespace is not there (yet); the job stays in slurmctld for MinJobAge.
				continue
			}
			return created, fmt.Errorf("usage record %s/%s: %w", ns, rec.Name, err)
		}
		created++
	}
	return created, nil
}
