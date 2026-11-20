// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package discovery turns the Node labels published by NVIDIA GPU Feature
// Discovery (GFD), the node's allocatable resources and DCGM exporter metrics
// into the facts the Gryvia GPU operator needs. It is pure: no Kubernetes
// client, no cgo, no NVML.
package discovery

import (
	"strconv"
	"strings"
)

// Labels and resource names published by NVIDIA GFD / the NVIDIA device plugin.
const (
	LabelGPUPresent   = "nvidia.com/gpu.present"
	LabelGPUProduct   = "nvidia.com/gpu.product"
	LabelGPUCount     = "nvidia.com/gpu.count"
	LabelGPUMemory    = "nvidia.com/gpu.memory" // MiB per GPU
	LabelDriverMajor  = "nvidia.com/cuda.driver.major"
	LabelDriverMinor  = "nvidia.com/cuda.driver.minor"
	LabelDriverRev    = "nvidia.com/cuda.driver.rev"
	LabelRuntimeMajor = "nvidia.com/cuda.runtime.major"
	LabelRuntimeMinor = "nvidia.com/cuda.runtime.minor"
	LabelMIGCapable   = "nvidia.com/mig.capable"
	LabelRDMACapable  = "feature.node.kubernetes.io/rdma.capable"
	LabelSRIOVCapable = "feature.node.kubernetes.io/network-sriov.capable"
	ResourceGPU       = "nvidia.com/gpu"
)

// Phases returned by Readiness.
const (
	PhaseReady             = "Ready"
	PhaseDegraded          = "Degraded"
	PhaseWaitingForDrivers = "WaitingForDrivers"
)

// Health values returned by Health.
const (
	HealthHealthy  = "Healthy"
	HealthDegraded = "Degraded"
	HealthFailed   = "Failed"
	HealthUnknown  = "Unknown"
)

// Temperature thresholds (Celsius) for GPU health classification.
const (
	temperatureDegraded = 85
	temperatureFailed   = 95
)

// WaitingMessage is the message reported while no GPU is allocatable.
const WaitingMessage = "waiting for the NVIDIA driver and device plugin: managed by the NVIDIA GPU Operator " +
	"(helm value nvidia.enabled=true), or install the driver and container toolkit on the host"

// Discovered is what a GPU node advertises through its labels.
type Discovered struct {
	GPUType       string
	GPUCount      int
	MemoryGB      int
	RDMA          bool
	SRIOV         bool
	DriverVersion string
	CUDAVersion   string
	Allocatable   int
}

// NormalizeProduct maps a GFD product name to the short GPU type used across
// Gryvia (H100, A100-80G, A100-40G, L40, A10, T4, V100). Unknown products are
// returned unchanged: it never guesses.
func NormalizeProduct(product string) string {
	tokens := strings.FieldsFunc(strings.ToUpper(product), func(r rune) bool { return r == '-' || r == ' ' || r == '_' })
	has := func(t string) bool {
		for _, x := range tokens {
			if x == t {
				return true
			}
		}
		return false
	}
	switch {
	case has("H100"):
		return "H100"
	case has("A100"):
		switch {
		case has("80GB"):
			return "A100-80G"
		case has("40GB"):
			return "A100-40G"
		}
		return product
	case has("L40S"), has("L40"):
		return "L40"
	case has("A10"):
		return "A10"
	case has("T4"):
		return "T4"
	case has("V100"):
		return "V100"
	}
	return product
}

func atoi(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil
}

// FromNode extracts GPU facts from a node's labels and allocatable resources
// (quantities as strings). ok is false when the node has no GPU.
func FromNode(labels map[string]string, allocatable map[string]string) (Discovered, bool) {
	if labels[LabelGPUPresent] != "true" {
		return Discovered{}, false
	}
	d := Discovered{
		GPUType: NormalizeProduct(labels[LabelGPUProduct]),
		RDMA:    labels[LabelRDMACapable] == "true",
		SRIOV:   labels[LabelSRIOVCapable] == "true",
	}
	if n, ok := atoi(labels[LabelGPUCount]); ok && n > 0 {
		d.GPUCount = n
	}
	if mib, ok := atoi(labels[LabelGPUMemory]); ok && mib > 0 {
		d.MemoryGB = int((float64(mib) / 1024.0) + 0.5)
	}
	if labels[LabelDriverMajor] != "" {
		d.DriverVersion = labels[LabelDriverMajor]
		if labels[LabelDriverMinor] != "" {
			d.DriverVersion += "." + labels[LabelDriverMinor]
			if labels[LabelDriverRev] != "" {
				d.DriverVersion += "." + labels[LabelDriverRev]
			}
		}
	}
	if labels[LabelRuntimeMajor] != "" {
		d.CUDAVersion = labels[LabelRuntimeMajor]
		if labels[LabelRuntimeMinor] != "" {
			d.CUDAVersion += "." + labels[LabelRuntimeMinor]
		}
	}
	if n, ok := atoi(allocatable[ResourceGPU]); ok && n > 0 {
		d.Allocatable = n
	}
	return d, true
}

// Readiness classifies a node from the declared GPU count and the number of
// GPUs the device plugin has made allocatable.
func Readiness(gpuCount, allocatable int, driverVersion string) (phase, message string) {
	switch {
	case allocatable <= 0:
		return PhaseWaitingForDrivers, WaitingMessage
	case allocatable < gpuCount:
		return PhaseDegraded, "only " + strconv.Itoa(allocatable) + " of " + strconv.Itoa(gpuCount) +
			" GPUs are allocatable: check the NVIDIA device plugin and driver on the node"
	}
	msg := strconv.Itoa(allocatable) + " GPU(s) allocatable"
	if driverVersion != "" {
		msg += ", driver " + driverVersion
	}
	return PhaseReady, msg
}

// Health classifies a GPU by temperature in Celsius. A non-positive
// temperature means it is unknown.
func Health(temperature int) string {
	switch {
	case temperature <= 0:
		return HealthUnknown
	case temperature > temperatureFailed:
		return HealthFailed
	case temperature > temperatureDegraded:
		return HealthDegraded
	}
	return HealthHealthy
}
