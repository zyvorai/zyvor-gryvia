// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import (
	"context"
	"strings"
	"testing"
)

func TestKubeSourcesEgressCaps(t *testing.T) {
	api := newFakeAPI()
	api.lists["/apis/gryvia.io/v1alpha1/gryviaquotas"] = `{"items":[
	 {"metadata":{"name":"a"},"spec":{"namespaces":["x","y"],"network":{"maxEgressMbps":100}}},
	 {"metadata":{"name":"b"},"spec":{"namespaces":["z"]}},
	 {"metadata":{"name":"c"},"spec":{"namespaces":["z"],"network":{"maxEgressMbps":0}}},
	 {"metadata":{"name":"d"},"spec":{"namespaces":["z"],"network":{}}}]}`
	k := &KubeSources{API: api, Node: "n1"}
	caps, err := k.EgressCaps(context.Background())
	if err != nil || len(caps) != 1 || caps[0].Quota != "a" || caps[0].Mbps != 100 || len(caps[0].Namespaces) != 2 {
		t.Fatalf("%v %v", caps, err)
	}
	api.lists["/apis/gryvia.io/v1alpha1/gryviaquotas"] = "garbage"
	if _, err := k.EgressCaps(context.Background()); err == nil {
		t.Fatal("garbage accepted")
	}
	api.getErr = errBoom
	if _, err := k.EgressCaps(context.Background()); err == nil {
		t.Fatal("API error swallowed")
	}
}

var errBoom = &boom{}

type boom struct{}

func (*boom) Error() string { return "boom" }

func TestKubeSourcesNodePods(t *testing.T) {
	api := newFakeAPI()
	var asked string
	k := &KubeSources{API: api, Node: "n1"}
	path := "/api/v1/namespaces/a/pods?fieldSelector=spec.nodeName%3Dn1%2Cstatus.phase%3DRunning"
	api.lists[path] = `{"items":[
	 {"metadata":{"uid":"u1","name":"ok","namespace":"a"},"spec":{"nodeName":"n1"},"status":{"phase":"Running","qosClass":"Burstable"}},
	 {"metadata":{"uid":"u2","name":"other-node","namespace":"a"},"spec":{"nodeName":"n2"},"status":{"phase":"Running","qosClass":"Burstable"}},
	 {"metadata":{"uid":"u3","name":"pending","namespace":"a"},"spec":{"nodeName":"n1"},"status":{"phase":"Pending","qosClass":"Burstable"}},
	 {"metadata":{"uid":"u4","name":"dying","namespace":"a","deletionTimestamp":"2026-01-01T00:00:00Z"},"spec":{"nodeName":"n1"},"status":{"phase":"Running","qosClass":"Burstable"}},
	 {"metadata":{"uid":"u5","name":"wrong-ns","namespace":"kube-system"},"spec":{"nodeName":"n1"},"status":{"phase":"Running","qosClass":"Burstable"}}]}`
	pods, err := k.NodePods(context.Background(), "a")
	if err != nil || len(pods) != 1 || pods[0].Name != "ok" || pods[0].QOS != "Burstable" || pods[0].UID != "u1" {
		t.Fatalf("%v %v (gets %v)", pods, err, api.gets)
	}
	asked = strings.Join(api.gets, ",")
	if !strings.Contains(asked, "fieldSelector=spec.nodeName%3Dn1") {
		t.Fatalf("no node field selector: %s", asked)
	}
	if _, err := (&KubeSources{API: api}).NodePods(context.Background(), "a"); err == nil {
		t.Fatal("empty node name accepted (would list every node's pods)")
	}
	if _, err := k.NodePods(context.Background(), "../x"); err == nil {
		t.Fatal("bad namespace accepted")
	}
}
