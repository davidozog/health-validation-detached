package toolkit_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/health-validation/catalog"
	"github.com/dsx-ai-factory/health-validation/catalog/health"
	"github.com/dsx-ai-factory/health-validation/catalog/infiniband"
	"github.com/dsx-ai-factory/health-validation/toolkit"
)

// ibStatCatalog builds a catalog on an architecture and execution context
// the InfiniBand checks apply to, so they evaluate rather than skip.
func ibStatCatalog() *catalog.Catalog {
	return catalog.New(
		catalog.WithArchitecture("GB200"),
		catalog.WithExecutionContext(health.OperatorInfra),
	)
}

// healthyPortLines is one NDR fabric port in ibstat's output format.
var healthyPortLines = []string{
	"CA 'mlx5_0'",
	"\tPort 1:",
	"\t\tState: Active",
	"\t\tPhysical state: LinkUp",
	"\t\tRate: 400",
	"\t\tLink layer: InfiniBand",
}

// splitFabricLines is a GB300 host whose fabric ports are broken out: 400 is
// a 2x split of the adapter's 800, 200 a 4x.
var splitFabricLines = []string{
	"CA 'mlx5_0'",
	"\tPort 1:", "\t\tState: Active", "\t\tPhysical state: LinkUp",
	"\t\tRate: 400", "\t\tLink layer: InfiniBand",
	"CA 'mlx5_1'",
	"\tPort 1:", "\t\tState: Active", "\t\tPhysical state: LinkUp",
	"\t\tRate: 200", "\t\tLink layer: InfiniBand",
}

// downPortLines is a second adapter whose link never trained.
var downPortLines = []string{
	"CA 'mlx5_1'",
	"\tPort 1:",
	"\t\tState: Down",
	"\t\tPhysical state: Polling",
	"\t\tRate: 10",
	"\t\tLink layer: InfiniBand",
}

func TestIBStatSensor_Name(t *testing.T) {
	s := toolkit.NewIBStatSensor(toolkit.MockIBStat())
	if s.Name() != "ibstat" {
		t.Errorf("Name() = %q, want ibstat", s.Name())
	}
}

// One measurement per port, not one per adapter or one per pass.
func TestIBStatSensor_MeasurePerPort(t *testing.T) {
	s := toolkit.NewIBStatSensor(toolkit.MockIBStat(append(healthyPortLines, downPortLines...)...))
	ms, err := s.Measure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("got %d measurements, want one per port", len(ms))
	}
	for _, m := range ms {
		if _, ok := m.(infiniband.PortMeasurement); !ok {
			t.Errorf("measurement is %T, want infiniband.PortMeasurement", m)
		}
	}
}

// End-to-end against the real catalog checks: a healthy NDR port passes and
// is not skipped, so the checks are demonstrably reachable from the sensor.
func TestIBStatSensor_HealthyPortPasses(t *testing.T) {
	s := toolkit.NewIBStatSensor(toolkit.MockIBStat(healthyPortLines...))
	nv, err := toolkit.Collect(ibStatCatalog(), s)
	if err != nil {
		t.Fatal(err)
	}
	if nv.OverallSeverity != health.Pass {
		t.Errorf("OverallSeverity = %v, want PASS", nv.OverallSeverity)
	}
	for _, v := range nv.SkippedChecks {
		if v.CheckID == "ib_port_state" || v.CheckID == "ib_port_rate" {
			t.Errorf("%s skipped (%v); a healthy port must evaluate it", v.CheckID, v.SkipReason)
		}
	}
}

func TestIBStatSensor_DownPortIsCritical(t *testing.T) {
	s := toolkit.NewIBStatSensor(toolkit.MockIBStat(append(healthyPortLines, downPortLines...)...))
	nv, err := toolkit.Collect(ibStatCatalog(), s)
	if err != nil {
		t.Fatal(err)
	}
	if nv.OverallSeverity != health.Critical {
		t.Errorf("OverallSeverity = %v, want CRITICAL", nv.OverallSeverity)
	}
}

// A rate no division produces is a hardware fault, and it lands on
// ib_port_rate alone while ib_port_state passes — the split that tells an
// operator to look at the cable, not the fabric.
func TestIBStatSensor_BadRateFailsOnlyTheRateCheck(t *testing.T) {
	s := toolkit.NewIBStatSensor(toolkit.MockIBStat(
		"CA 'mlx5_0'",
		"\tPort 1:",
		"\t\tState: Active",
		"\t\tPhysical state: LinkUp",
		"\t\tRate: 56",
		"\t\tLink layer: InfiniBand",
	))
	nv, err := toolkit.Collect(ibStatCatalog(), s)
	if err != nil {
		t.Fatal(err)
	}

	severities := make(map[string]health.Severity)
	for _, v := range nv.CheckResults {
		severities[v.CheckID] = v.Severity
	}
	if got := severities["ib_port_rate"]; got != health.Critical {
		t.Errorf("ib_port_rate = %v, want CRITICAL", got)
	}
	if got := severities["ib_port_state"]; got != health.Pass {
		t.Errorf("ib_port_state = %v, want PASS", got)
	}
}

// An undeclared split warns instead, so the whole node verdict stays out of
// the drain range: WARNING aggregates without making the run unhealthy.
func TestIBStatSensor_UndeclaredSplitWarnsWithoutDraining(t *testing.T) {
	s := toolkit.NewIBStatSensor(toolkit.MockIBStat(
		"CA 'mlx5_0'",
		"\tPort 1:",
		"\t\tState: Active",
		"\t\tPhysical state: LinkUp",
		"\t\tRate: 200",
		"\t\tLink layer: InfiniBand",
	))
	nv, err := toolkit.Collect(ibStatCatalog(), s)
	if err != nil {
		t.Fatal(err)
	}
	if nv.OverallSeverity != health.Warning {
		t.Errorf("OverallSeverity = %v, want WARNING", nv.OverallSeverity)
	}
}

// A mixed-fabric host — a broken-out RoCE NIC alongside whole InfiniBand
// fabric ports — is the GB200/GB300 shape, and the three ports exercise all
// three outcomes at once: the RoCE port passes as a legal division, the
// half-rate fabric port warns as a possible undeclared split, and the one at
// an unsupported rate is the only CRITICAL.
func TestIBStatSensor_MixedFabricJudgesLinkLayersSeparately(t *testing.T) {
	s := toolkit.NewIBStatSensor(toolkit.MockIBStat(
		"CA 'mlx5_0'", // RoCE, split by default: 100 is a legal division
		"\tPort 1:", "\t\tState: Active", "\t\tPhysical state: LinkUp",
		"\t\tRate: 100", "\t\tLink layer: Ethernet",
		"CA 'mlx5_1'", // InfiniBand at half rate: ambiguous, warn
		"\tPort 1:", "\t\tState: Active", "\t\tPhysical state: LinkUp",
		"\t\tRate: 200", "\t\tLink layer: InfiniBand",
		"CA 'mlx5_2'", // InfiniBand off the ladder entirely: fault
		"\tPort 1:", "\t\tState: Active", "\t\tPhysical state: LinkUp",
		"\t\tRate: 56", "\t\tLink layer: InfiniBand",
	))
	nv, err := toolkit.Collect(ibStatCatalog(), s)
	if err != nil {
		t.Fatal(err)
	}

	bySeverity := map[health.Severity][]string{}
	for _, v := range nv.CheckResults {
		if v.CheckID == "ib_port_rate" {
			bySeverity[v.Severity] = append(bySeverity[v.Severity], strings.Join(v.Messages, " "))
		}
		if v.CheckID == "ib_port_state" && v.Severity != health.Pass {
			t.Errorf("ib_port_state = %v, want PASS: every port is Active/LinkUp", v.Severity)
		}
	}

	if got := bySeverity[health.Pass]; len(got) != 1 {
		t.Errorf("ib_port_rate PASS count = %d, want 1 (the RoCE port): %v", len(got), got)
	}
	if got := bySeverity[health.Warning]; len(got) != 1 || !strings.Contains(got[0], "mlx5_1") {
		t.Errorf("ib_port_rate WARNING = %v, want one naming mlx5_1", got)
	}
	if got := bySeverity[health.Critical]; len(got) != 1 || !strings.Contains(got[0], "mlx5_2") {
		t.Errorf("ib_port_rate CRITICAL = %v, want one naming mlx5_2", got)
	}
	if nv.OverallSeverity != health.Critical {
		t.Errorf("OverallSeverity = %v, want CRITICAL (the worst of the three)", nv.OverallSeverity)
	}
}

// A GB300 fleet that breaks out its fabric ports declares it, and the split
// rates then apply: 400 (2x) and 200 (4x) are both healthy divisions of 800.
// Without the declaration these same ports fail, which is the point of the
// knob — see TestIBStatSensor_GB300FabricPortsDefaultToWholeRate.
func TestIBStatSensor_GB300SplitModePasses(t *testing.T) {
	s := toolkit.NewIBStatSensor(toolkit.MockIBStat(splitFabricLines...))
	cat := catalog.New(
		catalog.WithArchitecture("GB300"),
		catalog.WithExecutionContext(health.OperatorInfra),
		catalog.WithOverrides(catalog.NewOverrideSet(catalog.Override{
			CheckID:       "ib_port_rate",
			ThresholdName: "infiniband_port_split",
			Value:         true,
		})),
	)
	nv, err := toolkit.Collect(cat, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range nv.CheckResults {
		if v.Severity != health.Pass {
			t.Errorf("%s = %v (%v), want PASS: both rates are legal GB300 splits",
				v.CheckID, v.Severity, v.Messages)
		}
	}
}

// The same capture without the declaration: native InfiniBand defaults to the
// whole port rate, so both ports are flagged — but as WARNINGs, since both sit
// on legal divisions. A fleet that forgot the threshold is told about it
// without having every node drained.
func TestIBStatSensor_GB300FabricPortsDefaultToWholeRate(t *testing.T) {
	s := toolkit.NewIBStatSensor(toolkit.MockIBStat(splitFabricLines...))
	cat := catalog.New(
		catalog.WithArchitecture("GB300"),
		catalog.WithExecutionContext(health.OperatorInfra),
	)
	nv, err := toolkit.Collect(cat, s)
	if err != nil {
		t.Fatal(err)
	}
	var warned int
	for _, v := range nv.CheckResults {
		if v.CheckID != "ib_port_rate" {
			continue
		}
		if v.Severity == health.Critical {
			t.Errorf("ib_port_rate = CRITICAL (%v), want WARNING: both rates are legal divisions", v.Messages)
		}
		if v.Severity == health.Warning {
			warned++
		}
	}
	if warned != 2 {
		t.Errorf("ib_port_rate warned %d times, want 2: neither port is at GB300's whole 800 Gb/s", warned)
	}
	if nv.OverallSeverity != health.Warning {
		t.Errorf("OverallSeverity = %v, want WARNING so the node stays in service", nv.OverallSeverity)
	}
}

// A host with no InfiniBand must not fail: the checks are skipped, and the
// node verdict is unaffected.
func TestIBStatSensor_NoPortsSkipsRatherThanFails(t *testing.T) {
	s := toolkit.NewIBStatSensor(toolkit.MockIBStat())
	nv, err := toolkit.Collect(ibStatCatalog(), s)
	if err != nil {
		t.Fatal(err)
	}
	if nv.OverallSeverity == health.Critical {
		t.Errorf("OverallSeverity = %v, want a node without InfiniBand not to fail", nv.OverallSeverity)
	}

	skipped := make(map[string]bool)
	for _, v := range nv.SkippedChecks {
		skipped[v.CheckID] = true
	}
	for _, id := range []string{"ib_port_state", "ib_port_rate"} {
		if !skipped[id] {
			t.Errorf("%s was not skipped; an unmeasured port must be visibly unassessed", id)
		}
	}
}

// Errored is the hard-failure path (a timeout or a backend error), and must
// fail the checks rather than leave them unassessed.
func TestIBStatSensor_Errored(t *testing.T) {
	s := toolkit.NewIBStatSensor(toolkit.MockIBStat())
	ms := s.Errored(errors.New("ibstat sensor: collection timed out"))
	if len(ms) != 1 {
		t.Fatalf("got %d measurements, want one errored stand-in", len(ms))
	}
	m, ok := ms[0].(infiniband.PortMeasurement)
	if !ok {
		t.Fatalf("measurement is %T, want infiniband.PortMeasurement", ms[0])
	}
	if len(m.Errors()) != 1 {
		t.Fatalf("Errors() = %v, want one entry", m.Errors())
	}
	if got := len(m.Errors()[0].FailedSignals()); got != len(infiniband.PortMeasurementSignals) {
		t.Errorf("failed signals = %d, want all %d", got, len(infiniband.PortMeasurementSignals))
	}
}
