// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package aggregator

import (
	"testing"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/decoder"
)

func TestUnknownRankDoesNotCreateStraggler(t *testing.T) {
	a := NewNCCLAggregator(time.Minute)
	for i := 0; i < 5; i++ {
		a.Process(decoder.GPUEvent{EventType: decoder.GPUEvtNCCLOp, SrcRank: 0, WorldSize: 0, LatencyNs: 100})
	}
	a.DetectStragglers()
	if len(a.rankLatency) != 0 || len(a.GetStragglers()) != 0 {
		t.Fatal("unknown rank was treated as rank zero")
	}
	if a.GetOpStats()["AllReduce"].Count != 5 {
		t.Fatal("collective count was lost")
	}
}
