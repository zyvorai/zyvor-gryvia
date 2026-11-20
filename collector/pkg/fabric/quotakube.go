// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fabric

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// KubeSources reads GryviaQuota objects and this node's pods through the API
// server. It only ever issues GET requests.
type KubeSources struct {
	API  KubeAPI
	Node string // this node's name (NODE_NAME); required
}

type quotaList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Namespaces []string `json:"namespaces"`
			Network    *struct {
				MaxEgressMbps *int `json:"maxEgressMbps"`
			} `json:"network"`
		} `json:"spec"`
	} `json:"items"`
}

// EgressCaps lists the quotas that set spec.network.maxEgressMbps > 0.
func (k *KubeSources) EgressCaps(ctx context.Context) ([]EgressCap, error) {
	raw, err := k.API.Get(ctx, "/apis/gryvia.io/v1alpha1/gryviaquotas")
	if err != nil {
		return nil, err
	}
	var l quotaList
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	var out []EgressCap
	for _, it := range l.Items {
		if it.Spec.Network == nil || it.Spec.Network.MaxEgressMbps == nil || *it.Spec.Network.MaxEgressMbps == 0 {
			continue
		}
		out = append(out, EgressCap{Quota: it.Metadata.Name, Namespaces: it.Spec.Namespaces, Mbps: *it.Spec.Network.MaxEgressMbps})
	}
	return out, nil
}

type nodePodList struct {
	Items []struct {
		Metadata struct {
			UID               string `json:"uid"`
			Name              string `json:"name"`
			Namespace         string `json:"namespace"`
			DeletionTimestamp string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
		Status struct {
			Phase    string `json:"phase"`
			QOSClass string `json:"qosClass"`
		} `json:"status"`
	} `json:"items"`
}

// NodePods lists the Running, not-terminating pods of a namespace scheduled on Node.
func (k *KubeSources) NodePods(ctx context.Context, namespace string) ([]PodRef, error) {
	if k.Node == "" {
		return nil, fmt.Errorf("node name unknown")
	}
	if !k8sName.MatchString(namespace) {
		return nil, fmt.Errorf("invalid namespace %q", namespace)
	}
	q := url.Values{"fieldSelector": {"spec.nodeName=" + k.Node + ",status.phase=Running"}}
	raw, err := k.API.Get(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/pods?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var l nodePodList
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	var out []PodRef
	for _, p := range l.Items {
		// The field selector is not trusted alone.
		if p.Spec.NodeName != k.Node || p.Status.Phase != "Running" || p.Metadata.DeletionTimestamp != "" ||
			p.Metadata.Namespace != namespace {
			continue
		}
		out = append(out, PodRef{Namespace: namespace, Name: p.Metadata.Name, UID: p.Metadata.UID, QOS: p.Status.QOSClass})
	}
	return out, nil
}
