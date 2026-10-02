// SPDX-FileCopyrightText: Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/dsx-ai-factory/health-validation/catalog/health"
	"github.com/dsx-ai-factory/health-validation/collect"
	"github.com/dsx-ai-factory/health-validation/collect/observation"
	"github.com/dsx-ai-factory/health-validation/toolkit"
	"github.com/dsx-ai-factory/health-validation/toolkit/internal/collectbridge"
	"github.com/google/uuid"
)

// CollectAdapter converts validated observations into one catalog domain.
// Name and Signals are captured once per run; adapter names must be unique.
type CollectAdapter interface {
	Name() string
	Signals() []string
	Build([]*observation.Observation) []health.Measurement
	Errored(error) []health.Measurement
}

// WithCollect adds a caller-owned sensor that provides observation facts.
// Adapters registered with [App.WithCollectAdapters] assemble domain
// measurements across all sensors; the two methods may be called in any order.
// Sources and inventory shared by sensors must be constructed by the caller.
// Only requested signals are collected; each sensor must supply at least one.
// Every selected sensor runs; cadence filters evaluation only. Each registered
// collect source has its own timeout budget.
// Timeout blocking is keyed by sensor ID, not the underlying source. Shared
// NVML/DCGM sources must prevent new calls while a timed-out call is running.
// Late results never replace a cached timeout result.
// Legacy sensors remain registered. Do not feed the same domain through both
// paths; override its legacy backend with an empty provider when an adapter
// supplies that domain's measurements instead.
func (a *App) WithCollect(sensor collect.Sensor) *App {
	a.collectSources = append(a.collectSources, sensor)
	return a
}

// WithCollectAdapters adds adapters that assemble domain measurements from
// facts across all sensors registered with [App.WithCollect]. Calls append to
// the adapter list; names must be unique across calls. Sensor and adapter
// registration order does not matter; both lists are resolved for each run.
// Requested signals without a producer are reported as missing at run time,
// failing only dependent checks rather than causing a registration error.
func (a *App) WithCollectAdapters(adapters ...CollectAdapter) *App {
	a.collectAdapters = append(a.collectAdapters, adapters...)
	return a
}

// collectSensors resolves registered sensors and adapters into toolkit sensors
// sharing one collect cycle for this run. Registration errors and panics are
// returned so the CLI exits 2 before anything is measured.
func (a *App) collectSensors(timeout time.Duration) (sensors []toolkit.Sensor, err error) {
	if len(a.collectSources) == 0 && len(a.collectAdapters) == 0 {
		return nil, nil
	}
	if len(a.collectAdapters) == 0 {
		return nil, fmt.Errorf("collect sensors registered without adapters; register adapters with WithCollectAdapters")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			sensors = nil
			err = fmt.Errorf("collect registration panicked: %v", recovered)
		}
	}()
	cycle, err := collect.NewCycleWithTimeout(uuid.NewString(), time.Now().UTC(), timeout)
	if err != nil {
		return nil, err
	}
	runner := collectbridge.NewRunner(cycle)
	var requested []string
	adapterNames := make(map[string]bool)
	var bindings []collectbridge.Binding
	for _, declared := range a.collectAdapters {
		if isNilCollectInput(declared) {
			return nil, fmt.Errorf("registered collect adapter cannot be nil")
		}
		name, signals := declared.Name(), declared.Signals()
		if strings.TrimSpace(name) == "" || len(signals) == 0 {
			return nil, fmt.Errorf("incomplete collect adapter")
		}
		if adapterNames[name] {
			return nil, fmt.Errorf("collect adapter %q is already registered", name)
		}
		adapterNames[name] = true
		for _, signal := range signals {
			if strings.TrimSpace(signal) == "" {
				return nil, fmt.Errorf("adapter %q requests an empty signal", name)
			}
			requested = append(requested, signal)
		}
		bindings = append(bindings, collectbridge.Binding{
			Name: name, Signals: slices.Clone(signals),
			Build: declared.Build, Errored: declared.Errored,
		})
	}
	for _, source := range a.collectSources {
		if isNilCollectInput(source) {
			return nil, fmt.Errorf("registered collect sensor cannot be nil")
		}
		if err := runner.Register(source, requested...); err != nil {
			return nil, err
		}
	}
	for _, binding := range bindings {
		sensors = append(sensors, runner.Sensor(binding))
	}
	return sensors, nil
}

// isNilCollectInput also rejects typed nils. Toolkit calls Errored outside any
// recover, so a nil adapter whose Name and Signals ignore the receiver would
// pass registration and crash the run. IsNil panics on non-nilable kinds, hence
// the kind check.
func isNilCollectInput(input any) bool {
	if input == nil {
		return true
	}
	value := reflect.ValueOf(input)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
