// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package discovery

import "testing"

func TestNormalizeProduct(t *testing.T) {
	cases := map[string]string{
		"NVIDIA-H100-80GB-HBM3":   "H100",
		"NVIDIA-H100-PCIe":        "H100",
		"NVIDIA-H100-NVL":         "H100",
		"NVIDIA-A100-SXM4-80GB":   "A100-80G",
		"NVIDIA-A100-80GB-PCIe":   "A100-80G",
		"NVIDIA-A100-PCIE-40GB":   "A100-40G",
		"NVIDIA-A100-SXM4-40GB":   "A100-40G",
		"NVIDIA-L40S":             "L40",
		"NVIDIA-L40":              "L40",
		"NVIDIA-A10":              "A10",
		"Tesla-T4":                "T4",
		"Tesla-V100-SXM2-16GB":    "V100",
		"NVIDIA-GeForce-RTX-4090": "NVIDIA-GeForce-RTX-4090",
		"NVIDIA-A10G":             "NVIDIA-A10G",
		"NVIDIA-A100":             "NVIDIA-A100",
		"NVIDIA-H200":             "NVIDIA-H200",
		"":                        "",
		"something-new":           "something-new",
	}
	for in, want := range cases {
		if got := NormalizeProduct(in); got != want {
			t.Errorf("NormalizeProduct(%q)=%q want %q", in, got, want)
		}
	}
}

func TestFromNode(t *testing.T) {
	if _, ok := FromNode(map[string]string{"nvidia.com/gpu.present": "false"}, nil); ok {
		t.Fatal("expected not ok")
	}
	if _, ok := FromNode(nil, nil); ok {
		t.Fatal("expected not ok for nil labels")
	}
	labels := map[string]string{
		LabelGPUPresent: "true", LabelGPUProduct: "NVIDIA-H100-80GB-HBM3", LabelGPUCount: "8",
		LabelGPUMemory: "81559", LabelDriverMajor: "550", LabelDriverMinor: "54", LabelDriverRev: "15",
		LabelRuntimeMajor: "12", LabelRuntimeMinor: "4", LabelRDMACapable: "true", LabelSRIOVCapable: "false",
	}
	d, ok := FromNode(labels, map[string]string{"nvidia.com/gpu": "8"})
	if !ok {
		t.Fatal("expected ok")
	}
	want := Discovered{GPUType: "H100", GPUCount: 8, MemoryGB: 80, RDMA: true, DriverVersion: "550.54.15", CUDAVersion: "12.4", Allocatable: 8}
	if d != want {
		t.Errorf("got %+v want %+v", d, want)
	}
	// Sparse labels and bad values.
	d, ok = FromNode(map[string]string{LabelGPUPresent: "true", LabelGPUCount: "x", LabelGPUMemory: "-1", LabelDriverMajor: "535"}, map[string]string{"nvidia.com/gpu": "junk"})
	if !ok || d.GPUCount != 0 || d.MemoryGB != 0 || d.Allocatable != 0 || d.DriverVersion != "535" || d.CUDAVersion != "" {
		t.Errorf("unexpected %+v ok=%v", d, ok)
	}
	if d, _ := FromNode(map[string]string{LabelGPUPresent: "true", LabelGPUMemory: "24576"}, nil); d.MemoryGB != 24 {
		t.Errorf("memory rounding: %d", d.MemoryGB)
	}
}

func TestReadiness(t *testing.T) {
	cases := []struct {
		count, alloc int
		phase        string
	}{
		{8, 8, PhaseReady}, {8, 9, PhaseReady}, {8, 0, PhaseWaitingForDrivers},
		{8, 3, PhaseDegraded}, {0, 0, PhaseWaitingForDrivers}, {0, 2, PhaseReady},
	}
	for _, c := range cases {
		phase, msg := Readiness(c.count, c.alloc, "550.1")
		if phase != c.phase || msg == "" {
			t.Errorf("Readiness(%d,%d)=%s,%q want %s", c.count, c.alloc, phase, msg, c.phase)
		}
	}
	if _, msg := Readiness(8, 0, ""); msg != WaitingMessage {
		t.Errorf("message %q", msg)
	}
}

func TestHealth(t *testing.T) {
	cases := map[int]string{0: HealthUnknown, -3: HealthUnknown, 40: HealthHealthy, 85: HealthHealthy, 86: HealthDegraded, 95: HealthDegraded, 96: HealthFailed}
	for in, want := range cases {
		if got := Health(in); got != want {
			t.Errorf("Health(%d)=%s want %s", in, got, want)
		}
	}
}

const dcgmSample = `# HELP DCGM_FI_DEV_GPU_TEMP GPU temperature (in C).
# TYPE DCGM_FI_DEV_GPU_TEMP gauge
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-aaa",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="n1"} 45
DCGM_FI_DEV_GPU_TEMP{gpu="1",UUID="GPU-bbb",device="nvidia1",modelName="x, y"} 90
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-aaa"} 73
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-aaa"} 1024
DCGM_FI_DEV_FB_FREE{gpu="0",UUID="GPU-aaa"} 80000
DCGM_FI_DEV_POWER_USAGE{gpu="0",UUID="GPU-aaa"} 310.6
DCGM_FI_DEV_SM_CLOCK{gpu="0",UUID="GPU-aaa"} 1980

garbage line
DCGM_FI_DEV_GPU_TEMP{gpu="x"} 5
DCGM_FI_DEV_GPU_TEMP{gpu="2" 5
DCGM_FI_DEV_GPU_TEMP{gpu="3"} NaN
DCGM_FI_DEV_GPU_TEMP{gpu="4"}
DCGM_FI_DEV_GPU_TEMP{gpu="5",UUID="unterminated} 5
`

func TestParseDCGM(t *testing.T) {
	got := ParseDCGM(dcgmSample)
	if len(got) != 2 {
		t.Fatalf("want 2 gpus, got %+v", got)
	}
	g0 := got[0]
	if g0.Index != 0 || g0.UUID != "GPU-aaa" || g0.Temperature != 45 || !g0.HasTemperature || g0.Utilization != 73 ||
		g0.MemoryUsed != 1024 || g0.MemoryFree != 80000 || g0.PowerUsage != 311 || g0.MemoryTotal() != 81024 {
		t.Errorf("gpu0 %+v", g0)
	}
	if got[1].Index != 1 || got[1].Temperature != 90 || got[1].UUID != "GPU-bbb" {
		t.Errorf("gpu1 %+v", got[1])
	}
}

func TestParseDCGMRobust(t *testing.T) {
	for _, in := range []string{"", "\n\n", "#", "{", "}{", "a{", "a{b=} 1", `DCGM_FI_DEV_GPU_TEMP{gpu="0"`, "\x00\xff", `DCGM_FI_DEV_GPU_TEMP{gpu="0",="}"} 1`} {
		_ = ParseDCGM(in)
	}
}
