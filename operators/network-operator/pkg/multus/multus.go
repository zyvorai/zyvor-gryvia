// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package multus

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-operator/api/v1"
)

// CreateNetworkAttachment creates a Multus NetworkAttachmentDefinition
func CreateNetworkAttachment(ctx context.Context, k8sClient client.Client, network *gryviav1.GryviaNetwork) error {
	config, err := generateNetworkConfig(network)
	if err != nil {
		return fmt.Errorf("failed to generate network config: %w", err)
	}

	// Use the network's target namespace. GryviaNetwork is cluster-scoped so it
	// has no namespace of its own; the user should set spec.targetNamespace
	// explicitly. We fall back to "default" but log a warning so operators
	// notice the implicit behaviour.
	namespace := network.Spec.TargetNamespace
	if namespace == "" {
		namespace = "default"
		logger := log.FromContext(ctx)
		logger.Info("GryviaNetwork has no targetNamespace set, defaulting NAD namespace to \"default\"",
			"network", network.Name)
	}

	nad := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "k8s.cni.cncf.io/v1",
			"kind":       "NetworkAttachmentDefinition",
			"metadata": map[string]interface{}{
				"name":      network.Name,
				"namespace": namespace,
				"labels": map[string]interface{}{
					"gryvia.io/network": network.Name,
					"gryvia.io/type":    network.Spec.NetworkType,
				},
			},
			"spec": map[string]interface{}{
				"config": config,
			},
		},
	}

	nad.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "k8s.cni.cncf.io",
		Version: "v1",
		Kind:    "NetworkAttachmentDefinition",
	})

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(nad.GroupVersionKind())

	err = k8sClient.Get(ctx, types.NamespacedName{Name: network.Name, Namespace: namespace}, existing)
	if err != nil {
		if errors.IsNotFound(err) {
			return k8sClient.Create(ctx, nad)
		}
		return err
	}

	// Update existing
	nad.SetResourceVersion(existing.GetResourceVersion())
	return k8sClient.Update(ctx, nad)
}

func generateNetworkConfig(network *gryviav1.GryviaNetwork) (string, error) {
	switch network.Spec.NetworkType {
	case "rdma":
		return generateRDMAConfig(network)
	case "sriov":
		return generateSRIOVConfig(network)
	case "standard":
		return generateStandardConfig(network)
	default:
		return "", fmt.Errorf("unknown network type: %s", network.Spec.NetworkType)
	}
}

func generateRDMAConfig(network *gryviav1.GryviaNetwork) (string, error) {
	if network.Spec.RDMA == nil {
		return "", fmt.Errorf("RDMA configuration required")
	}

	// Default MTU for RDMA networks is 9000 (jumbo frames).
	// Valid range: 1280 (IPv6 minimum) to 9216 (common jumbo frame max).
	mtu := network.Spec.MTU
	if mtu == 0 {
		mtu = 9000
	}
	if mtu < 1280 || mtu > 9216 {
		return "", fmt.Errorf("invalid MTU %d: must be between 1280 and 9216", mtu)
	}

	configObj := map[string]interface{}{
		"cniVersion": "0.3.1",
		"name":       network.Name,
		"type":       "macvlan",
		"master":     "ib0",
		"mode":       "bridge",
		"mtu":        mtu,
		"ipam": map[string]interface{}{
			"type":    "whereabouts",
			"range":   network.Spec.RDMA.Subnet,
			"gateway": network.Spec.RDMA.Gateway,
		},
	}
	configBytes, err := json.Marshal(configObj)
	if err != nil {
		return "", fmt.Errorf("failed to marshal RDMA config: %w", err)
	}

	return string(configBytes), nil
}

func generateSRIOVConfig(network *gryviav1.GryviaNetwork) (string, error) {
	if network.Spec.SRIOV == nil {
		return "", fmt.Errorf("SR-IOV configuration required")
	}

	// Default MTU for SR-IOV networks is 9000 (jumbo frames).
	// Valid range: 1280 (IPv6 minimum) to 9216 (common jumbo frame max).
	mtu := network.Spec.MTU
	if mtu == 0 {
		mtu = 9000
	}
	if mtu < 1280 || mtu > 9216 {
		return "", fmt.Errorf("invalid MTU %d: must be between 1280 and 9216", mtu)
	}

	// Use SRIOV spec values for IPAM if available, otherwise use defaults
	subnet := "10.56.0.0/16"
	gateway := "10.56.217.1"
	if network.Spec.SRIOV != nil {
		if network.Spec.SRIOV.Subnet != "" {
			subnet = network.Spec.SRIOV.Subnet
		}
		if network.Spec.SRIOV.Gateway != "" {
			gateway = network.Spec.SRIOV.Gateway
		}
	}

	configObj := map[string]interface{}{
		"cniVersion": "0.3.1",
		"name":       network.Name,
		"type":       "sriov",
		"vlan":       0,
		"mtu":        mtu,
		"ipam": map[string]interface{}{
			"type":    "host-local",
			"subnet":  subnet,
			"routes":  []map[string]string{{"dst": "0.0.0.0/0"}},
			"gateway": gateway,
		},
	}
	configBytes, err := json.Marshal(configObj)
	if err != nil {
		return "", fmt.Errorf("failed to marshal SR-IOV config: %w", err)
	}

	return string(configBytes), nil
}

func generateStandardConfig(network *gryviav1.GryviaNetwork) (string, error) {
	// Default MTU for standard networks is 1500.
	// Valid range: 1280 (IPv6 minimum) to 9216 (common jumbo frame max).
	mtu := network.Spec.MTU
	if mtu == 0 {
		mtu = 1500
	}
	if mtu < 1280 || mtu > 9216 {
		return "", fmt.Errorf("invalid MTU %d: must be between 1280 and 9216", mtu)
	}

	configObj := map[string]interface{}{
		"cniVersion": "0.3.1",
		"name":       network.Name,
		"type":       "bridge",
		"bridge":     "gryvia0",
		"mtu":        mtu,
		"ipam": map[string]interface{}{
			"type":   "host-local",
			"subnet": "10.244.0.0/16",
		},
	}
	configBytes, err := json.Marshal(configObj)
	if err != nil {
		return "", fmt.Errorf("failed to marshal standard config: %w", err)
	}

	return string(configBytes), nil
}

// GetNetworkAttachment retrieves a NetworkAttachmentDefinition
func GetNetworkAttachment(ctx context.Context, k8sClient client.Client, name, namespace string) (*unstructured.Unstructured, error) {
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "k8s.cni.cncf.io",
		Version: "v1",
		Kind:    "NetworkAttachmentDefinition",
	})

	err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, nad)
	return nad, err
}
