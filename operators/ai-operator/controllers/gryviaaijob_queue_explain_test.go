package controllers

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func testClusterQueue(name, cohort, nominal, total, borrowed string) *unstructured.Unstructured {
	cq := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"cohort": cohort,
			"resourceGroups": []interface{}{map[string]interface{}{
				"coveredResources": []interface{}{"nvidia.com/gpu"},
				"flavors": []interface{}{map[string]interface{}{
					"name":      "default",
					"resources": []interface{}{map[string]interface{}{"name": "nvidia.com/gpu", "nominalQuota": nominal}},
				}},
			}},
		},
		"status": map[string]interface{}{
			"flavorsUsage": []interface{}{map[string]interface{}{
				"name": "default",
				"resources": []interface{}{map[string]interface{}{
					"name": "nvidia.com/gpu", "total": total, "borrowed": borrowed,
				}},
			}},
		},
	}}
	cq.SetName(name)
	return cq
}

func TestQueueCategory(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   workloadState
		msg  string
		want string
	}{
		{"no workload", workloadState{}, "", "WaitingForWorkload"},
		{"evicted", workloadState{Found: true, Evicted: true}, "x", "Evicted"},
		{"checks", workloadState{Found: true, QuotaReserved: true}, "", "AdmissionChecksPending"},
		{"quota", workloadState{Found: true}, "couldn't assign flavors: insufficient quota for nvidia.com/gpu", "QuotaExhausted"},
		{"other", workloadState{Found: true}, "waiting", "Pending"},
	} {
		if got := queueCategory(tc.st, "", tc.msg); got != tc.want {
			t.Errorf("%s: category = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestBuildQueueExplanationNamesBorrowers(t *testing.T) {
	mine := testClusterQueue("gryvia-a", "gryvia", "8", "8", "0")
	peers := []*unstructured.Unstructured{
		mine,
		testClusterQueue("gryvia-b", "gryvia", "8", "14", "6"),
		testClusterQueue("gryvia-c", "gryvia", "8", "10", "2"),
		testClusterQueue("gryvia-d", "gryvia", "8", "4", "0"),
		testClusterQueue("other-org", "other", "8", "20", "12"),
	}
	st := workloadState{Found: true}
	e := buildQueueExplanation(st, "Pending", "insufficient quota for nvidia.com/gpu", mine, peers)

	if e.Category != "QuotaExhausted" || e.ClusterQueue != "gryvia-a" || e.Cohort != "gryvia" {
		t.Fatalf("unexpected header: %+v", e)
	}
	if e.Nominal != 8 || e.InUse != 8 || e.Borrowed != 0 {
		t.Errorf("usage = %d/%d/%d, want 8/8/0", e.Nominal, e.InUse, e.Borrowed)
	}
	if len(e.Borrowers) != 2 || e.Borrowers[0].ClusterQueue != "gryvia-b" || e.Borrowers[0].Borrowed != 6 || e.Borrowers[1].ClusterQueue != "gryvia-c" {
		t.Errorf("borrowers = %+v, want gryvia-b(6) then gryvia-c(2) and nothing from another cohort", e.Borrowers)
	}
}

func TestBuildQueueExplanationWithoutQueue(t *testing.T) {
	e := buildQueueExplanation(workloadState{}, "WaitingForWorkload", "waiting", nil, nil)
	if e.Category != "WaitingForWorkload" || e.ClusterQueue != "" || len(e.Borrowers) != 0 {
		t.Fatalf("unexpected: %+v", e)
	}
}
