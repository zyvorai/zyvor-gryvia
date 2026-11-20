// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

// TotalGPUs is the number of GPUs the job holds in total: distributed.nodes pods with
// distributed.gpusPerNode GPUs each for a distributed job (gpusPerNode falls back to 1 when
// spec.gpus is set and to 0 for a CPU-only job), spec.gpus otherwise. It mirrors the
// controller's replica and per-pod counts and is what quota and budget checks must use.
func (s GryviaAIJobSpec) TotalGPUs() int32 {
	if s.Distributed != nil && s.Distributed.Enabled {
		nodes := s.Distributed.Nodes
		if nodes < 1 {
			nodes = 1
		}
		per := s.Distributed.GpusPerNode
		if per <= 0 {
			if s.GPUs == 0 {
				return 0
			}
			per = 1
		}
		return nodes * per
	}
	if s.GPUs < 0 {
		return 1
	}
	return s.GPUs
}
