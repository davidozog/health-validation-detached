// SPDX-FileCopyrightText: Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package infiniband

import "github.com/dsx-ai-factory/health-validation/catalog/health"

// PortRate detects a port that came up at the wrong rate — a link that
// negotiated down, silently costing bandwidth without ever appearing as an
// outage.
//
// What "wrong" means depends on whether the port is split. An adapter's full
// rate can be divided into several logical ports, and the link speed tracks
// the division: a GB300's 800 Gb/s presents as 1x800, 2x400, 4x200, or 8x100,
// and a GB200's 400 as 1x400, 2x200, or 4x100. The check therefore runs in one
// of two modes per port:
//
//   - Unsplit (exact): the rate must equal expected_ib_rate_gbps — 400 on
//     GB200, 800 on GB300. This is the strict mode, and it catches a link that
//     fell back one step.
//   - Split (membership): the rate must be one of expected_ib_split_rates_gbps.
//     This admits every legal division, so it cannot tell a split from a
//     degraded link — 400 on a GB300 may be half of a 2x split or a failed
//     800. It still rejects a rate no division produces.
//
// Severity follows how confident the check can be that it is looking at a
// fault rather than a configuration gap:
//
//   - Critical, in either mode, when the rate is outside the ladder entirely.
//     No supported division produces it, so the link is genuinely wrong.
//   - Warning, in unsplit mode only, when the rate is a legal division of the
//     expected one. That is the ambiguous case: far more often a fleet that
//     split its ports without declaring it than a link that fell back. Warning
//     keeps the node in service (the CLI exits 0 for it) and recommends
//     Monitor, because draining a whole fleet over an undeclared topology
//     would cost more than the degradation it would be reporting.
//
// The mode is chosen by link layer, because the two are configured
// differently in practice: native InfiniBand fabric ports are normally run
// whole, while the Ethernet/RoCE NICs alongside them are commonly broken out.
// Hence infiniband_port_split defaults false and ethernet_port_split defaults
// true. A fleet that splits its fabric ports sets the former; one that runs
// whole Ethernet ports clears the latter. Both are per-link-layer rather than
// one shared knob precisely so the two defaults can differ.
//
// The link-layer names are matched literally because they select which knob
// applies — they are sysfs's vocabulary, not a fleet-tunable value. A port
// reporting some third link layer is not rated at all.
//
// The rule is also scoped to ports already in the expected logical state, so a
// down port fails [PortState] alone rather than both checks: sysfs still
// reports a rate for a port that is not up, and reporting it as a degraded link
// would misattribute the fault. expected_ib_state is therefore declared here
// too, and must be overridden alongside [PortState]'s copy if a fleet changes
// it.
func PortRate() health.CheckDefinition {
	return health.CheckDefinition{
		ID:          "ib_port_rate",
		Name:        "InfiniBand Port Rate",
		Description: "Verifies every active InfiniBand or RoCE port reported by sysfs negotiated its expected rate: exactly expected_ib_rate_gbps when the port is unsplit, or one of expected_ib_split_rates_gbps when it is. Fails CRITICAL on a rate no supported division produces, and WARNING on an unsplit port sitting on a legal division — most often an undeclared split rather than a fault.",
		Category:    health.InfiniBand,

		RequiredSignals: []string{
			SignalIBDevice,
			SignalIBPort,
			SignalIBState,
			SignalIBLinkLayer,
			SignalIBRateGbps,
		},

		ExecutionClass:    health.Passive,
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra, health.TenantGuest},
		SchedulingCadence: health.Continuous,
		Architectures:     []string{"GB200", "GB300"},

		ConditionScope:    health.ClusterScope,
		Provenance:        health.NVIDIA,
		MaturityLevel:     health.Experimental,
		RecommendedAction: health.GenericDrain(),

		Thresholds: map[string]health.ThresholdSpec{
			// Both rate thresholds are per-generation, because the rates track the
			// platform's adapter. Adding an architecture is what forces its rates to
			// be stated: registration rejects a declared architecture missing from
			// either map, so a new platform cannot silently inherit another's.
			"expected_ib_rate_gbps": {
				Value: float64(400),
				PerArchitecture: map[string]any{
					"GB200": float64(400),
					"GB300": float64(800),
				},
				Category:    health.DefaultThreshold,
				Description: "Rate, in Gb/s, an active unsplit port must report exactly. GB200 ships NDR (400); GB300 ships XDR (800). Applies to a link layer whose port_split threshold is false.",
				Unit:        "Gb/s",
			},
			"expected_ib_split_rates_gbps": {
				Value: []float64{400, 200, 100},
				PerArchitecture: map[string]any{
					"GB200": []float64{400, 200, 100},
					"GB300": []float64{800, 400, 200, 100},
				},
				Category:    health.DefaultThreshold,
				Description: "Rates, in Gb/s, an active split port may report; anything else fails. Each is the adapter's full rate divided by a supported number of ports. Applies to a link layer whose port_split threshold is true.",
				Unit:        "Gb/s",
			},
			"infiniband_port_split": {
				Value:       false,
				Category:    health.DefaultThreshold,
				Description: "Whether native InfiniBand ports are split into multiple logical ports. False (the default) rates them against expected_ib_rate_gbps exactly; true admits any rate in expected_ib_split_rates_gbps. Set true for a fleet that breaks out its fabric ports.",
				Unit:        "boolean",
			},
			"ethernet_port_split": {
				Value:       true,
				Category:    health.DefaultThreshold,
				Description: "Whether Ethernet/RoCE ports are split into multiple logical ports. True (the default, since breakout is the common case there) admits any rate in expected_ib_split_rates_gbps; false requires expected_ib_rate_gbps exactly.",
				Unit:        "boolean",
			},
			"expected_ib_state": {
				Value:       "ACTIVE",
				Category:    health.DefaultThreshold,
				Description: "Logical port state a port must be in for its rate to be assessed, as sysfs names it. Keep in step with the ib_port_state threshold of the same name.",
				Unit:        "state",
			},
		},

		Rules: []health.Rule{
			// A rate outside the ladder entirely is a hardware fault in either
			// mode: no division of the port rate produces it, so the link did
			// not come up the way any supported configuration would.
			{
				Expression: `ib_state == expected_ib_state && ` +
					`(ib_link_layer == "InfiniBand" || ib_link_layer == "Ethernet") && ` +
					`!(ib_rate_gbps in expected_ib_split_rates_gbps)`,
				Severity: health.Critical,
				Message:  "Port ${ib_device}:${ib_port} (${ib_link_layer}): negotiated ${ib_rate_gbps} Gb/s, which is not a supported rate for this platform (expected one of ${expected_ib_split_rates_gbps} Gb/s).",
			},
			// An unsplit port sitting on a legal division is far more often a
			// fleet that split its ports without saying so than a link that
			// fell back — and draining every node for an undeclared topology is
			// worse than the degradation it would be reporting. Warn and keep
			// the node in service; the operator either sets the threshold or
			// investigates.
			{
				Expression: `ib_state == expected_ib_state && ` +
					`((ib_link_layer == "InfiniBand" && !infiniband_port_split) || (ib_link_layer == "Ethernet" && !ethernet_port_split)) && ` +
					`ib_rate_gbps != expected_ib_rate_gbps && ` +
					`ib_rate_gbps in expected_ib_split_rates_gbps`,
				Severity:          health.Warning,
				RecommendedAction: health.Monitor,
				Message:           "Port ${ib_device}:${ib_port} (${ib_link_layer}): negotiated ${ib_rate_gbps} Gb/s rather than the full ${expected_ib_rate_gbps} Gb/s. That is a supported division of the port rate, so this port is most likely broken out without its link layer's port_split threshold set — set it to confirm, or investigate a link that fell back.",
			},
		},
	}
}
