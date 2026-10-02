// SPDX-FileCopyrightText: Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package collectadapters

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dsx-ai-factory/health-validation/catalog/gpu"
	"github.com/dsx-ai-factory/health-validation/catalog/health"
	"github.com/dsx-ai-factory/health-validation/catalog/nvlink"
	"github.com/dsx-ai-factory/health-validation/collect/observation"
	gpusignals "github.com/dsx-ai-factory/health-validation/collect/signals/gpu"
	"github.com/dsx-ai-factory/health-validation/toolkit/observations"
)

// sourceLevelGPU identifies failures not attributable to one GPU.
const sourceLevelGPU = "gpu"

// domainAdapter assembles one catalog category from canonical GPU signals.
type domainAdapter struct {
	signalIDs []string
	category  health.Category
}

// GPU prepares the cutover of toolkit.GPUSensor (toolkit.NewGPUSensor).
// The app still registers that legacy sensor; only tests use this adapter.
// Unmapped catalog signals remain failures until cutover coverage is complete.
func GPU() *domainAdapter {
	return &domainAdapter{category: health.GPU, signalIDs: []string{
		gpusignals.PowerDraw.ID(),
		gpusignals.PowerLimitRequested.ID(),
		gpusignals.PowerLimitEnforced.ID(),
		gpusignals.PerformanceState.ID(),
		gpusignals.HardwareSlowdownActive.ID(),
		gpusignals.HardwareThermalSlowdownActive.ID(),
		gpusignals.HardwarePowerBrakeSlowdownActive.ID(),
	}}
}

// NVLink prepares the cutover of toolkit.NVLinkSensor (toolkit.NewNVLinkSensor).
// The app still registers that legacy sensor; only tests use this adapter.
// Per-link catalog signals remain unmapped until cutover coverage is complete.
func NVLink() *domainAdapter {
	return &domainAdapter{category: health.NVLink, signalIDs: []string{
		gpusignals.FabricManagerStatus.ID(),
		gpusignals.NVLinkFabricBandwidthDegraded.ID(),
		gpusignals.NVLinkFabricRouteRecoveryActive.ID(),
		gpusignals.NVLinkFabricRouteUnhealthy.ID(),
		gpusignals.NVLinkFabricAccessTimeoutRecovery.ID(),
	}}
}

func (a *domainAdapter) Name() string {
	if a.category == health.GPU {
		return "gpu"
	}
	return "nvlink"
}

func (a *domainAdapter) Signals() []string { return slices.Clone(a.signalIDs) }

// Build consumes observations already validated by the bridge.
func (a *domainAdapter) Build(obs []*observation.Observation) []health.Measurement {
	measurements := adaptCategory(a.category, a.signalIDs, obs)
	if len(measurements) == 0 {
		return a.Errored(fmt.Errorf("no %s observations were collected", a.Name()))
	}
	return measurements
}

// Errored reports err against every catalog signal of the category.
func (a *domainAdapter) Errored(err error) []health.Measurement {
	if a.category == health.GPU {
		return []health.Measurement{gpu.ErroredGPU(sourceLevelGPU, err, gpu.MeasurementSignals...)}
	}
	return []health.Measurement{nvlink.ErroredNVLink(sourceLevelGPU, err, nvlink.MeasurementSignals...)}
}

// adaptCategory groups observations per GPU, converts each group with
// observations.ToMeasurements, and keeps measurements of category.
func adaptCategory(category health.Category, signals []string, obs []*observation.Observation) []health.Measurement {
	groups := make(map[adapterKey][]*observation.Observation)
	for _, o := range obs {
		if !slices.Contains(signals, o.GetSignalId()) {
			continue
		}
		key := adapterGroup(o)
		groups[key] = append(groups[key], o)
	}
	var out []health.Measurement
	keys := make([]adapterKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	// Map iteration is random; sort so Build output order is stable for tests,
	// logs, and run-to-run diffs.
	slices.SortFunc(keys, func(a, b adapterKey) int { return strings.Compare(a.label(), b.label()) })
	for _, key := range keys {
		for _, m := range observations.ToMeasurements(groups[key]) {
			if m.Category() != category {
				continue
			}
			// UUID-less slots are meaningful only within their producing source.
			if key.kind != "uuid" && key.namespace != "" {
				switch value := m.(type) {
				case gpu.GPUMeasurement:
					value.GPUID = key.label()
					m = value
				case nvlink.NVLinkMeasurement:
					value.GPUID = key.label()
					m = value
				}
			}
			out = append(out, m)
		}
	}
	return out
}

// adapterKey identifies one GPU group: by UUID, by source-local slot, or by
// source for source-scoped observations.
type adapterKey struct {
	namespace string
	gpuID     string
	kind      string
}

// label renders k as a sortable, source-qualified GPU ID.
func (k adapterKey) label() string { return fmt.Sprintf("%s:%q/%q", k.kind, k.namespace, k.gpuID) }

// adapterGroup assigns o to its GPU group. Slots are namespaced by source so
// two producers' slot 0 are never merged without a UUID.
func adapterGroup(o *observation.Observation) adapterKey {
	ref := o.GetTarget().GetGpu()
	if ref.GetUuid() != "" {
		return adapterKey{kind: "uuid", gpuID: ref.GetUuid()}
	}
	if slot := ref.GetSourceLocalId(); slot != "" {
		return adapterKey{kind: "slot", namespace: o.GetSource(), gpuID: slot}
	}
	return adapterKey{kind: "source", namespace: o.GetSource()}
}
