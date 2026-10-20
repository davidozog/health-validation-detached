// SPDX-FileCopyrightText: Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package infiniband_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/health-validation/catalog"
	"github.com/dsx-ai-factory/health-validation/catalog/health"
	"github.com/dsx-ai-factory/health-validation/catalog/infiniband"
)

// activeAt builds an ACTIVE/LinkUp native-InfiniBand port at the given rate.
func activeAt(rate float64) infiniband.PortMeasurement {
	m := port("ACTIVE", "LinkUp")
	m.RateGbps = rate
	return m
}

// ethernetAt builds an ACTIVE/LinkUp RoCE port at the given rate.
func ethernetAt(rate float64) infiniband.PortMeasurement {
	m := activeAt(rate)
	m.LinkLayer = "Ethernet"
	return m
}

// archCatalog builds a catalog for one architecture, with any overrides.
func archCatalog(arch string, overrides ...catalog.Override) *catalog.Catalog {
	opts := []catalog.Option{
		catalog.WithArchitecture(arch),
		catalog.WithExecutionContext(health.OperatorInfra),
	}
	if len(overrides) > 0 {
		opts = append(opts, catalog.WithOverrides(catalog.NewOverrideSet(overrides...)))
	}
	return catalog.New(opts...)
}

func rateOverride(name string, value any) catalog.Override {
	return catalog.Override{CheckID: "ib_port_rate", ThresholdName: name, Value: value}
}

func TestPortRateDefinition(t *testing.T) {
	def := infiniband.PortRate()
	if def.ID != "ib_port_rate" || def.Name != "InfiniBand Port Rate" {
		t.Errorf("identity = %q/%q", def.ID, def.Name)
	}
	if def.Category != health.InfiniBand {
		t.Errorf("Category = %v, want InfiniBand", def.Category)
	}
	want := []string{
		infiniband.SignalIBDevice,
		infiniband.SignalIBPort,
		infiniband.SignalIBState,
		infiniband.SignalIBLinkLayer,
		infiniband.SignalIBRateGbps,
	}
	if got := def.RequiredSignals; !slices.Equal(got, want) {
		t.Errorf("RequiredSignals = %v, want %v", got, want)
	}

	// The exact rate is a scalar and the split rates a list; swapping either
	// shape silently changes which rule can ever match.
	if _, isScalar := def.Thresholds["expected_ib_rate_gbps"].Value.(float64); !isScalar {
		t.Errorf("expected_ib_rate_gbps = %T, want float64", def.Thresholds["expected_ib_rate_gbps"].Value)
	}
	if _, isList := def.Thresholds["expected_ib_split_rates_gbps"].Value.([]float64); !isList {
		t.Errorf("expected_ib_split_rates_gbps = %T, want []float64", def.Thresholds["expected_ib_split_rates_gbps"].Value)
	}

	// The split defaults differ by link layer: fabric ports are normally run
	// whole, breakout NICs normally are not.
	if got := def.Thresholds["infiniband_port_split"].Value; got != false {
		t.Errorf("infiniband_port_split = %v, want false", got)
	}
	if got := def.Thresholds["ethernet_port_split"].Value; got != true {
		t.Errorf("ethernet_port_split = %v, want true", got)
	}

	// Every declared architecture must state its own rates, so a new platform
	// cannot silently inherit another's. Registration enforces it; asserting
	// here documents that the omission would be the bug.
	for _, name := range []string{"expected_ib_rate_gbps", "expected_ib_split_rates_gbps"} {
		for _, arch := range def.Architectures {
			if _, ok := def.Thresholds[name].PerArchitecture[arch]; !ok {
				t.Errorf("architecture %q declared but has no %s entry", arch, name)
			}
		}
	}

	// One CRITICAL rule for a rate off the ladder, one WARNING rule for the
	// ambiguous undeclared-split case. The WARNING must keep the node in
	// service, so it overrides the check's drain default with Monitor.
	if len(def.Rules) != 2 {
		t.Fatalf("Rules = %v, want a CRITICAL and a WARNING rule", def.Rules)
	}
	var sawCritical, sawWarning bool
	for _, r := range def.Rules {
		switch r.Severity {
		case health.Critical:
			sawCritical = true
			if r.HasRecommendedAction() {
				t.Errorf("CRITICAL rule overrides the check's drain default with %v", r.RecommendedAction)
			}
		case health.Warning:
			sawWarning = true
			if !r.RecommendedAction.Equal(health.Monitor) {
				t.Errorf("WARNING rule action = %v, want Monitor (the node stays in service)", r.RecommendedAction)
			}
		default:
			t.Errorf("rule %q severity = %v, want Critical or Warning", r.Expression, r.Severity)
		}
	}
	if !sawCritical || !sawWarning {
		t.Errorf("rules critical=%v warning=%v, want both", sawCritical, sawWarning)
	}
}

// Unsplit is the default for native InfiniBand, so the rate must match the
// platform's full port rate exactly: 400 on GB200, 800 on GB300.
func TestPortRate_InfiniBandDefaultsToExactRate(t *testing.T) {
	for _, tc := range []struct {
		arch string
		full float64
	}{
		{"GB200", 400},
		{"GB300", 800},
	} {
		cat := archCatalog(tc.arch)

		v, err := cat.Evaluate("ib_port_rate", activeAt(tc.full))
		if err != nil {
			t.Fatal(err)
		}
		if v.Skipped || v.Severity != health.Pass {
			t.Errorf("%s at %v Gb/s: skipped=%v severity=%v, want PASS", tc.arch, tc.full, v.Skipped, v.Severity)
		}

		// Half the full rate is a legal division, so an unsplit port sitting
		// there is ambiguous: warn and keep the node in service rather than
		// draining a fleet that merely forgot to declare its topology.
		v, err = cat.Evaluate("ib_port_rate", activeAt(tc.full/2))
		if err != nil {
			t.Fatal(err)
		}
		if v.Severity != health.Warning {
			t.Errorf("%s at %v Gb/s unsplit: severity = %v, want WARNING", tc.arch, tc.full/2, v.Severity)
		}
		if !v.RecommendedAction.Equal(health.Monitor) {
			t.Errorf("%s: RecommendedAction = %v, want Monitor", tc.arch, v.RecommendedAction)
		}
		if joined := strings.Join(v.Messages, "\n"); !strings.Contains(joined, "port_split") {
			t.Errorf("%s: messages = %q, want the split threshold named as the remedy", tc.arch, joined)
		}
	}
}

// The undeclared-split warning must not drain: the CLI exits 0 for WARNING,
// so a fleet that forgot the threshold stays in service while the finding is
// still visible in the verdict.
func TestPortRate_UndeclaredSplitDoesNotDrain(t *testing.T) {
	v, err := archCatalog("GB300").Evaluate("ib_port_rate", activeAt(400))
	if err != nil {
		t.Fatal(err)
	}
	if v.Severity >= health.Critical {
		t.Fatalf("severity = %v, want below Critical so the node is not drained", v.Severity)
	}
	if v.RecommendedAction.OperationalState == health.OperationalDrain {
		t.Errorf("OperationalState = drain, want the node kept in service")
	}
}

// Enabling the split knob admits every legal division of the port rate.
func TestPortRate_InfiniBandSplitModeAdmitsTheLadder(t *testing.T) {
	for _, tc := range []struct {
		arch  string
		rates []float64
	}{
		{"GB200", []float64{400, 200, 100}},
		{"GB300", []float64{800, 400, 200, 100}},
	} {
		cat := archCatalog(tc.arch, rateOverride("infiniband_port_split", true))
		for _, rate := range tc.rates {
			v, err := cat.Evaluate("ib_port_rate", activeAt(rate))
			if err != nil {
				t.Fatal(err)
			}
			if v.Skipped || v.Severity != health.Pass {
				t.Errorf("%s split at %v Gb/s: skipped=%v severity=%v, want PASS",
					tc.arch, rate, v.Skipped, v.Severity)
			}
		}
	}
}

// Split mode still rejects a rate no division produces — the residual
// detection it retains.
func TestPortRate_SplitModeRejectsRateOffTheLadder(t *testing.T) {
	for _, arch := range []string{"GB200", "GB300"} {
		cat := archCatalog(arch, rateOverride("infiniband_port_split", true))
		v, err := cat.Evaluate("ib_port_rate", activeAt(56)) // FDR: not a split of either
		if err != nil {
			t.Fatal(err)
		}
		if v.Skipped || v.Severity != health.Critical {
			t.Fatalf("%s split at 56 Gb/s: skipped=%v severity=%v, want CRITICAL", arch, v.Skipped, v.Severity)
		}
		joined := strings.Join(v.Messages, "\n")
		if !strings.Contains(joined, "mlx5_0:1") || !strings.Contains(joined, "56") {
			t.Errorf("%s: messages = %q, want the port and its rate named", arch, joined)
		}
		if !v.RecommendedAction.Equal(health.GenericDrain()) {
			t.Errorf("%s: RecommendedAction = %v, want drain", arch, v.RecommendedAction)
		}
	}
}

// Ethernet defaults the other way: breakout is the common case there, so a
// RoCE port at a legal division passes without any configuration.
func TestPortRate_EthernetDefaultsToSplit(t *testing.T) {
	cat := archCatalog("GB300")
	for _, rate := range []float64{800, 400, 200, 100} {
		v, err := cat.Evaluate("ib_port_rate", ethernetAt(rate))
		if err != nil {
			t.Fatal(err)
		}
		if v.Skipped || v.Severity != health.Pass {
			t.Errorf("Ethernet at %v Gb/s: skipped=%v severity=%v, want PASS by default", rate, v.Skipped, v.Severity)
		}
	}
}

// The same 400 Gb/s reading is a fault on a GB300 fabric port and healthy on
// a GB300 RoCE port, purely because the two default to different modes. This
// is the behavior the per-link-layer knobs exist to produce.
func TestPortRate_LinkLayerSelectsTheMode(t *testing.T) {
	cat := archCatalog("GB300")

	v, err := cat.Evaluate("ib_port_rate", activeAt(400))
	if err != nil {
		t.Fatal(err)
	}
	if v.Severity != health.Warning {
		t.Errorf("GB300 InfiniBand at 400: severity = %v, want WARNING (unsplit expects 800)", v.Severity)
	}

	v, err = cat.Evaluate("ib_port_rate", ethernetAt(400))
	if err != nil {
		t.Fatal(err)
	}
	if v.Severity != health.Pass {
		t.Errorf("GB300 Ethernet at 400: severity = %v, want PASS (split by default)", v.Severity)
	}
}

// A fleet running whole Ethernet ports clears the default and gets exact
// matching there too.
func TestPortRate_EthernetSplitCanBeCleared(t *testing.T) {
	cat := archCatalog("GB300", rateOverride("ethernet_port_split", false))

	v, err := cat.Evaluate("ib_port_rate", ethernetAt(400))
	if err != nil {
		t.Fatal(err)
	}
	if v.Severity != health.Warning {
		t.Errorf("GB300 Ethernet at 400 with split cleared: severity = %v, want WARNING", v.Severity)
	}

	v, err = cat.Evaluate("ib_port_rate", ethernetAt(800))
	if err != nil {
		t.Fatal(err)
	}
	if v.Severity != health.Pass {
		t.Errorf("GB300 Ethernet at 800 with split cleared: severity = %v, want PASS", v.Severity)
	}
}

// Exactly one mode applies per port: the two rules are mutually exclusive, so
// a failing port reports one cause rather than two contradictory ones.
func TestPortRate_ModesAreMutuallyExclusive(t *testing.T) {
	for _, m := range []infiniband.PortMeasurement{activeAt(56), ethernetAt(56)} {
		v, err := archCatalog("GB300").Evaluate("ib_port_rate", m)
		if err != nil {
			t.Fatal(err)
		}
		if len(v.Messages) != 1 {
			t.Errorf("%s port: Messages = %v, want exactly one mode to fire", m.LinkLayer, v.Messages)
		}
	}
}

// A link layer the check knows nothing about is not rated at all, rather than
// being forced through one mode or the other.
func TestPortRate_IgnoresUnknownLinkLayer(t *testing.T) {
	m := activeAt(1)
	m.LinkLayer = "Omni-Path"

	v, err := archCatalog("GB300").Evaluate("ib_port_rate", m)
	if err != nil {
		t.Fatal(err)
	}
	if v.Skipped || v.Severity != health.Pass {
		t.Fatalf("skipped=%v severity=%v, want PASS for an unrecognized link layer", v.Skipped, v.Severity)
	}
}

// A port that is not up reports a rate that means nothing. Failing it here
// too would blame the cable for a fault ib_port_state already attributes.
func TestPortRate_IgnoresInactivePort(t *testing.T) {
	m := port("DOWN", "Polling")
	m.RateGbps = 10

	v, err := ibCatalog().Evaluate("ib_port_rate", m)
	if err != nil {
		t.Fatal(err)
	}
	if v.Skipped || v.Severity != health.Pass {
		t.Fatalf("skipped=%v severity=%v, want PASS: a down port is ib_port_state's finding", v.Skipped, v.Severity)
	}
}

// Operators retune the rates themselves, not just the mode.
func TestPortRate_RateOverrides(t *testing.T) {
	cat := archCatalog("GB200",
		rateOverride("expected_ib_rate_gbps", float64(200)),
		rateOverride("expected_ib_split_rates_gbps", []float64{200, 100}),
	)

	v, err := cat.Evaluate("ib_port_rate", activeAt(200))
	if err != nil {
		t.Fatal(err)
	}
	if v.Severity != health.Pass {
		t.Errorf("200 against an overridden 200 expectation: severity = %v, want PASS", v.Severity)
	}

	// 400 was the stock expectation and is now out of range.
	v, err = cat.Evaluate("ib_port_rate", activeAt(400))
	if err != nil {
		t.Fatal(err)
	}
	if v.Severity != health.Critical {
		t.Errorf("400 against an overridden 200 expectation: severity = %v, want CRITICAL", v.Severity)
	}
}

// sysfs exposed the port but its rate was unreadable: the check must fail on
// the missing signal rather than read the zero value as a dead link.
func TestPortRate_MissingRateSignal(t *testing.T) {
	m := port("ACTIVE", "LinkUp")
	m.RateGbps = 0
	m.AddError(errors.New("rate: permission denied"), infiniband.SignalIBRateGbps)

	v, err := ibCatalog().Evaluate("ib_port_rate", m)
	if err != nil {
		t.Fatal(err)
	}
	if v.Skipped || v.Severity != health.Critical {
		t.Fatalf("skipped=%v severity=%v, want CRITICAL on the unread signal", v.Skipped, v.Severity)
	}
	if !strings.Contains(strings.Join(v.Messages, "\n"), "permission denied") {
		t.Errorf("messages = %v, want the collection failure surfaced", v.Messages)
	}
}
