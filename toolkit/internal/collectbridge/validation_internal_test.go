// SPDX-FileCopyrightText: Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package collectbridge

import (
	"strings"
	"testing"

	"github.com/dsx-ai-factory/health-validation/collect"
	"github.com/dsx-ai-factory/health-validation/collect/observation"
	gpusignals "github.com/dsx-ai-factory/health-validation/collect/signals/gpu"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCanonicalTargetKinds(t *testing.T) {
	value := func(s collect.Signal, target *observation.ObservationTarget) *observation.Observation {
		return &observation.Observation{
			SignalId: s.ID(), Source: "test", Target: target, ObservedAt: timestamppb.Now(), Unit: s.Unit(),
			Outcome: &observation.Observation_Value{Value: &observation.Value{Kind: &observation.Value_IntValue{IntValue: 1}}},
		}
	}
	draw := gpusignals.PowerDraw
	if err := validateCollectObservation(value(draw, observation.GPUTarget("GPU-1")), draw); err != nil {
		t.Fatalf("GPU target rejected: %v", err)
	}
	err := validateCollectObservation(value(draw, observation.NodeTarget("")), draw)
	if err == nil || !strings.Contains(err.Error(), "requires a GPU target") {
		t.Fatalf("node target for a GPU signal: err=%v", err)
	}

	// A later entry overrides an earlier one, so a per-link signal can be
	// carved out of the whole-GPU entry.
	index := canonicalIndex([]canonicalTarget{
		{kind: targetGPU, signals: gpusignals.All()},
		{kind: targetNVLinkEndpoint, signals: collect.Signals{draw}},
	})
	if got := index[draw.ID()].target; got != targetNVLinkEndpoint {
		t.Fatalf("override target=%q, want %q", got, targetNVLinkEndpoint)
	}
	if got := index[gpusignals.PowerLimitEnforced.ID()].target; got != targetGPU {
		t.Fatalf("untouched target=%q, want %q", got, targetGPU)
	}
}
