// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import "testing"

func TestTransportHint(t *testing.T) {
	cases := []struct {
		name, env string
		want      uint32
	}{
		{"empty", "", XportUnknown},
		{"nothing forced", "PATH=/bin\x00HOME=/root", XportUnknown},
		{"p2p allowed is not asserted", "NCCL_P2P_DISABLE=0", XportUnknown},
		{"p2p off falls to shm", "NCCL_P2P_DISABLE=1", XportSHM},
		{"p2p and shm off means net", "NCCL_P2P_DISABLE=1\x00NCCL_SHM_DISABLE=1", XportNet},
		{"shm off alone", "NCCL_SHM_DISABLE=1", XportUnknown},
		{"ib plugin", "NCCL_NET=IB", XportNetIB},
		{"roce wins over p2p flags", "NCCL_NET=RoCE\x00NCCL_P2P_DISABLE=1", XportNetRoCE},
		{"value is trimmed", "NCCL_P2P_DISABLE= 1 ", XportSHM},
		{"key prefix does not match", "XNCCL_P2P_DISABLE=1", XportUnknown},
	}
	for _, c := range cases {
		if got := TransportHint(c.env); got != c.want {
			t.Errorf("%s: TransportHint = %d, want %d", c.name, got, c.want)
		}
	}
}
