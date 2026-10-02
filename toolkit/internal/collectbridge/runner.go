// SPDX-FileCopyrightText: Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package collectbridge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dsx-ai-factory/health-validation/catalog/health"
	"github.com/dsx-ai-factory/health-validation/collect"
	"github.com/dsx-ai-factory/health-validation/collect/observation"
	"google.golang.org/protobuf/proto"
)

// Binding captures adapter metadata and callbacks for one collection run.
type Binding struct {
	Build   func([]*observation.Observation) []health.Measurement
	Errored func(error) []health.Measurement
	Name    string
	Signals []string
}

// Runner coordinates one collection cycle. Sensors run sequentially,
// at most once, and each canonical signal has one explicitly selected owner.
// Registration finishes before collection starts. Callers own source lifetime.
type Runner struct {
	cycle   *collect.Cycle
	owner   map[string]*collectEntry
	gate    chan struct{}
	entries []*collectEntry
}

// collectEntry is one registered source and its run-once result cache.
type collectEntry struct {
	sensor    collect.Sensor
	batch     *observation.ObservationBatch
	ready     chan struct{}
	cancel    context.CancelFunc
	id        string
	signals   collect.Signals
	once      sync.Once
	completed sync.Once
}

// NewRunner coordinates sensors under cycle.
func NewRunner(cycle *collect.Cycle) *Runner {
	return &Runner{cycle: cycle, owner: make(map[string]*collectEntry), gate: make(chan struct{}, 1)}
}

// Register assigns the sensor's declared signals matching the requested IDs.
// At least one match is required; overlapping producers are rejected.
func (r *Runner) Register(sensor collect.Sensor, signalIDs ...string) error {
	if len(signalIDs) == 0 {
		return fmt.Errorf("explicit collect signal IDs are required")
	}
	if sensor == nil {
		return fmt.Errorf("collect sensor with a nonempty ID is required")
	}
	id := sensor.ID()
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("collect sensor with a nonempty ID is required")
	}
	if r.cycle == nil || r.cycle.CollectionID() == "" {
		return fmt.Errorf("collection cycle is required")
	}
	for _, e := range r.entries {
		if e.id == id {
			return fmt.Errorf("collect sensor %q is already registered", id)
		}
	}
	requested := make(map[string]bool, len(signalIDs))
	for _, signalID := range signalIDs {
		requested[signalID] = true
	}
	declared := sensor.Signals()
	var selectedIDs []string
	for _, definition := range declared {
		if definition == nil {
			return fmt.Errorf("collect sensor %q declares a nil signal", id)
		}
		if requested[definition.ID()] {
			selectedIDs = append(selectedIDs, definition.ID())
		}
	}
	if len(selectedIDs) == 0 {
		return fmt.Errorf("sensor %q supplies no adapter-requested signals", id)
	}
	op := collect.NewOp(collect.WithSignalID(selectedIDs...))
	selected, err := op.Signals(declared)
	if err != nil {
		return fmt.Errorf("register collect sensor %q: %w", id, err)
	}
	owned := make(collect.Signals, 0, len(selected))
	for _, s := range selected {
		if err := validateCollectDefinition(s); err != nil {
			return err
		}
		if previous := r.owner[s.ID()]; previous != nil {
			return fmt.Errorf("signal %q is produced by both %q and %q; select one source explicitly", s.ID(), previous.id, id)
		}
		definition := runnerSignal{id: s.ID()}
		if unit := s.Unit(); unit != nil {
			copied := *unit
			definition.unit = &copied
		}
		owned = append(owned, definition)
	}
	e := &collectEntry{sensor: sensor, id: id, signals: owned, ready: make(chan struct{})}
	for _, s := range owned {
		r.owner[s.ID()] = e
	}
	r.entries = append(r.entries, e)
	return nil
}

// Observe returns independently owned observations for the requested signals.
// Unknown signals produce NOT_CONFIGURED errors; successful empty collections
// remain empty so the adapter can apply its own absence semantics.
func (r *Runner) Observe(ctx context.Context, signalIDs ...string) []*observation.Observation {
	want := make(map[string]bool, len(signalIDs))
	var out []*observation.Observation
	for _, id := range signalIDs {
		if want[id] {
			continue
		}
		want[id] = true
		if r.owner[id] == nil {
			s := collect.NewSignal(id, "")
			if canonical := canonicalGPU[id]; canonical != nil {
				s = canonical
			}
			failure := collectFailure(s, "toolkit", fmt.Errorf("no collect sensor is registered for signal %q", id))
			failure.GetCollectionError().Category = observation.CollectionErrorCategoryNotConfigured
			out = append(out, failure)
		}
	}
	for _, e := range r.entries {
		needed := false
		for _, s := range e.signals {
			if want[s.ID()] {
				needed = true
				break
			}
		}
		if !needed {
			continue
		}
		for _, o := range r.observe(ctx, e).GetObservations() {
			if want[o.GetSignalId()] {
				out = append(out, proto.Clone(o).(*observation.Observation))
			}
		}
	}
	return out
}

// observe starts e at most once and waits for its batch. If ctx expires first,
// the failure is cached and the source canceled, so a late result can never
// replace it.
func (r *Runner) observe(ctx context.Context, e *collectEntry) *observation.ObservationBatch {
	e.once.Do(func() {
		callCtx, cancel := context.WithCancel(ctx)
		e.cancel = cancel
		go func() {
			defer cancel()
			batch := r.measure(callCtx, e)
			if callCtx.Err() != nil {
				batch = r.failed(e, callCtx.Err())
			}
			e.complete(batch)
		}()
	})
	select {
	case <-e.ready:
		return e.batch
	case <-ctx.Done():
		e.complete(r.failed(e, ctx.Err()))
		e.cancel()
		return e.batch
	}
}

// complete caches batch if e has no result yet; the first completion wins.
func (e *collectEntry) complete(batch *observation.ObservationBatch) {
	e.completed.Do(func() { e.batch = batch; close(e.ready) })
}

// measure runs e under the sequential gate and validates its batch as a whole.
// A top-level source error replaces untargeted successes for the same signal
// and is appended for signals the batch omitted, keeping valid partial data.
func (r *Runner) measure(ctx context.Context, e *collectEntry) (batch *observation.ObservationBatch) {
	defer func() {
		if recovered := recover(); recovered != nil {
			batch = r.failed(e, fmt.Errorf("collect sensor %q panicked: %v", e.id, recovered))
		}
	}()
	select {
	case r.gate <- struct{}{}:
	case <-ctx.Done():
		return r.failed(e, ctx.Err())
	}
	defer func() { <-r.gate }()
	batch, sourceErr := collect.Run(ctx, r.cycle, e.id, func(ctx context.Context) (*observation.ObservationBatch, error) {
		return e.sensor.Measure(ctx, collect.WithCycle(r.cycle), collect.WithSignalID(e.signals.IDs()...))
	})
	if batch == nil {
		return r.failed(e, errors.Join(sourceErr, errors.New("observation batch is required")))
	}
	batch = proto.Clone(batch).(*observation.ObservationBatch)
	if err := validateCollectBatch(batch, r.cycle.CollectionID(), e.signals); err != nil {
		return r.failed(e, fmt.Errorf("collect sensor %q: %w", e.id, err))
	}
	if sourceErr != nil {
		for _, failure := range r.failed(e, sourceErr).GetObservations() {
			found := false
			for i, current := range batch.Observations {
				if current.GetSignalId() == failure.GetSignalId() && current.GetTarget() == nil {
					if current.GetCollectionError() == nil {
						batch.Observations[i] = failure
					}
					found = true
					break
				}
			}
			if !found {
				batch.Observations = append(batch.Observations, failure)
			}
		}
	}
	return batch
}

func (r *Runner) failed(e *collectEntry, err error) *observation.ObservationBatch {
	batch := &observation.ObservationBatch{CollectionId: r.cycle.CollectionID()}
	for _, s := range e.signals {
		batch.Observations = append(batch.Observations, collectFailure(s, e.id, err))
	}
	return batch
}

// collectFailure builds a source-scoped error observation, classifying both
// collect and toolkit deadline errors as timeouts.
func collectFailure(s collect.Signal, source string, err error) *observation.Observation {
	o := observation.NewErrorObservation(s.ID(), s.Unit(), source, "Measure", err)
	if errors.Is(err, collect.ErrCollectionTimeout) || errors.Is(err, health.ErrCollectionTimeout) {
		o.GetCollectionError().Category = observation.CollectionErrorCategoryTimeout
	}
	return o
}

// Sensor exposes a binding through the toolkit's collection interface.
func (r *Runner) Sensor(binding Binding) *Sensor {
	binding.Signals = slices.Clone(binding.Signals)
	return &Sensor{runner: r, binding: binding}
}

// Sensor adapts cached observations to the toolkit collection interface.
type Sensor struct {
	runner  *Runner
	binding Binding
}

func (s *Sensor) Name() string { return s.binding.Name }

func (s *Sensor) Measure(ctx context.Context) ([]health.Measurement, error) {
	return s.binding.Build(s.runner.Observe(ctx, s.binding.Signals...)), nil
}

func (s *Sensor) Errored(err error) []health.Measurement { return s.binding.Errored(err) }

// Prepare fills the shared cache under each source's timeout budget. It
// prepares every registered source, not just this binding's; the run-once
// cache makes later calls no-ops.
func (s *Sensor) Prepare(timeout time.Duration) {
	for _, e := range s.runner.entries {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		s.runner.observe(ctx, e)
		cancel()
	}
}

// runnerSignal snapshots a declared signal so a producer cannot change its ID
// or unit after registration.
type runnerSignal struct {
	unit *string
	id   string
}

func (s runnerSignal) ID() string { return s.id }

// Unit returns the snapshotted unit; nil and an empty unit are distinct.
func (s runnerSignal) Unit() *string { return s.unit }
