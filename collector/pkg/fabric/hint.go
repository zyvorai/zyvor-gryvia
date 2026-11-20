// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fabric

import "strings"

// TransportHint classifies a process environment (the NUL-separated contents of
// /proc/<pid>/environ) into the NCCL_XPORT_* value nccl_transport.c reads from its
// transport_hint map. It reports only what the environment states: NCCL picks its
// transport at run time, so an environment that does not force one yields
// XportUnknown rather than a guess. Unknown (0) is valid; the signal still fires.
func TransportHint(environ string) uint32 {
	p2pOff := envGet(environ, "NCCL_P2P_DISABLE") == "1"
	shmOff := envGet(environ, "NCCL_SHM_DISABLE") == "1"
	net := strings.ToLower(envGet(environ, "NCCL_NET"))
	switch {
	case strings.Contains(net, "roce"):
		return XportNetRoCE
	case net == "ib" || strings.Contains(net, "infiniband"):
		return XportNetIB
	case p2pOff && shmOff:
		return XportNet
	case p2pOff:
		return XportSHM
	default:
		return XportUnknown
	}
}

func envGet(environ, key string) string {
	prefix := key + "="
	for _, line := range strings.Split(environ, "\x00") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}
