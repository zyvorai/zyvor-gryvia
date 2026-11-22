// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func elasticJob(min, nodes int32) *gryviav1.GryviaAIJob {
	j := cpuJob("el")
	j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: nodes, Framework: "pytorch", Backend: "gloo",
		Elastic: &gryviav1.ElasticConfig{MinNodes: min}}
	return j
}

func envValue(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// An elastic job asks the Indexed Job for maxNodes workers, finishes at minNodes successes, and hands the launcher min:max.
func TestElasticJobShape(t *testing.T) {
	r, _ := newAIJobReconciler()
	bj, err := r.buildJob(elasticJob(2, 4))
	if err != nil {
		t.Fatal(err)
	}
	if *bj.Spec.Completions != 4 || *bj.Spec.Parallelism != 4 {
		t.Errorf("completions/parallelism = %d/%d, want 4/4", *bj.Spec.Completions, *bj.Spec.Parallelism)
	}
	sp := bj.Spec.SuccessPolicy
	if sp == nil || len(sp.Rules) != 1 || sp.Rules[0].SucceededCount == nil || *sp.Rules[0].SucceededCount != 2 {
		t.Fatalf("successPolicy = %+v, want one rule with succeededCount 2", sp)
	}
	env := bj.Spec.Template.Spec.Containers[0].Env
	if v, _ := envValue(env, "NNODES"); v != "2:4" {
		t.Errorf("NNODES = %q, want 2:4", v)
	}
	for k, want := range map[string]string{"GRYVIA_ELASTIC": "true", "GRYVIA_ELASTIC_MIN_NODES": "2", "GRYVIA_ELASTIC_MAX_NODES": "4",
		"TORCH_DISABLE_SHARE_RDZV_TCP_STORE": "1"} {
		if v, _ := envValue(env, k); v != want {
			t.Errorf("%s = %q, want %q", k, v, want)
		}
	}
	n := 0
	for _, e := range env {
		if e.Name == "NNODES" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("NNODES appears %d times", n)
	}
}

// A TORCH_DISABLE_SHARE_RDZV_TCP_STORE in the spec wins over the operator's default.
func TestElasticSharedStoreUserOverride(t *testing.T) {
	r, _ := newAIJobReconciler()
	j := elasticJob(1, 2)
	j.Spec.Env = []corev1.EnvVar{{Name: "TORCH_DISABLE_SHARE_RDZV_TCP_STORE", Value: "0"}}
	bj, err := r.buildJob(j)
	if err != nil {
		t.Fatal(err)
	}
	var vals []string
	for _, e := range bj.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "TORCH_DISABLE_SHARE_RDZV_TCP_STORE" {
			vals = append(vals, e.Value)
		}
	}
	if len(vals) != 1 || vals[0] != "0" {
		t.Errorf("TORCH_DISABLE_SHARE_RDZV_TCP_STORE = %v, want [0]", vals)
	}
}

// min == nodes has no success policy; non-elastic jobs are untouched.
func TestElasticDefaultsAndNonElastic(t *testing.T) {
	r, _ := newAIJobReconciler()
	bj, _ := r.buildJob(elasticJob(1, 3))
	if *bj.Spec.Completions != 3 || bj.Spec.SuccessPolicy == nil {
		t.Errorf("completions=%d policy=%v", *bj.Spec.Completions, bj.Spec.SuccessPolicy)
	}
	fixed, _ := r.buildJob(elasticJob(3, 3))
	if fixed.Spec.SuccessPolicy != nil {
		t.Error("min == max must not set a success policy")
	}
	plain := cpuJob("plain")
	plain.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 2}
	pj, _ := r.buildJob(plain)
	if pj.Spec.SuccessPolicy != nil {
		t.Error("a non-elastic job got a success policy")
	}
	if v, _ := envValue(pj.Spec.Template.Spec.Containers[0].Env, "NNODES"); v != "2" {
		t.Errorf("non-elastic NNODES = %q, want 2", v)
	}
	for _, k := range []string{"GRYVIA_ELASTIC", "TORCH_DISABLE_SHARE_RDZV_TCP_STORE"} {
		if _, ok := envValue(pj.Spec.Template.Spec.Containers[0].Env, k); ok {
			t.Errorf("non-elastic job got %s", k)
		}
	}
}

func TestValidateElastic(t *testing.T) {
	cases := []struct {
		name string
		job  *gryviav1.GryviaAIJob
		kind string
		want string
	}{
		{"ok", elasticJob(1, 4), gryviav1.WorkloadKindJob, ""},
		{"min zero", elasticJob(0, 4), gryviav1.WorkloadKindJob, "minNodes must be at least 1"},
		{"min above nodes", elasticJob(5, 4), gryviav1.WorkloadKindJob, "must be at least distributed.elastic.minNodes"},
		{"statefulset", elasticJob(1, 4), gryviav1.WorkloadKindStatefulSet, "run-to-completion Job"},
		{"not elastic", cpuJob("x"), gryviav1.WorkloadKindStatefulSet, ""},
	}
	for _, c := range cases {
		err := validateElastic(c.job, c.kind)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err=%v, want %q", c.name, err, c.want)
		}
	}
	tf := elasticJob(1, 2)
	tf.Spec.Distributed.Framework = "tensorflow"
	if err := validateElastic(tf, gryviav1.WorkloadKindJob); err == nil || !strings.Contains(err.Error(), "PyTorch") {
		t.Errorf("tensorflow: %v", err)
	}
}
