package xid_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/health-validation/catalog"
	"github.com/dsx-ai-factory/health-validation/catalog/health"
	"github.com/dsx-ai-factory/health-validation/catalog/xid"
)

// hostCatalog builds the Xid catalog for a host running driverVersion,
// the way the app hands the version it discovers to the catalog.
func hostCatalog(t *testing.T, driverVersion string) *catalog.Catalog {
	t.Helper()
	v, ok := health.ParseDriverVersion(driverVersion)
	if !ok {
		t.Fatalf("ParseDriverVersion(%q) failed", driverVersion)
	}
	return xidCatalog(t, catalog.WithDriverVersion(v))
}

func xidCatalog(t *testing.T, opts ...catalog.Option) *catalog.Catalog {
	t.Helper()
	cat := catalog.NewEmpty(append([]catalog.Option{
		catalog.WithArchitecture("GB200"),
		catalog.WithExecutionContext(health.OperatorInfra),
	}, opts...)...)
	for _, cd := range xid.Checks() {
		cat.MustRegister(cd)
	}
	return cat
}

// measurementFor builds an XidMeasurement directly from raw Xid codes —
// the catalog-native way, with no dmesg parsing. Parsing "NVRM: Xid …"
// log lines is the toolkit's concern (toolkit/xid.ParseXid) and is tested
// there; the catalog only needs the aggregated XidCounts/EventCount.
func measurementFor(codes ...int64) xid.XidMeasurement {
	m := xid.XidMeasurement{Since: "boot"}
	for _, code := range codes {
		m.Events = append(m.Events, xid.XidEvent{PCIAddress: "0000:01:00", XidCode: code, Pid: 1, ProcessName: "app"})
		if m.XidCounts == nil {
			m.XidCounts = make(map[int64]int64)
		}
		m.XidCounts[code]++
		m.EventCount++
	}
	return m
}

func evalXid(t *testing.T, codes ...int64) health.NodeVerdict {
	t.Helper()
	nv, err := xidCatalog(t).EvaluateAll(measurementFor(codes...))
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	return nv
}

func TestXidErrors_SeverityByBucket(t *testing.T) {
	tests := []struct {
		name string
		code int64
		want health.Severity
	}{
		{"reset_gpu (46)", 46, health.Critical},
		{"fell off bus (79)", 79, health.Critical},
		{"memory remapping failure (64)", 64, health.Critical},
		{"double bit ECC error (48)", 48, health.Critical},
		{"app fault (11)", 11, health.Warning},
		{"graphics engine exception (13)", 13, health.Warning},
		{"memory page fault (31)", 31, health.Warning},
		{"vgpu start (78)", 78, health.Warning},
		{"preemptive removal (45)", 45, health.Warning},
		{"recovery action changed (154)", 154, health.Info},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nv := evalXid(t, tc.code)
			if nv.OverallSeverity != tc.want {
				t.Errorf("code %d: OverallSeverity = %v, want %v", tc.code, nv.OverallSeverity, tc.want)
			}
		})
	}
}

func TestXidErrors_WarningActionByBucket(t *testing.T) {
	tests := []struct {
		name string
		want health.RecommendedAction
		code int64
	}{
		{"app fault (13)", health.Drain(health.RemediationActionRestartApp, health.RemediationActionServiceRequired), 13},
		{"memory page fault (31)", health.Drain(health.RemediationActionRestartApp, health.RemediationActionServiceRequired), 31},
		{"preemptive removal (45)", health.Drain(health.RemediationActionWorkflowXid45), 45},
		{"vgpu start (78)", health.Drain(health.RemediationActionUpdateSWFW), 78},
		{"key rotation (151)", health.Drain(health.RemediationActionRestartOS), 151},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := checkResult(evalXid(t, tc.code), "gpu_xid_warning")
			if !ok {
				t.Fatalf("gpu_xid_warning verdict not found")
			}
			if !v.RecommendedAction.Equal(tc.want) {
				t.Errorf("code %d: RecommendedAction = %v, want %v", tc.code, v.RecommendedAction, tc.want)
			}
		})
	}
}

// nvlink5MeasurementFor builds an XidMeasurement for one NVLink5-family
// event (144-150), the way toolkit/xid.ParseXid would after decoding the
// extended Fatal/Nonfatal log format.
func nvlink5MeasurementFor(code int64, fatal bool) xid.XidMeasurement {
	m := xid.XidMeasurement{
		Since:     "boot",
		XidCounts: map[int64]int64{code: 1},
		Events: []xid.XidEvent{{
			PCIAddress: "0000:01:00",
			XidCode:    code,
			NVLink5:    &xid.NVLink5Detail{SubComponent: "NETIR_LINK_EVT", Fatal: fatal, Link: 8},
		}},
		EventCount: 1,
	}
	if fatal {
		m.FatalXidCounts = map[int64]int64{code: 1}
	}
	return m
}

func TestXidErrors_NVLink5_FatalIsCritical(t *testing.T) {
	nv, err := xidCatalog(t).EvaluateAll(nvlink5MeasurementFor(146, true))
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	if nv.OverallSeverity != health.Critical {
		t.Errorf("OverallSeverity = %v, want CRITICAL", nv.OverallSeverity)
	}
	v, ok := checkResult(nv, "gpu_xid_146")
	if !ok {
		t.Fatalf("gpu_xid_146 verdict not found")
	}
	if v.Severity != health.Critical {
		t.Errorf("gpu_xid_146 severity = %v, want CRITICAL", v.Severity)
	}
	if want := health.Drain(health.RemediationActionResetGPU, health.RemediationActionServiceRequired); !v.RecommendedAction.Equal(want) {
		t.Errorf("RecommendedAction = %v, want %v", v.RecommendedAction, want)
	}
}

// TestXidErrors_NVLink5_NonfatalIsInfoNoAction pins what a Nonfatal
// occurrence costs the node: nothing. The decode table resolves it to
// IGNORE, so it is reported at INFO (which exits 0) rather than drawing
// a WARNING the operator cannot act on.
func TestXidErrors_NVLink5_NonfatalIsInfoNoAction(t *testing.T) {
	nv, err := xidCatalog(t).EvaluateAll(nvlink5MeasurementFor(146, false))
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	if nv.OverallSeverity != health.Info {
		t.Errorf("OverallSeverity = %v, want INFO", nv.OverallSeverity)
	}
	if nv.ExitCode() != 0 {
		t.Errorf("ExitCode = %d, want 0: an ignorable fault must not fail the run", nv.ExitCode())
	}
	v, ok := checkResult(nv, "gpu_xid_146")
	if !ok {
		t.Fatalf("gpu_xid_146 verdict not found")
	}
	if v.Severity != health.Info {
		t.Errorf("gpu_xid_146 severity = %v, want INFO", v.Severity)
	}
	if !v.RecommendedAction.Equal(health.NoAction) {
		t.Errorf("RecommendedAction = %v, want NoAction", v.RecommendedAction)
	}
}

// nvlink5SubXidCodeMeasurementFor builds an XidMeasurement for one Xid 149
// NETIR_LINK_EVT/NETIR_LINK_DOWN event carrying the given subcode, the way
// toolkit/xid.ParseXid would after decoding the extended log format.
// intrInfoForSubXidCode builds an IntrInfo register value whose
// fault-subtype bits (IntrInfo[25:20]) encode subcode's digit, e.g.
// "149.4" -> "0x00400000", so a test measurement carries the raw field
// a sensor would and sub_xid_code_counts is derived from it.
func intrInfoForSubXidCode(subcode string) string {
	_, digit, _ := strings.Cut(subcode, ".")
	v, err := strconv.ParseUint(digit, 16, 8)
	if err != nil {
		panic("intrInfoForSubXidCode: bad subcode " + subcode)
	}
	return fmt.Sprintf("0x%08x", v<<20)
}

func nvlink5SubXidCodeMeasurementFor(subcode string, fatal bool) xid.XidMeasurement {
	m := xid.XidMeasurement{
		Since:     "boot",
		XidCounts: map[int64]int64{149: 1},
		Events: []xid.XidEvent{{
			PCIAddress: "0000:01:00",
			XidCode:    149,
			NVLink5: &xid.NVLink5Detail{
				SubComponent: "NETIR_LINK_EVT",
				IntrInfo:     intrInfoForSubXidCode(subcode),
				Fatal:        fatal,
				Link:         8,
			},
		}},
		EventCount: 1,
	}
	if fatal {
		m.FatalXidCounts = map[int64]int64{149: 1}
	}
	return m
}

func TestXidErrors_NVLink5SubXidCode_Critical(t *testing.T) {
	nv, err := xidCatalog(t).EvaluateAll(nvlink5SubXidCodeMeasurementFor("149.4", true))
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	v, ok := checkResult(nv, "gpu_xid_149_4")
	if !ok {
		t.Fatalf("gpu_xid_149_4 verdict not found")
	}
	if v.Severity != health.Critical {
		t.Errorf("gpu_xid_149_4 severity = %v, want CRITICAL", v.Severity)
	}
	if want := health.Drain(health.RemediationActionResetGPU, health.RemediationActionServiceRequired); !v.RecommendedAction.Equal(want) {
		t.Errorf("RecommendedAction = %v, want %v", v.RecommendedAction, want)
	}
}

func TestXidErrors_NVLink5SubXidCode_Warning(t *testing.T) {
	// Subcode 149.26 is the one row whose decode-table resolution is
	// IGNORE rather than RESET_GPU.
	nv, err := xidCatalog(t).EvaluateAll(nvlink5SubXidCodeMeasurementFor("149.26", false))
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	v, ok := checkResult(nv, "gpu_xid_149_26")
	if !ok {
		t.Fatalf("gpu_xid_149_26 verdict not found")
	}
	if v.Severity != health.Warning {
		t.Errorf("gpu_xid_149_26 severity = %v, want WARNING", v.Severity)
	}
	if !v.RecommendedAction.Equal(health.NoAction) {
		t.Errorf("RecommendedAction = %v, want NoAction", v.RecommendedAction)
	}
}

func TestXidErrors_NVLink5SubXidCode_AdditiveWithBaseline(t *testing.T) {
	// A "149.4" Fatal event fires both the per-subcode check and the
	// baseline gpu_xid_149 Fatal/Nonfatal check: subcode checks are
	// additive, not a replacement.
	nv, err := xidCatalog(t).EvaluateAll(nvlink5SubXidCodeMeasurementFor("149.4", true))
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	if _, ok := checkResult(nv, "gpu_xid_149"); !ok {
		t.Errorf("gpu_xid_149 (baseline) verdict not found")
	}
	if _, ok := checkResult(nv, "gpu_xid_149_4"); !ok {
		t.Errorf("gpu_xid_149_4 (subcode) verdict not found")
	}
}

func checkResult(nv health.NodeVerdict, checkID string) (health.CheckVerdict, bool) {
	for _, v := range nv.CheckResults {
		if v.CheckID == checkID {
			return v, true
		}
	}
	return health.CheckVerdict{}, false
}

func TestXidErrors_IgnoredCode_Pass(t *testing.T) {
	// Xid 63 (memory remapping event) has immediate action IGNORE and is
	// not in any bucket, so it must not fire.
	nv := evalXid(t, 63)
	if nv.OverallSeverity != health.Pass {
		t.Errorf("ignored code 63: OverallSeverity = %v, want PASS", nv.OverallSeverity)
	}
}

func TestXidErrors_NoEvents_Pass(t *testing.T) {
	nv := evalXid(t)
	if nv.OverallSeverity != health.Pass {
		t.Errorf("clean boot: OverallSeverity = %v, want PASS", nv.OverallSeverity)
	}
}

func TestXidErrors_WorstSeverityWins(t *testing.T) {
	// An app fault (WARNING) alongside a reset-class fault (CRITICAL)
	// aggregates to CRITICAL.
	nv := evalXid(t, 11, 79)
	if nv.OverallSeverity != health.Critical {
		t.Errorf("OverallSeverity = %v, want CRITICAL", nv.OverallSeverity)
	}
}

func TestXidErrors_PerCodeCriticalCheck(t *testing.T) {
	// Xid 79 fires its own gpu_xid_79 check (CRITICAL, reset+escalate); every
	// other Xid check runs against the same measurement but passes.
	nv := evalXid(t, 79)

	var critical []health.CheckVerdict
	for _, v := range nv.CheckResults {
		if v.Severity == health.Critical {
			critical = append(critical, v)
		}
	}
	if len(critical) != 1 {
		t.Fatalf("got %d CRITICAL verdicts, want 1 (only gpu_xid_79)", len(critical))
	}
	if critical[0].CheckID != "gpu_xid_79" {
		t.Errorf("CheckID = %q, want gpu_xid_79", critical[0].CheckID)
	}
	if want := health.Drain(health.RemediationActionResetGPU, health.RemediationActionServiceRequired); !critical[0].RecommendedAction.Equal(want) {
		t.Errorf("RecommendedAction = %v, want %v", critical[0].RecommendedAction, want)
	}
}

// TestXidErrors_NVLink5_FatalDoesNotAlsoReportNonfatal guards the
// exclusion in the Nonfatal rule. Rules are not first-match-wins: every
// rule whose expression matches contributes its message, and a Fatal
// occurrence is in xid_counts as well as fatal_xid_counts. Without the
// exclusion the verdict carries both messages, telling a reader the same
// fault is Fatal and Nonfatal. Severity stays CRITICAL either way, so
// asserting on severity alone would not catch this.
func TestXidErrors_NVLink5_FatalDoesNotAlsoReportNonfatal(t *testing.T) {
	nv, err := xidCatalog(t).EvaluateAll(nvlink5MeasurementFor(146, true))
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	v, ok := checkResult(nv, "gpu_xid_146")
	if !ok {
		t.Fatalf("gpu_xid_146 verdict not found")
	}

	if len(v.Messages) != 1 {
		t.Errorf("Messages = %q, want exactly one", v.Messages)
	}
	for _, msg := range v.Messages {
		if strings.Contains(msg, "(Nonfatal)") {
			t.Errorf("Fatal occurrence reported as Nonfatal: %q", msg)
		}
	}
}

// TestXidErrors_NVLink5_NonfatalStillReports is the other half of not
// escalating: the event must still appear in the verdict. Dropping the
// rule entirely would silence it, leaving no record that the fault
// occurred at all.
func TestXidErrors_NVLink5_NonfatalStillReports(t *testing.T) {
	nv, err := xidCatalog(t).EvaluateAll(nvlink5MeasurementFor(146, false))
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	v, ok := checkResult(nv, "gpu_xid_146")
	if !ok {
		t.Fatalf("gpu_xid_146 verdict not found")
	}

	if v.Severity != health.Info {
		t.Errorf("Severity = %v, want INFO", v.Severity)
	}
	if len(v.Messages) != 1 || !strings.Contains(v.Messages[0], "no action") {
		t.Errorf("Messages = %q, want one message stating no action is needed", v.Messages)
	}
}
