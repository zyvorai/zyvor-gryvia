package controllers

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func TestElasticInitialSize(t *testing.T) {
	r, _ := newAIJobReconciler()
	j := elasticJob(2, 4)
	j.Spec.Distributed.Elastic.DesiredNodes = 3
	bj, err := r.buildJob(j)
	if err != nil {
		t.Fatal(err)
	}
	if *bj.Spec.Parallelism != 3 || *bj.Spec.Completions != 3 {
		t.Errorf("parallelism/completions = %d/%d, want 3/3", *bj.Spec.Parallelism, *bj.Spec.Completions)
	}
	if v, _ := envValue(bj.Spec.Template.Spec.Containers[0].Env, "NNODES"); v != "2:4" {
		t.Errorf("NNODES = %q, want 2:4 (the bounds, not the current size)", v)
	}

	j.Labels = map[string]string{LabelKueueQueue: "q"}
	bj, _ = r.buildJob(j)
	if *bj.Spec.Parallelism != 4 {
		t.Errorf("Kueue job parallelism = %d, want 4 (partial admission picks the size)", *bj.Spec.Parallelism)
	}
}

func TestDesiredWorkersClamp(t *testing.T) {
	d := &gryviav1.DistributedConfig{Enabled: true, Nodes: 4, Elastic: &gryviav1.ElasticConfig{MinNodes: 2}}
	for desired, want := range map[int32]int32{0: 4, 1: 2, 3: 3, 9: 4} {
		d.Elastic.DesiredNodes = desired
		if n, ok := d.DesiredWorkers(); !ok || n != want {
			t.Errorf("desired %d -> %d, %v; want %d", desired, n, ok, want)
		}
	}
	if _, ok := (&gryviav1.DistributedConfig{Enabled: true, Nodes: 2}).DesiredWorkers(); ok {
		t.Error("non-elastic job reported desired workers")
	}
}

// resizeFixture: a Running elastic job (min 1, nodes 4) whose Indexed Job has the given size.
func resizeFixture(t *testing.T, size int32) (*GryviaAIJobReconciler, *gryviav1.GryviaAIJob, *batchv1.Job) {
	t.Helper()
	j := elasticJob(1, 4)
	j.Namespace = "default"
	j.Spec.Distributed.Elastic.DesiredNodes = size
	j.Status.Phase = PhaseRunning
	r, _ := newAIJobReconciler(j)
	bj, err := r.buildJob(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := controllerutil.SetControllerReference(j, bj, r.Scheme); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(context.Background(), bj); err != nil {
		t.Fatal(err)
	}
	return r, j, bj
}

func resizedJob(t *testing.T, r *GryviaAIJobReconciler, j *gryviav1.GryviaAIJob) *batchv1.Job {
	t.Helper()
	bj := &batchv1.Job{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: j.Namespace, Name: j.Name}, bj); err != nil {
		t.Fatal(err)
	}
	return bj
}

func condition(j *gryviav1.GryviaAIJob, typ string) (status, reason, msg string) {
	for _, c := range j.Status.Conditions {
		if c.Type == typ {
			return string(c.Status), c.Reason, c.Message
		}
	}
	return "", "", ""
}

func TestElasticResizeGrowAndShrink(t *testing.T) {
	ctx := context.Background()
	r, j, bj := resizeFixture(t, 2)

	j.Spec.Distributed.Elastic.DesiredNodes = 3
	if err := r.reconcileElasticResize(ctx, j, bj); err != nil {
		t.Fatal(err)
	}
	got := resizedJob(t, r, j)
	if *got.Spec.Parallelism != 3 || *got.Spec.Completions != 3 {
		t.Fatalf("after grow parallelism/completions = %d/%d, want 3/3", *got.Spec.Parallelism, *got.Spec.Completions)
	}
	if s, reason, msg := condition(j, ConditionResized); s != "True" || reason != "Grown" || msg != "2 -> 3 workers" {
		t.Errorf("Resized = %s/%s/%q", s, reason, msg)
	}
	if st := j.Status.Elastic; st == nil || st.CurrentNodes != 3 || st.DesiredNodes != 3 || st.Resizes != 1 || st.LastResizeTime == nil {
		t.Errorf("status.elastic = %+v", st)
	}

	j.Spec.Distributed.Elastic.DesiredNodes = 2
	if err := r.reconcileElasticResize(ctx, j, got); err != nil {
		t.Fatal(err)
	}
	got = resizedJob(t, r, j)
	if *got.Spec.Parallelism != 2 || *got.Spec.Completions != 2 {
		t.Fatalf("after shrink parallelism/completions = %d/%d, want 2/2", *got.Spec.Parallelism, *got.Spec.Completions)
	}
	if _, reason, _ := condition(j, ConditionResized); reason != "Shrunk" {
		t.Errorf("Resized reason = %s, want Shrunk", reason)
	}
	if j.Status.Elastic.Resizes != 2 {
		t.Errorf("resizes = %d, want 2", j.Status.Elastic.Resizes)
	}

	// Unchanged: no update, status mirrors the Job.
	rv := got.ResourceVersion
	if err := r.reconcileElasticResize(ctx, j, got); err != nil {
		t.Fatal(err)
	}
	if resizedJob(t, r, j).ResourceVersion != rv || j.Status.Elastic.Resizes != 2 {
		t.Error("an unchanged size updated the Job")
	}
}

func TestElasticResizeBlocked(t *testing.T) {
	ctx := context.Background()

	t.Run("kueue", func(t *testing.T) {
		r, j, bj := resizeFixture(t, 4)
		bj.Labels[LabelKueueQueue] = "q"
		two := int32(2)
		bj.Spec.Parallelism = &two
		j.Spec.Distributed.Elastic.DesiredNodes = 3
		if err := r.reconcileElasticResize(ctx, j, bj); err != nil {
			t.Fatal(err)
		}
		if *resizedJob(t, r, j).Spec.Parallelism != 4 {
			t.Error("Kueue-managed Job was resized")
		}
		if s, reason, _ := condition(j, ConditionResizeBlocked); s != "True" || reason != "KueueManaged" {
			t.Errorf("ResizeBlocked = %s/%s", s, reason)
		}
	})

	t.Run("finished", func(t *testing.T) {
		r, j, bj := resizeFixture(t, 2)
		bj.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue}}
		j.Spec.Distributed.Elastic.DesiredNodes = 3
		if err := r.reconcileElasticResize(ctx, j, bj); err != nil {
			t.Fatal(err)
		}
		if *resizedJob(t, r, j).Spec.Parallelism != 2 {
			t.Error("finishing Job was resized")
		}
	})

	t.Run("terminal phase", func(t *testing.T) {
		r, j, bj := resizeFixture(t, 2)
		j.Status.Phase = PhaseSucceeded
		j.Spec.Distributed.Elastic.DesiredNodes = 3
		if err := r.reconcileElasticResize(ctx, j, bj); err != nil {
			t.Fatal(err)
		}
		if *resizedJob(t, r, j).Spec.Parallelism != 2 {
			t.Error("succeeded job was resized")
		}
	})

	t.Run("out of bounds", func(t *testing.T) {
		r, j, bj := resizeFixture(t, 2)
		j.Spec.Distributed.Elastic.DesiredNodes = 9
		if err := r.reconcileElasticResize(ctx, j, bj); err != nil {
			t.Fatal(err)
		}
		if *resizedJob(t, r, j).Spec.Parallelism != 2 {
			t.Error("out-of-bounds request resized the Job")
		}
		if s, reason, msg := condition(j, ConditionResizeBlocked); s != "True" || reason != "OutOfBounds" || !strings.Contains(msg, "got 9") {
			t.Errorf("ResizeBlocked = %s/%s/%q", s, reason, msg)
		}
	})
}

// The reconcile loop applies desiredNodes to an existing Job and reports it in status.
func TestElasticResizeThroughReconcileBatchJob(t *testing.T) {
	ctx := context.Background()
	r, j, _ := resizeFixture(t, 2)
	j.Spec.Distributed.Elastic.DesiredNodes = 4
	if err := r.reconcileBatchJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	if got := resizedJob(t, r, j); *got.Spec.Parallelism != 4 {
		t.Errorf("parallelism = %d, want 4", *got.Spec.Parallelism)
	}
	if j.Status.Elastic == nil || j.Status.Elastic.CurrentNodes != 4 {
		t.Errorf("status.elastic = %+v", j.Status.Elastic)
	}
}
