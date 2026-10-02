// SPDX-FileCopyrightText: Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package collectbridge

import (
	"fmt"
	"testing"

	"github.com/dsx-ai-factory/health-validation/collect"
	"github.com/dsx-ai-factory/health-validation/collect/observation"
	gpusignals "github.com/dsx-ai-factory/health-validation/collect/signals/gpu"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCollectValueKindsCoverCanonicalGPU(t *testing.T) {
	canonical := make(map[string]bool)
	for _, signal := range gpusignals.All() {
		id := signal.ID()
		canonical[id] = true
		if kind, ok := collectValueKinds[id]; !ok {
			t.Errorf("canonical signal %q has no declared value kind", id)
		} else {
			switch kind {
			case kindInt, kindDouble, kindNumeric, kindBool:
			default:
				t.Errorf("canonical signal %q has an undefined value kind: %d", id, kind)
			}
		}
	}
	for id := range collectValueKinds {
		if !canonical[id] {
			t.Errorf("value kind declared for non-canonical signal %q", id)
		}
	}
}

func TestCollectObservationDeclaredValueKinds(t *testing.T) {
	tests := []struct {
		signal   collect.Signal
		name     string
		intOK    bool
		doubleOK bool
		boolOK   bool
	}{
		{signal: gpusignals.PerformanceState, name: "int", intOK: true},
		{signal: gpusignals.ComputeUtilization, name: "double", doubleOK: true},
		{signal: gpusignals.PowerDraw, name: "numeric", intOK: true, doubleOK: true},
		{signal: gpusignals.HardwareSlowdownActive, name: "bool", boolOK: true},
	}
	for _, tt := range tests {
		values := []struct {
			value *observation.Value
			name  string
			valid bool
		}{
			{value: &observation.Value{Kind: &observation.Value_IntValue{IntValue: 1}}, name: "int", valid: tt.intOK},
			{value: &observation.Value{Kind: &observation.Value_DoubleValue{DoubleValue: 1}}, name: "double", valid: tt.doubleOK},
			{value: &observation.Value{Kind: &observation.Value_BoolValue{BoolValue: true}}, name: "bool", valid: tt.boolOK},
			{value: &observation.Value{Kind: &observation.Value_StringValue{StringValue: "1"}}, name: "string"},
		}
		for _, value := range values {
			t.Run(tt.name+"/"+value.name, func(t *testing.T) {
				err := validateCollectObservation(valueKindObservation(tt.signal, value.value), tt.signal)
				if value.valid {
					if err != nil {
						t.Fatalf("valid %s value rejected: %v", value.name, err)
					}
				} else {
					want := fmt.Sprintf("signal %q requires a %s value", tt.signal.ID(), tt.name)
					if err == nil || err.Error() != want {
						t.Fatalf("error=%v, want %q", err, want)
					}
				}
			})
		}
	}
}

func TestCollectObservationRejectsUndeclaredCanonicalValueKind(t *testing.T) {
	signal := gpusignals.PerformanceState
	id := signal.ID()
	kind, ok := collectValueKinds[id]
	if !ok {
		t.Fatal("test requires a declared canonical value kind")
	}
	t.Cleanup(func() { collectValueKinds[id] = kind })
	delete(collectValueKinds, id)
	value := &observation.Value{Kind: &observation.Value_IntValue{IntValue: 1}}
	err := validateCollectObservation(valueKindObservation(signal, value), signal)
	want := fmt.Sprintf("signal %q has no declared value kind", id)
	if err == nil || err.Error() != want {
		t.Fatalf("error=%v, want %q", err, want)
	}
}

func TestCollectObservationPrivateSignalNeedsNoValueKind(t *testing.T) {
	signal := collect.NewSignal("private.valid", "")
	value := &observation.Value{Kind: &observation.Value_BoolValue{BoolValue: true}}
	if err := validateCollectObservation(valueKindObservation(signal, value), signal); err != nil {
		t.Fatalf("private signal rejected: %v", err)
	}
}

func valueKindObservation(signal collect.Signal, value *observation.Value) *observation.Observation {
	return &observation.Observation{
		SignalId:   signal.ID(),
		Source:     "test",
		Target:     observation.GPUTarget("GPU-1"),
		ObservedAt: timestamppb.Now(),
		Unit:       signal.Unit(),
		Outcome:    &observation.Observation_Value{Value: value},
	}
}
