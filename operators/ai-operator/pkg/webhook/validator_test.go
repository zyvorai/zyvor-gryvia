package webhook

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func validJob() *gryviav1.GryviaAIJob {
	return &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{Name: "train-1", Namespace: "default"},
		Spec: gryviav1.GryviaAIJobSpec{
			Type:  "training",
			GPUs:  4,
			Image: "pytorch/pytorch:2.1.0",
		},
	}
}

func TestValidateJob(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*gryviav1.GryviaAIJob)
		wantErr string // empty means valid
	}{
		{"valid", func(j *gryviav1.GryviaAIJob) {}, ""},
		{"zero gpus is a CPU-only job", func(j *gryviav1.GryviaAIJob) { j.Spec.GPUs = 0 }, ""},
		{"negative gpus", func(j *gryviav1.GryviaAIJob) { j.Spec.GPUs = -2 }, "spec.gpus must not be negative, got -2"},
		{"workload kind job", func(j *gryviav1.GryviaAIJob) { j.Spec.WorkloadKind = "job" }, ""},
		{"bad workload kind", func(j *gryviav1.GryviaAIJob) { j.Spec.WorkloadKind = "deployment" }, "spec.workloadKind \"deployment\""},
		{"timeout days", func(j *gryviav1.GryviaAIJob) { j.Spec.Timeout = "7d" }, ""},
		{"bad timeout", func(j *gryviav1.GryviaAIJob) { j.Spec.Timeout = "soon" }, "spec.timeout"},
		{"negative retry limit", func(j *gryviav1.GryviaAIJob) { j.Spec.RetryLimit = -1 }, "spec.retryLimit must not be negative"},
		{"empty image", func(j *gryviav1.GryviaAIJob) { j.Spec.Image = "" }, "spec.image is required"},
		{"bad name", func(j *gryviav1.GryviaAIJob) { j.Name = "Bad_Name" }, "metadata.name \"Bad_Name\" is invalid"},
		{"bad type", func(j *gryviav1.GryviaAIJob) { j.Spec.Type = "mining" }, "spec.type \"mining\" is not supported; valid values: training"},
		{"empty type", func(j *gryviav1.GryviaAIJob) { j.Spec.Type = "" }, "spec.type is required"},
		{"bad network", func(j *gryviav1.GryviaAIJob) { j.Spec.Network = "wifi" }, "spec.network \"wifi\""},
		{"priority high", func(j *gryviav1.GryviaAIJob) { j.Spec.Priority = 101 }, "spec.priority must be between 0 and 100"},
		{"bad framework", func(j *gryviav1.GryviaAIJob) {
			j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 2, Framework: "caffe"}
		}, "distributed.framework \"caffe\""},
		{"distributed no nodes", func(j *gryviav1.GryviaAIJob) {
			j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true}
		}, "distributed.nodes must be greater than 0"},
		{"distributed too big", func(j *gryviav1.GryviaAIJob) {
			j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 200, GpusPerNode: 8}
		}, "unreasonably large"},
		{"elastic ok", func(j *gryviav1.GryviaAIJob) {
			j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 4, Elastic: &gryviav1.ElasticConfig{MinNodes: 2}}
		}, ""},
		{"elastic min zero", func(j *gryviav1.GryviaAIJob) {
			j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 4, Elastic: &gryviav1.ElasticConfig{}}
		}, "distributed.elastic.minNodes must be between 1 and distributed.nodes (4)"},
		{"elastic min above nodes", func(j *gryviav1.GryviaAIJob) {
			j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 4, Elastic: &gryviav1.ElasticConfig{MinNodes: 5}}
		}, "distributed.elastic.minNodes must be between 1 and distributed.nodes (4)"},
		{"elastic desired ok", func(j *gryviav1.GryviaAIJob) {
			j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 4, Elastic: &gryviav1.ElasticConfig{MinNodes: 2, DesiredNodes: 3}}
		}, ""},
		{"elastic desired below min", func(j *gryviav1.GryviaAIJob) {
			j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 4, Elastic: &gryviav1.ElasticConfig{MinNodes: 2, DesiredNodes: 1}}
		}, "distributed.elastic.desiredNodes must be between minNodes (2) and distributed.nodes (4)"},
		{"elastic desired above nodes", func(j *gryviav1.GryviaAIJob) {
			j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 4, Elastic: &gryviav1.ElasticConfig{MinNodes: 2, DesiredNodes: 5}}
		}, "distributed.elastic.desiredNodes must be between minNodes (2) and distributed.nodes (4)"},
		{"elastic tensorflow", func(j *gryviav1.GryviaAIJob) {
			j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 4, Framework: "tensorflow", Elastic: &gryviav1.ElasticConfig{MinNodes: 1}}
		}, "distributed.elastic is for PyTorch"},
		{"distributed disabled ignored", func(j *gryviav1.GryviaAIJob) {
			j.Spec.Distributed = &gryviav1.DistributedConfig{Framework: "caffe"}
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := validJob()
			tt.mutate(j)
			errs := ValidateJob(j)
			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("expected valid, got %v", errs)
				}
				return
			}
			if !strings.Contains(strings.Join(errs, "; "), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, errs)
			}
		})
	}
}

func TestValidateJobReportsAllErrors(t *testing.T) {
	j := validJob()
	j.Spec.GPUs = -1
	j.Spec.Image = ""
	if errs := ValidateJob(j); len(errs) != 2 {
		t.Fatalf("expected 2 errors, got %v", errs)
	}
}

func review(t *testing.T, op admissionv1.Operation, job *gryviav1.GryviaAIJob) *admissionv1.AdmissionReview {
	t.Helper()
	job.APIVersion, job.Kind = "gryvia.io/v1alpha1", "GryviaAIJob"
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	return &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       "abc-123",
			Operation: op,
			Name:      job.Name,
			Namespace: job.Namespace,
			Kind:      metav1.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaAIJob"},
			Resource:  metav1.GroupVersionResource{Group: "gryvia.io", Version: "v1alpha1", Resource: "gryviaaijobs"},
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
}

func post(t *testing.T, h http.Handler, ar *admissionv1.AdmissionReview) *admissionv1.AdmissionReview {
	t.Helper()
	body, _ := json.Marshal(ar)
	req := httptest.NewRequest(http.MethodPost, ValidatePath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := &admissionv1.AdmissionReview{}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAdmissionReviewOverHTTP(t *testing.T) {
	h := &admission.Webhook{Handler: NewGryviaAIJobValidator(nil)}

	ok := post(t, h, review(t, admissionv1.Create, validJob()))
	if ok.Response == nil || !ok.Response.Allowed || ok.Response.UID != "abc-123" {
		t.Fatalf("valid job should be allowed: %+v", ok.Response)
	}

	bad := validJob()
	bad.Spec.GPUs = -1
	bad.Spec.Image = ""
	denied := post(t, h, review(t, admissionv1.Create, bad))
	if denied.Response == nil || denied.Response.Allowed {
		t.Fatalf("invalid job should be denied: %+v", denied.Response)
	}
	msg := denied.Response.Result.Message
	if !strings.Contains(msg, "spec.gpus must not be negative") || !strings.Contains(msg, "spec.image is required") {
		t.Fatalf("unhelpful message: %q", msg)
	}

	del := post(t, h, review(t, admissionv1.Delete, bad))
	if !del.Response.Allowed {
		t.Fatal("deletes must always be allowed")
	}
}

// Every GryviaAIJob shipped in examples/ must pass the validator.
func TestExamplesAreAccepted(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "examples")
	if _, err := os.Stat(root); err != nil {
		t.Skip("examples directory not available")
	}
	checked := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !(strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
		for {
			var raw map[string]interface{}
			if err := dec.Decode(&raw); err != nil {
				if err == io.EOF {
					break
				}
				t.Logf("skipping unparsable document in %s: %v", path, err)
				break
			}
			if raw["kind"] != "GryviaAIJob" {
				continue
			}
			b, _ := json.Marshal(raw)
			job := &gryviav1.GryviaAIJob{}
			if err := json.Unmarshal(b, job); err != nil {
				t.Errorf("%s: cannot decode job: %v", path, err)
				continue
			}
			checked++
			if errs := ValidateJob(job); len(errs) > 0 {
				t.Errorf("%s: job %q rejected: %v", path, job.Name, errs)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no GryviaAIJob examples found; is the path right?")
	}
	t.Logf("validated %d example jobs", checked)
}
