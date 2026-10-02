// SPDX-FileCopyrightText: Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package collectbridge

import gpusignals "github.com/dsx-ai-factory/health-validation/collect/signals/gpu"

type collectValueKind uint8

const (
	kindInt collectValueKind = iota
	kindDouble
	kindNumeric
	kindBool
)

func (kind collectValueKind) String() string {
	switch kind {
	case kindInt:
		return "int"
	case kindDouble:
		return "double"
	case kindNumeric:
		return "numeric"
	case kindBool:
		return "bool"
	default:
		return "unknown"
	}
}

// Every canonical signal needs an explicit kind, independent of its name.
var collectValueKinds = map[string]collectValueKind{
	gpusignals.PowerDraw.ID():                           kindNumeric,
	gpusignals.PowerLimitRequested.ID():                 kindNumeric,
	gpusignals.PowerLimitEnforced.ID():                  kindNumeric,
	gpusignals.PowerViolationDuration.ID():              kindInt,
	gpusignals.ReliabilityViolationDuration.ID():        kindInt,
	gpusignals.BoardLimitViolationDuration.ID():         kindInt,
	gpusignals.PerformanceState.ID():                    kindInt,
	gpusignals.TemperatureCelsius.ID():                  kindInt,
	gpusignals.TemperatureHBMCelsius.ID():               kindInt,
	gpusignals.TemperatureSlowdownMarginCelsius.ID():    kindInt,
	gpusignals.TemperatureThresholdShutdownCelsius.ID(): kindInt,
	gpusignals.TemperatureThresholdSlowdownCelsius.ID(): kindInt,
	gpusignals.TemperatureThresholdMemMaxCelsius.ID():   kindInt,
	gpusignals.TemperatureThresholdGPUMaxCelsius.ID():   kindInt,
	gpusignals.ThermalViolationDuration.ID():            kindInt,
	gpusignals.SMClock.ID():                             kindInt,
	gpusignals.MemoryClock.ID():                         kindInt,
	gpusignals.ClockEventReasons.ID():                   kindInt,
	gpusignals.HardwareSlowdownActive.ID():              kindBool,
	gpusignals.HardwareThermalSlowdownActive.ID():       kindBool,
	gpusignals.HardwarePowerBrakeSlowdownActive.ID():    kindBool,
	gpusignals.ComputeUtilization.ID():                  kindDouble,
	gpusignals.MemoryCopyUtilization.ID():               kindInt,
	gpusignals.FramebufferFree.ID():                     kindInt,
	gpusignals.FramebufferUsed.ID():                     kindInt,
	gpusignals.FramebufferTotal.ID():                    kindInt,
	gpusignals.FramebufferUsedRatio.ID():                kindDouble,
	gpusignals.UncorrectableRemappedRows.ID():           kindInt,
	gpusignals.CorrectableRemappedRows.ID():             kindInt,
	gpusignals.RowRemapFailure.ID():                     kindInt,
	gpusignals.RowRemapPending.ID():                     kindInt,
	gpusignals.ECCSingleBitVolatileTotal.ID():           kindInt,
	gpusignals.ECCDoubleBitVolatileTotal.ID():           kindInt,
	gpusignals.ECCSingleBitAggregateTotal.ID():          kindInt,
	gpusignals.ECCDoubleBitAggregateTotal.ID():          kindInt,
	gpusignals.ECCSingleBitVolatileDevice.ID():          kindInt,
	gpusignals.ECCDoubleBitVolatileDevice.ID():          kindInt,
	gpusignals.ECCSingleBitAggregateDevice.ID():         kindInt,
	gpusignals.ECCDoubleBitAggregateDevice.ID():         kindInt,
	gpusignals.RemapRowsAvailableHigh.ID():              kindInt,
	gpusignals.RemapRowsAvailableLow.ID():               kindInt,
	gpusignals.RemapRowsAvailableMax.ID():               kindInt,
	gpusignals.RemapRowsAvailableNone.ID():              kindInt,
	gpusignals.RemapRowsAvailablePartial.ID():           kindInt,
	gpusignals.PCIeReplayCount.ID():                     kindInt,
	gpusignals.NVLinkThroughputTotal.ID():               kindInt,
	gpusignals.NVLinkDLCrcErrorCount.ID():               kindInt,
	gpusignals.NVLinkDLRecoveryErrorCount.ID():          kindInt,
	gpusignals.NVLinkDLReplayErrorCount.ID():            kindInt,
	gpusignals.NVLinkRecoverySuccessfulCount.ID():       kindInt,
	gpusignals.NVLinkRecoveryFailedCount.ID():           kindInt,
	gpusignals.FabricManagerStatus.ID():                 kindInt,
	gpusignals.NVLinkFabricBandwidthDegraded.ID():       kindBool,
	gpusignals.NVLinkFabricRouteRecoveryActive.ID():     kindBool,
	gpusignals.NVLinkFabricRouteUnhealthy.ID():          kindBool,
	gpusignals.NVLinkFabricAccessTimeoutRecovery.ID():   kindBool,
	gpusignals.C2CLinkReplayErrorCount.ID():             kindInt,
	gpusignals.NVLinkRXGeneralErrorCount.ID():           kindInt,
	gpusignals.NVLinkRXErrorCount.ID():                  kindInt,
	gpusignals.NVLinkRXMalformedPacketErrorCount.ID():   kindInt,
	gpusignals.NVLinkRXRemoteErrorCount.ID():            kindInt,
	gpusignals.NVLinkRXSymbolErrorCount.ID():            kindInt,
	gpusignals.NVLinkRXBufferOverrunErrorCount.ID():     kindInt,
	gpusignals.NVLinkLocalIntegrityErrorCount.ID():      kindInt,
	gpusignals.NVLinkEffectiveErrorCount.ID():           kindInt,
	gpusignals.NVLinkEffectiveBER.ID():                  kindDouble,
	gpusignals.NVLinkSymbolBER.ID():                     kindDouble,
	gpusignals.NVLinkTXDiscardCount.ID():                kindInt,
	gpusignals.InforomConfigurationValid.ID():           kindInt,
}
