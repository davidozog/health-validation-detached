// SPDX-FileCopyrightText: Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package collectbridge

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/dsx-ai-factory/health-validation/collect"
	"github.com/dsx-ai-factory/health-validation/collect/observation"
	gpusignals "github.com/dsx-ai-factory/health-validation/collect/signals/gpu"
	"google.golang.org/protobuf/proto"
)

// Target kinds a canonical signal's observations may describe.
const (
	targetNode           = "node"
	targetGPU            = "GPU"
	targetNVLinkEndpoint = "NVLink endpoint"
	targetGPUPair        = "GPU pair"
)

// canonicalTargets lists the target kind each canonical signal accepts.
// Migrating domains append entries; a later entry overrides an earlier one
// for the same signal, so a per-link or GPU-pair signal in gpusignals is
// declared after the whole-GPU entry. observations.targetID attributes
// endpoint and pair targets to their source GPU, so a new kind needs a
// matching mapping there.
var canonicalTargets = []canonicalTarget{
	{kind: targetGPU, signals: gpusignals.All()},
}

type canonicalTarget struct {
	kind    string
	signals collect.Signals
}

// canonicalRule is HV's acceptance rule for one canonical signal: its
// definition, which fixes the unit, and its target kind.
type canonicalRule struct {
	signal collect.Signal
	target string
}

var canonical = canonicalIndex(canonicalTargets)

func canonicalIndex(entries []canonicalTarget) map[string]canonicalRule {
	out := make(map[string]canonicalRule)
	for _, entry := range entries {
		for _, s := range entry.signals {
			out[s.ID()] = canonicalRule{signal: s, target: entry.kind}
		}
	}
	return out
}

// targetKind names the kind of a typed target, or "" for none.
func targetKind(target *observation.ObservationTarget) string {
	switch target.GetKind().(type) {
	case *observation.ObservationTarget_Node:
		return targetNode
	case *observation.ObservationTarget_GPU:
		return targetGPU
	case *observation.ObservationTarget_NVLinkEndpoint:
		return targetNVLinkEndpoint
	case *observation.ObservationTarget_GPUPair:
		return targetGPUPair
	default:
		return ""
	}
}

// sameCollectUnit treats a nil unit and an empty unit as different.
func sameCollectUnit(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// validateCollectDefinition rejects empty IDs and canonical signals declared
// with a different unit.
func validateCollectDefinition(s collect.Signal) error {
	if s == nil || strings.TrimSpace(s.ID()) == "" {
		return fmt.Errorf("nonempty signal ID is required")
	}
	if rule, ok := canonical[s.ID()]; ok && !sameCollectUnit(s.Unit(), rule.signal.Unit()) {
		return fmt.Errorf("signal %q redefines its canonical unit", s.ID())
	}
	return nil
}

// validateCollectBatch checks cycle identity, signal ownership, and every
// observation. Duplicates are keyed by signal and target; source-scoped and
// source-local targets also key on source, since their identity is only
// meaningful within it.
func validateCollectBatch(batch *observation.ObservationBatch, cycleID string, owned collect.Signals) error {
	if batch == nil {
		return fmt.Errorf("observation batch is required")
	}
	if batch.GetCollectionId() != cycleID || strings.TrimSpace(batch.GetCollectionId()) == "" {
		return fmt.Errorf("collection ID %q does not match active cycle %q", batch.GetCollectionId(), cycleID)
	}
	if err := observation.ValidateBatch(batch); err != nil {
		return err
	}
	definitions := make(map[string]collect.Signal, len(owned))
	for _, s := range owned {
		definitions[s.ID()] = s
	}
	seen := make(map[string]bool)
	for i, o := range batch.GetObservations() {
		definition := definitions[o.GetSignalId()]
		if definition == nil {
			return fmt.Errorf("observation %d: signal %q is not owned by this sensor", i, o.GetSignalId())
		}
		if err := validateCollectObservation(o, definition); err != nil {
			return fmt.Errorf("observation %d: %w", i, err)
		}
		target, _ := proto.MarshalOptions{Deterministic: true}.Marshal(o.GetTarget())
		namespace := ""
		if o.GetTarget() == nil || collectSourceLocal(o.GetTarget()) {
			namespace = o.GetSource()
		}
		key := o.GetSignalId() + "\x00" + namespace + "\x00" + string(target)
		if seen[key] {
			return fmt.Errorf("observation %d: duplicate signal %q for one target", i, o.GetSignalId())
		}
		seen[key] = true
	}
	return nil
}

// validateCollectObservation applies HV's acceptance rules to one observation:
// provenance, a past timestamp, canonical unit, a valid target, exactly one
// outcome, and, for canonical signals, the target kind in canonicalTargets.
// Whether a value's kind fits a signal is left to the adapter that interprets
// it, so a wrong scalar fails only the checks that read it. These rules live
// here rather than in collect so its contract is unchanged for other
// consumers.
func validateCollectObservation(o *observation.Observation, s collect.Signal) error {
	if strings.TrimSpace(o.GetSource()) == "" {
		return fmt.Errorf("source is required")
	}
	if strings.TrimSpace(o.GetSignalId()) == "" {
		return fmt.Errorf("signal ID is required")
	}
	at := o.GetObservedAt()
	if at == nil || (at.Seconds == 0 && at.Nanos == 0) {
		return fmt.Errorf("nonzero observation timestamp is required")
	}
	if err := at.CheckValid(); err != nil {
		return fmt.Errorf("observation timestamp: %w", err)
	}
	if at.AsTime().After(time.Now()) {
		return fmt.Errorf("observation timestamp is in the future")
	}
	if !sameCollectUnit(o.Unit, s.Unit()) {
		return fmt.Errorf("signal %q has noncanonical unit", s.ID())
	}
	if err := validateCollectTarget(o.GetTarget()); err != nil {
		return err
	}
	if collectSourceLocal(o.GetTarget()) && o.GetCollectionError() == nil {
		return fmt.Errorf("source-local GPU identity requires a collection error")
	}
	switch outcome := o.GetOutcome().(type) {
	case *observation.Observation_Value:
		if o.GetTarget() == nil {
			return fmt.Errorf("value requires a typed target")
		}
		switch kind := outcome.Value.GetKind().(type) {
		case *observation.Value_IntValue:
			if kind == nil {
				return fmt.Errorf("value kind is required")
			}
		case *observation.Value_DoubleValue:
			if kind == nil || math.IsNaN(kind.DoubleValue) || math.IsInf(kind.DoubleValue, 0) {
				return fmt.Errorf("finite double value is required")
			}
		case *observation.Value_StringValue:
			if kind == nil {
				return fmt.Errorf("value kind is required")
			}
		case *observation.Value_BoolValue:
			if kind == nil {
				return fmt.Errorf("value kind is required")
			}
		default:
			return fmt.Errorf("value kind is required")
		}
	case *observation.Observation_CollectionError:
		category := outcome.CollectionError.GetCategory()
		if category < observation.CollectionErrorCategoryTimeout || category > observation.CollectionErrorCategorySourceError {
			return fmt.Errorf("defined collection-error category is required")
		}
	case *observation.Observation_NoValue:
		switch outcome.NoValue.GetReason() {
		case observation.NoValueReasonNoTargets:
			if o.GetTarget() != nil {
				return fmt.Errorf("NO_TARGETS must be source-scoped")
			}
		case observation.NoValueReasonNotSupported:
			if o.GetTarget() == nil {
				return fmt.Errorf("NOT_SUPPORTED requires a target")
			}
		default:
			return fmt.Errorf("defined no-value reason is required")
		}
	default:
		return fmt.Errorf("observation outcome is required")
	}
	if rule, ok := canonical[s.ID()]; ok && o.GetTarget() != nil && targetKind(o.GetTarget()) != rule.target {
		return fmt.Errorf("signal %q requires a %s target", s.ID(), rule.target)
	}
	return nil
}

// validateCollectTarget accepts a nil (source-scoped) target or one typed
// target with valid GPU identities.
func validateCollectTarget(target *observation.ObservationTarget) error {
	if target == nil {
		return nil
	}
	switch kind := target.GetKind().(type) {
	case *observation.ObservationTarget_Node:
		if kind == nil || kind.Node == nil {
			return fmt.Errorf("node target is required")
		}
	case *observation.ObservationTarget_GPU:
		if kind == nil {
			return fmt.Errorf("GPU target is required")
		}
		return validateCollectGPURef(kind.Gpu)
	case *observation.ObservationTarget_NVLinkEndpoint:
		if kind == nil {
			return fmt.Errorf("NVLink endpoint is required")
		}
		return validateCollectGPURef(kind.NvLinkEndpoint.GetGpu())
	case *observation.ObservationTarget_GPUPair:
		if kind == nil {
			return fmt.Errorf("GPU pair is required")
		}
		if err := validateCollectGPURef(kind.GpuPair.GetSourceGpu()); err != nil {
			return err
		}
		return validateCollectGPURef(kind.GpuPair.GetPeerGpu())
	default:
		return fmt.Errorf("typed target kind is required")
	}
	return nil
}

// validateCollectGPURef requires exactly one of UUID or source-local slot ID.
func validateCollectGPURef(ref *observation.GPURef) error {
	if ref == nil || (strings.TrimSpace(ref.GetUuid()) == "") == (strings.TrimSpace(ref.GetSourceLocalId()) == "") {
		return fmt.Errorf("GPU target requires exactly one UUID or source-local ID")
	}
	return nil
}

// collectSourceLocal reports whether any GPU in target lacks a UUID.
func collectSourceLocal(target *observation.ObservationTarget) bool {
	return target.GetGpu().GetSourceLocalId() != "" || target.GetNvLinkEndpoint().GetGpu().GetSourceLocalId() != "" || target.GetGpuPair().GetSourceGpu().GetSourceLocalId() != "" || target.GetGpuPair().GetPeerGpu().GetSourceLocalId() != ""
}
