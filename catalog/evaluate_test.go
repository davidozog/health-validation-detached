package catalog_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/health-validation/catalog"
	"github.com/dsx-ai-factory/health-validation/catalog/catalogtest"
	"github.com/dsx-ai-factory/health-validation/catalog/gpu"
	"github.com/dsx-ai-factory/health-validation/catalog/health"
	"github.com/dsx-ai-factory/health-validation/catalog/node"
	"github.com/dsx-ai-factory/health-validation/catalog/nvlink"
	"github.com/dsx-ai-factory/health-validation/catalog/xid"
)

// twoSignalMeasurement is a synthetic Node-category measurement carrying
// two independent signals. The engine tests use it to exercise
// multi-signal evaluation, error attribution, and rule aggregation
// without coupling to a production domain's schema.
type twoSignalMeasurement struct {
	errors []health.ErroredMeasurement
	First  int64 `json:"first"`
	Second int64 `json:"second"`
}

func (twoSignalMeasurement) Category() health.Category { return health.Node }

func (m twoSignalMeasurement) Errors() []health.ErroredMeasurement { return m.errors }

func (m *twoSignalMeasurement) AddError(err error, signals ...string) {
	m.errors = append(m.errors, health.NewErroredMeasurement(err, signals...))
}

func TestEvaluate_NotFound(t *testing.T) {
	cat := catalog.New(catalog.WithArchitecture(catalogtest.DummyArchitecture))
	_, err := cat.Evaluate("nonexistent", gpu.GPUMeasurement{})
	if err == nil {
		t.Error("expected error for missing check")
	}
}

func TestEvaluate_ArchitectureUnset(t *testing.T) {
	cat := catalog.New()
	_, err := cat.Evaluate("cpu_count", gpu.GPUMeasurement{})
	if !errors.Is(err, catalog.ErrArchitectureUnset) {
		t.Fatalf("Evaluate error = %v, want ErrArchitectureUnset", err)
	}
	_, err = cat.EvaluateAllRaw()
	if !errors.Is(err, catalog.ErrArchitectureUnset) {
		t.Fatalf("EvaluateAllRaw error = %v, want ErrArchitectureUnset", err)
	}
}

func TestEvaluate_SkipDisabled(t *testing.T) {
	cat := catalog.New(
		catalog.WithArchitecture("GB200"),
		catalog.WithExecutionContext(health.OperatorInfra),
		catalog.WithDisabledChecks("gpu_xid_79"),
	)

	v, err := cat.Evaluate("gpu_xid_79", xid.XidMeasurement{
		XidCounts: map[int64]int64{79: 1}, Since: "boot", EventCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Skipped || v.SkipReason != health.CheckDisabled {
		t.Errorf("expected skipped with CHECK_DISABLED, got skipped=%v reason=%v", v.Skipped, v.SkipReason)
	}
}

// inventoryMeasurement and deviceMeasurement are two synthetic
// Node-category measurements with disjoint signal schemas — a node-wide
// list and a per-device metric. They stand in for the real situation
// that motivated signal-subset routing: two measurement types that
// share a category but feed different checks.
type inventoryMeasurement struct {
	Names []string `json:"inventory_names"`
}

func (inventoryMeasurement) Category() health.Category           { return health.Node }
func (inventoryMeasurement) Errors() []health.ErroredMeasurement { return nil }

type deviceMeasurement struct {
	Device string  `json:"device_id"`
	Metric float64 `json:"device_metric"`
}

func (deviceMeasurement) Category() health.Category           { return health.Node }
func (deviceMeasurement) Errors() []health.ErroredMeasurement { return nil }

// Two measurement types that share a Category but carry disjoint signal
// schemas must each route only to the check whose required signals they
// carry. Neither check is skipped as collateral of the other's
// measurement, and a per-entity check yields one verdict per entity —
// the behavior that raw category routing got wrong.
func TestEvaluateAll_RoutesByRequiredSignals_NoCrossMeasurementSkips(t *testing.T) {
	inventory := health.CheckDefinition{
		ID:                "inventory_check",
		Category:          health.Node,
		RequiredSignals:   []string{"inventory_names"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "'absent' in inventory_names", Severity: health.Critical, Message: "missing"},
		},
	}
	perDevice := health.CheckDefinition{
		ID:                "per_device_check",
		Category:          health.Node,
		RequiredSignals:   []string{"device_id", "device_metric"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "device_metric > 1.0", Severity: health.Critical, Message: "device ${device_id} too high"},
		},
	}

	cat := catalog.NewEmpty(
		catalog.WithArchitecture(catalogtest.DummyArchitecture),
		catalog.WithExecutionContext(health.OperatorInfra),
	)
	cat.MustRegister(inventory)
	cat.MustRegister(perDevice)

	verdicts, err := cat.EvaluateAllRaw(
		inventoryMeasurement{Names: []string{"dev0", "dev1"}},
		deviceMeasurement{Device: "dev0", Metric: 0.5},
		deviceMeasurement{Device: "dev1", Metric: 0.5},
	)
	if err != nil {
		t.Fatalf("EvaluateAllRaw: %v", err)
	}

	// The inventory measurement feeds inventory_check once; the two device
	// measurements feed per_device_check twice. Nothing is skipped.
	byCheck := map[string]int{}
	for _, v := range verdicts {
		if v.Skipped {
			t.Errorf("CheckID %q skipped (%v); signal-subset routing should feed both checks",
				v.CheckID, v.SkipReason)
			continue
		}
		if v.Severity != health.Pass {
			t.Errorf("CheckID %q severity = %v, want PASS", v.CheckID, v.Severity)
		}
		byCheck[v.CheckID]++
	}
	if byCheck["inventory_check"] != 1 {
		t.Errorf("inventory_check verdicts = %d, want 1 (fed by the one inventory measurement)",
			byCheck["inventory_check"])
	}
	if byCheck["per_device_check"] != 2 {
		t.Errorf("per_device_check verdicts = %d, want 2 (one per device measurement)",
			byCheck["per_device_check"])
	}
}

// Registering a check declares that its sensor is expected, so a category
// that supplies no measurement is a hole in the assessment rather than a
// domain that was out of scope. Every registered check must yield a
// verdict: the collected domain's checks evaluate, and every other
// domain's checks are visible as SENSOR_MISSING skips instead of
// vanishing from the output.
func TestEvaluateAll_AbsentCategoryChecksSkipSensorMissing(t *testing.T) {
	// Xid 79 is CRITICAL; the measurement carries all xid signals. The full
	// catalog also holds gpu/node/nvlink/cpu checks in other categories.
	m := xid.XidMeasurement{
		XidCounts:  map[int64]int64{79: 1},
		EventCount: 1,
		Since:      "boot",
		Events:     []xid.XidEvent{{XidCode: 79}},
	}

	cat := catalog.New(
		catalog.WithArchitecture("GB200"),
		catalog.WithExecutionContext(health.OperatorInfra),
	)

	verdicts, err := cat.EvaluateAllRaw(m)
	if err != nil {
		t.Fatalf("EvaluateAllRaw: %v", err)
	}

	seen := make(map[string]bool, len(verdicts))
	for _, v := range verdicts {
		seen[v.CheckID] = true
		if v.Category == health.Xid {
			if v.SkipReason == health.SensorMissing {
				t.Errorf("CheckID %q: collected category reported SENSOR_MISSING", v.CheckID)
			}
			continue
		}
		// Only Xid supplies a measurement here, so every other check skips
		// SENSOR_MISSING in its own right. Nothing else is reachable: a
		// dependency never causes a skip.
		switch v.SkipReason {
		case health.SensorMissing:
			if !strings.Contains(v.SkipDetail, v.Category.String()) {
				t.Errorf("CheckID %q skip detail %q does not name the missing category",
					v.CheckID, v.SkipDetail)
			}
		default:
			t.Errorf("CheckID %q (category %v): skipped=%v reason=%v, want SENSOR_MISSING",
				v.CheckID, v.Category, v.Skipped, v.SkipReason)
		}
	}

	// No registered check may be missing from the output entirely — the
	// silent-drop this skip reason exists to eliminate.
	for _, cd := range cat.List(catalog.Filter{}) {
		if !seen[cd.ID] {
			t.Errorf("CheckID %q produced no verdict at all", cd.ID)
		}
	}
}

// Disabling a check is intentional; a missing sensor is not. When both
// apply the operator's reason must win, so that config-driven disabling
// and an absent sensor stay distinguishable in the output.
func TestEvaluateAll_DisabledOutranksSensorMissing(t *testing.T) {
	disabled := health.CheckDefinition{
		ID:                "node_disabled",
		Category:          health.Node,
		RequiredSignals:   []string{node.SignalGPUCount},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "gpu_count < 1", Severity: health.Critical, Message: "no gpus"},
		},
	}

	cat := catalog.NewEmpty(
		catalog.WithArchitecture(catalogtest.DummyArchitecture),
		catalog.WithExecutionContext(health.OperatorInfra),
		catalog.WithDisabledChecks("node_disabled"),
	)
	cat.MustRegister(disabled)

	// No measurements at all: the Node sensor never reported.
	verdicts, err := cat.EvaluateAllRaw()
	if err != nil {
		t.Fatalf("EvaluateAllRaw: %v", err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("verdicts = %d, want 1", len(verdicts))
	}
	if got := verdicts[0].SkipReason; got != health.CheckDisabled {
		t.Errorf("skip reason = %v, want CHECK_DISABLED", got)
	}
}

// A check whose domain reported nothing is recorded as a SENSOR_MISSING
// skip rather than vanishing from the verdict, so a consumer can tell
// "not assessed" from "assessed and healthy". The checks that did run
// still drive OverallSeverity: an unassessed domain is a coverage fact to
// read off skip_reason, not a severity.
func TestEvaluateAll_MissingSensorRecordsSkip(t *testing.T) {
	present := health.CheckDefinition{
		ID:                "node_present",
		Category:          health.Node,
		RequiredSignals:   []string{node.SignalGPUCount},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "gpu_count < 1", Severity: health.Critical, Message: "no gpus"},
		},
	}
	absent := health.CheckDefinition{
		ID:                "gpu_absent",
		Category:          health.GPU,
		RequiredSignals:   []string{"gpu_id"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "gpu_id == ''", Severity: health.Critical, Message: "no id"},
		},
	}

	cat := catalog.NewEmpty(
		catalog.WithArchitecture(catalogtest.DummyArchitecture),
		catalog.WithExecutionContext(health.OperatorInfra),
	)
	cat.MustRegister(present)
	cat.MustRegister(absent)

	nv, err := cat.EvaluateAll(node.NodeMeasurement{GPUCount: 4})
	if err != nil {
		t.Fatal(err)
	}
	if nv.OverallSeverity != health.Pass {
		t.Errorf("OverallSeverity = %v, want PASS (from the check that ran)", nv.OverallSeverity)
	}
	if len(nv.CheckResults) != 1 || nv.CheckResults[0].CheckID != "node_present" {
		t.Errorf("check results = %+v, want the one check whose sensor reported", nv.CheckResults)
	}
	if len(nv.SkippedChecks) != 1 || nv.SkippedChecks[0].SkipReason != health.SensorMissing {
		t.Errorf("skipped = %+v, want one SENSOR_MISSING", nv.SkippedChecks)
	}
}

func TestEvaluateAll_Dependencies(t *testing.T) {
	driver := health.CheckDefinition{
		ID:                "node_driver",
		Category:          health.Node,
		RequiredSignals:   []string{"driver_loaded"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "driver_loaded == false", Severity: health.Critical, Message: "no driver"},
		},
	}
	dependent := health.CheckDefinition{
		ID:                "node_check",
		Category:          health.Node,
		RequiredSignals:   []string{node.SignalGPUCount},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Thresholds:        map[string]health.ThresholdSpec{},
		Dependencies:      []string{"node_driver"},
		Rules: []health.Rule{
			{Expression: "gpu_count < 1", Severity: health.Critical, Message: "no gpus"},
		},
	}

	cat := catalog.NewEmpty(
		catalog.WithArchitecture(catalogtest.DummyArchitecture),
		catalog.WithExecutionContext(health.OperatorInfra),
	)
	cat.MustRegister(driver)
	cat.MustRegister(dependent)

	nv, err := cat.EvaluateAll(node.NodeMeasurement{GPUCount: 4})
	if err != nil {
		t.Fatal(err)
	}
	// node_driver has no measurement carrying driver_loaded, so it skips. Its
	// dependent carries gpu_count and evaluates regardless: a dependency that
	// did not run is not a reason to leave a readable signal unassessed.
	if len(nv.SkippedChecks) != 1 {
		t.Errorf("skipped = %d, want 1", len(nv.SkippedChecks))
	}
	for _, sc := range nv.SkippedChecks {
		if sc.CheckID != "node_driver" {
			t.Errorf("skipped %q, want only node_driver", sc.CheckID)
		}
	}
	var ran bool
	for _, r := range nv.CheckResults {
		if r.CheckID == "node_check" {
			ran = true
		}
	}
	if !ran {
		t.Error("node_check did not run; its required signal was present")
	}
}

// A measurement reporting Errors produces one Critical CheckVerdict
// per matching check, with the sensor's detail surfaced in the
// message. The CEL rules must not run against the placeholder.
func TestEvaluateAll_ErroredMeasurement(t *testing.T) {
	cat := catalog.New(
		catalog.WithArchitecture("GB200"),
		catalog.WithExecutionContext(health.OperatorInfra),
	)

	verdicts, err := cat.EvaluateAllRaw(
		xid.ErroredXid("boot", errors.New("journal unavailable: ERROR_READ_FAILED"),
			xid.MeasurementSignals...),
	)
	if err != nil {
		t.Fatalf("EvaluateAllRaw: %v", err)
	}
	if len(verdicts) == 0 {
		t.Fatal("no verdicts produced for errored xid measurement")
	}
	// Every xid check the measurement feeds must fail Critical with the
	// sensor error detail surfaced — XidMeasurement carries all xid signals.
	// Checks in other categories the measurement does not feed skip (no
	// applicable measurement, or an unconfigured threshold); they are not
	// collateral failures, so a skip is fine, a non-Critical verdict is not.
	var failed int
	for _, v := range verdicts {
		if v.Skipped {
			continue
		}
		failed++
		if v.Severity != health.Critical {
			t.Errorf("CheckID %q: Severity = %v, want CRITICAL", v.CheckID, v.Severity)
		}
		if !strings.Contains(strings.Join(v.Messages, " "), "ERROR_READ_FAILED") {
			t.Errorf("CheckID %q: Messages = %q, want sensor detail surfaced", v.CheckID, v.Messages)
		}
	}
	if failed == 0 {
		t.Error("errored xid measurement produced no failed verdicts for the checks it feeds")
	}
}

// An errored measurement only fails checks whose RequiredSignals are
// all present in the measurement's schema. A category-matching check
// that requires signals the measurement type doesn't carry does not
// route to it, so that check skips NoApplicableMeasurement instead of
// becoming collateral damage of the errored entity.
func TestEvaluateAll_ErroredMeasurement_GatesOnRequiredSignals(t *testing.T) {
	// standard requires gpu_id, which is in GPUMeasurement's schema.
	standard := health.CheckDefinition{
		ID:                "gpu_standard",
		Category:          health.GPU,
		RequiredSignals:   []string{gpu.SignalGPUID},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Architectures:     []string{"GB200"},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "gpu_id != ''", Severity: health.Critical, Message: "missing gpu_id"},
		},
	}
	// thermal requires thermal_temp, which GPUMeasurement does not carry.
	thermal := health.CheckDefinition{
		ID:                "gpu_thermal",
		Category:          health.GPU,
		RequiredSignals:   []string{"thermal_temp"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Architectures:     []string{"GB200"},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "thermal_temp > 80", Severity: health.Critical, Message: "hot"},
		},
	}

	cat := catalog.NewEmpty(
		catalog.WithArchitecture("GB200"),
		catalog.WithExecutionContext(health.OperatorInfra),
	)
	cat.MustRegister(standard)
	cat.MustRegister(thermal)

	verdicts, err := cat.EvaluateAllRaw(gpu.ErroredGPU("GPU-4", errors.New("GPU is lost"),
		gpu.MeasurementSignals...))
	if err != nil {
		t.Fatalf("EvaluateAllRaw: %v", err)
	}

	var critical, thermalSkipped int
	for _, v := range verdicts {
		switch {
		case v.Skipped:
			if v.CheckID != "gpu_thermal" {
				t.Errorf("unexpected skip on %q", v.CheckID)
				break
			}
			thermalSkipped++
			if v.SkipReason != health.NoApplicableMeasurement {
				t.Errorf("gpu_thermal SkipReason = %v, want NO_APPLICABLE_MEASUREMENT (signal not in schema)",
					v.SkipReason)
			}
		case v.Severity == health.Critical:
			critical++
			if v.CheckID == "gpu_thermal" {
				t.Errorf("Critical verdict on gpu_thermal; thermal_temp not in GPUMeasurement schema")
			}
		}
	}
	if critical != 1 {
		t.Errorf("got %d Critical verdicts, want 1 (gpu_standard)", critical)
	}
	// gpu_thermal requires thermal_temp, outside the GPUMeasurement
	// schema, so it skips exactly once.
	if thermalSkipped != 1 {
		t.Errorf("got %d gpu_thermal skips, want 1 (signal outside schema)", thermalSkipped)
	}
}

// A partially-errored measurement only fails the checks whose
// RequiredSignals overlap with FailedSignals. Checks whose required
// signals were all successfully collected continue to run normally
// against the populated fields, even though the measurement carries
// non-empty Errors.
func TestEvaluateAll_ErroredMeasurement_PartialFailureSparesUnaffectedChecks(t *testing.T) {
	// firstOnly requires the first signal, which the sensor collected
	// successfully. secondOnly requires the second signal, which failed.
	firstOnly := health.CheckDefinition{
		ID:                "node_first_only",
		Category:          health.Node,
		RequiredSignals:   []string{"first"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Architectures:     []string{"GB200"},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "first < 1", Severity: health.Critical, Message: "first too low"},
		},
	}
	secondOnly := health.CheckDefinition{
		ID:                "node_second_only",
		Category:          health.Node,
		RequiredSignals:   []string{"second"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Architectures:     []string{"GB200"},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "second > 0", Severity: health.Critical, Message: "second nonzero"},
		},
	}

	cat := catalog.NewEmpty(
		catalog.WithArchitecture("GB200"),
		catalog.WithExecutionContext(health.OperatorInfra),
	)
	cat.MustRegister(firstOnly)
	cat.MustRegister(secondOnly)

	// Sensor collected the first signal successfully but the second failed.
	m := twoSignalMeasurement{First: 4}
	m.AddError(errors.New("second-signal query: ERROR_UNKNOWN"), "second")

	verdicts, err := cat.EvaluateAllRaw(m)
	if err != nil {
		t.Fatalf("EvaluateAllRaw: %v", err)
	}

	var firstVerdict, secondVerdict *health.CheckVerdict
	for i := range verdicts {
		switch verdicts[i].CheckID {
		case "node_first_only":
			firstVerdict = &verdicts[i]
		case "node_second_only":
			secondVerdict = &verdicts[i]
		}
	}
	if firstVerdict == nil || secondVerdict == nil {
		t.Fatalf("missing one of the expected verdicts: first=%v second=%v",
			firstVerdict, secondVerdict)
		return
	}
	// first signal was collected — CEL ran on real data and the check passed.
	if firstVerdict.Severity != health.Pass {
		t.Errorf("node_first_only severity = %v, want PASS (first was collected)",
			firstVerdict.Severity)
	}
	// second signal failed — Critical with the sensor's detail.
	if secondVerdict.Severity != health.Critical {
		t.Errorf("node_second_only severity = %v, want CRITICAL (second signal failed)",
			secondVerdict.Severity)
	}
	if !strings.Contains(strings.Join(secondVerdict.Messages, " "), "second-signal query") {
		t.Errorf("node_second_only messages = %q, want sensor detail surfaced", secondVerdict.Messages)
	}
}

// When a measurement carries multiple independent error entries and a
// check's RequiredSignals overlap with more than one, the resulting
// Critical verdict's message joins the matching entries' details so
// consumers see every failure that affected the check.
func TestEvaluateAll_ErroredMeasurement_JoinsMatchingErrorDetails(t *testing.T) {
	// nodeBoth requires both signals; both error entries cover signals
	// it needs, so both details must appear in the verdict.
	nodeBoth := health.CheckDefinition{
		ID:                "node_both",
		Category:          health.Node,
		RequiredSignals:   []string{"first", "second"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Architectures:     []string{"GB200"},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "first < 1", Severity: health.Critical, Message: "first too low"},
		},
	}

	cat := catalog.NewEmpty(
		catalog.WithArchitecture("GB200"),
		catalog.WithExecutionContext(health.OperatorInfra),
	)
	cat.MustRegister(nodeBoth)

	var m twoSignalMeasurement
	m.AddError(errors.New("first-signal query: ERROR_UNKNOWN"), "first")
	m.AddError(errors.New("second-signal query: ERROR_UNKNOWN"), "second")

	verdicts, err := cat.EvaluateAllRaw(m)
	if err != nil {
		t.Fatalf("EvaluateAllRaw: %v", err)
	}
	var v health.CheckVerdict
	for _, vv := range verdicts {
		if vv.CheckID == "node_both" {
			v = vv
			break
		}
	}
	if v.CheckID == "" {
		t.Fatalf("no node_both verdict in %d results", len(verdicts))
	}
	if v.Severity != health.Critical {
		t.Fatalf("Severity = %v, want CRITICAL", v.Severity)
	}
	// Both error entries cover signals the check needs, so both
	// details should appear in the verdict messages.
	joined := strings.Join(v.Messages, " ")
	if !strings.Contains(joined, "first-signal query") {
		t.Errorf("Messages = %q, missing first-signal error detail", v.Messages)
	}
	if !strings.Contains(joined, "second-signal query") {
		t.Errorf("Messages = %q, missing second-signal error detail", v.Messages)
	}
}

// An errored measurement and a healthy measurement in the same call
// each route to their own verdicts: the healthy one runs through CEL
// normally; the errored one short-circuits to a Critical verdict.
// The aggregate severity is Critical because of the errored entity.
func TestEvaluateAll_MixedErroredAndHealthy(t *testing.T) {
	check := health.CheckDefinition{
		ID:                "gpu_check",
		Category:          health.GPU,
		RequiredSignals:   []string{gpu.SignalGPUID},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Architectures:     []string{"GB200"},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			// Never matches for the healthy measurement; the errored
			// one short-circuits before CEL runs.
			{Expression: "gpu_id == 'trigger-critical'", Severity: health.Critical, Message: "triggered"},
		},
	}

	cat := catalog.NewEmpty(
		catalog.WithArchitecture("GB200"),
		catalog.WithExecutionContext(health.OperatorInfra),
	)
	cat.MustRegister(check)

	healthy := gpu.GPUMeasurement{GPUID: "GPU-0"}
	errored := gpu.ErroredGPU("GPU-7", errors.New("GPU-7 fell off the bus"),
		gpu.MeasurementSignals...)

	nv, err := cat.EvaluateAll(healthy, errored)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	if nv.OverallSeverity != health.Critical {
		t.Errorf("OverallSeverity = %v, want CRITICAL (errored GPU fails)",
			nv.OverallSeverity)
	}
	if len(nv.CheckResults) != 2 {
		t.Errorf("CheckResults = %d, want 2 (1 check x 2 measurements)",
			len(nv.CheckResults))
	}
	if nv.ExitCode() != 1 {
		t.Errorf("ExitCode = %d, want 1 (CRITICAL)", nv.ExitCode())
	}
}

// A Rule with a non-empty RecommendedAction overrides the parent
// CheckDefinition's RecommendedAction in the resulting verdict; rules
// that leave it empty fall back to the check-level default.
func TestEvaluate_RuleRecommendedActionOverridesCheckDefault(t *testing.T) {
	check := health.CheckDefinition{
		ID:                "node_action_override",
		Category:          health.Node,
		RequiredSignals:   []string{"first", "second"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Thresholds:        map[string]health.ThresholdSpec{},
		RecommendedAction: health.Drain(health.RemediationActionResetGPU),
		Rules: []health.Rule{
			{
				Expression:        "second > 0",
				Severity:          health.Warning,
				Message:           "second nonzero",
				RecommendedAction: health.Drain(health.RemediationActionRestartOS),
			},
			{
				Expression: "first < 4",
				Severity:   health.Critical,
				Message:    "first too low",
			},
		},
	}

	cat := catalog.NewEmpty(
		catalog.WithArchitecture(catalogtest.DummyArchitecture),
		catalog.WithExecutionContext(health.OperatorInfra),
	)
	cat.MustRegister(check)

	rebootCase := twoSignalMeasurement{First: 4, Second: 1}
	v, err := cat.Evaluate("node_action_override", rebootCase)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !v.RecommendedAction.Equal(health.Drain(health.RemediationActionRestartOS)) {
		t.Errorf("RecommendedAction = %v, want %v (rule override)", v.RecommendedAction, "restart_os")
	}

	drainCase := twoSignalMeasurement{First: 3}
	v, err = cat.Evaluate("node_action_override", drainCase)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !v.RecommendedAction.Equal(health.Drain(health.RemediationActionResetGPU)) {
		t.Errorf("RecommendedAction = %v, want %v (check default)", v.RecommendedAction, "drain")
	}
}

// All matching rules contribute to the verdict: each yields one
// entry in Messages; Severity is the worst across the matching set;
// RecommendedAction comes from the highest-priority matching rule's
// override (or the check default if it has none).
func TestEvaluate_AllMatchingRulesAggregate(t *testing.T) {
	check := health.CheckDefinition{
		ID:                "node_multi_rule",
		Category:          health.Node,
		RequiredSignals:   []string{"first", "second"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Thresholds:        map[string]health.ThresholdSpec{},
		RecommendedAction: health.Drain(health.RemediationActionResetGPU),
		Rules: []health.Rule{
			{
				Expression:        "second > 0",
				Severity:          health.Warning,
				Message:           "second nonzero",
				RecommendedAction: health.Drain(health.RemediationActionRestartOS),
			},
			{
				Expression: "first < 4",
				Severity:   health.Critical,
				Message:    "first below four",
			},
			{
				Expression: "first == 0",
				Severity:   health.Critical,
				Message:    "first is zero",
			},
		},
	}

	cat := catalog.NewEmpty(
		catalog.WithArchitecture(catalogtest.DummyArchitecture),
		catalog.WithExecutionContext(health.OperatorInfra),
	)
	cat.MustRegister(check)

	// All three rules fire simultaneously.
	m := twoSignalMeasurement{First: 0, Second: 1}
	v, err := cat.Evaluate("node_multi_rule", m)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if len(v.Messages) != 3 {
		t.Fatalf("Messages len = %d, want 3 (one per matching rule)", len(v.Messages))
	}
	wantOrder := []string{
		"second nonzero",
		"first below four",
		"first is zero",
	}
	for i, want := range wantOrder {
		if v.Messages[i] != want {
			t.Errorf("Messages[%d] = %q, want %q (declaration order preserved)", i, v.Messages[i], want)
		}
	}

	// Worst severity across the three rules.
	if v.Severity != health.Critical {
		t.Errorf("Severity = %v, want CRITICAL (worst across matches)", v.Severity)
	}

	// Top-priority rule (first Critical match, "first below four") has no
	// RecommendedAction override, so the check default applies.
	if !v.RecommendedAction.Equal(health.Drain(health.RemediationActionResetGPU)) {
		t.Errorf("RecommendedAction = %v, want %v (top-priority rule has no override → check default)",
			v.RecommendedAction, "drain")
	}
}

// When only lower-severity rules match, the verdict reflects that
// severity, all lower-severity messages appear, and the override
// (if any) on the top-priority match wins.
func TestEvaluate_MultiMatchPicksTopPriorityOverride(t *testing.T) {
	check := health.CheckDefinition{
		ID:                "node_action_priority",
		Category:          health.Node,
		RequiredSignals:   []string{"first", "second"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Thresholds:        map[string]health.ThresholdSpec{},
		RecommendedAction: health.Drain(health.RemediationActionResetGPU),
		Rules: []health.Rule{
			{
				Expression:        "first == 4",
				Severity:          health.Info,
				Message:           "first is four (informational)",
				RecommendedAction: health.Drain(health.RemediationActionContactSupport),
			},
			{
				Expression:        "second > 0",
				Severity:          health.Warning,
				Message:           "second nonzero",
				RecommendedAction: health.Drain(health.RemediationActionRestartOS),
			},
		},
	}

	cat := catalog.NewEmpty(
		catalog.WithArchitecture(catalogtest.DummyArchitecture),
		catalog.WithExecutionContext(health.OperatorInfra),
	)
	cat.MustRegister(check)

	m := twoSignalMeasurement{First: 4, Second: 1}
	v, err := cat.Evaluate("node_action_priority", m)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if len(v.Messages) != 2 {
		t.Fatalf("Messages len = %d, want 2", len(v.Messages))
	}
	if v.Severity != health.Warning {
		t.Errorf("Severity = %v, want WARNING", v.Severity)
	}
	// Warning rule has higher severity than Info, so its "restart_os"
	// override wins over Info's "monitor".
	if !v.RecommendedAction.Equal(health.Drain(health.RemediationActionRestartOS)) {
		t.Errorf("RecommendedAction = %v, want %v (Warning rule outranks Info rule)",
			v.RecommendedAction, "restart_os")
	}
}

func TestEvaluateAll_NodeVerdict(t *testing.T) {
	cat := catalog.New(
		catalog.WithArchitecture("GB200"),
		catalog.WithExecutionContext(health.OperatorInfra),
	)

	// Xid 79 ("GPU has fallen off the bus") is a CRITICAL-severity check.
	m := xid.XidMeasurement{
		XidCounts:  map[int64]int64{79: 1},
		EventCount: 1,
		Since:      "boot",
		Events:     []xid.XidEvent{{XidCode: 79}},
	}

	nv, err := cat.EvaluateAll(m)
	if err != nil {
		t.Fatal(err)
	}
	if nv.OverallSeverity != health.Critical {
		t.Errorf("overall = %v, want CRITICAL", nv.OverallSeverity)
	}
	if nv.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1", nv.ExitCode())
	}
}

// Every verdict the catalog aggregates must name the catalog that produced
// it, so a consumer can tell whether a threshold or rule fix was in effect.
func TestEvaluateAll_StampsCatalogVersion(t *testing.T) {
	cat := catalog.New(catalog.WithArchitecture("GB200"))

	nv, err := cat.EvaluateAll(node.NodeMeasurement{GPUCount: 8})
	if err != nil {
		t.Fatal(err)
	}
	if nv.CatalogVersion == "" {
		t.Error("CatalogVersion is empty; every catalog-produced verdict must carry one")
	}
}

// Operator overrides supplied via WithOverrides must be honored by the
// batch evaluation path (EvaluateAll/Collect), not just the single-check
// Evaluate path. Regression test for overrides being dropped because the
// catalog built the engine's Params without them.
func TestEvaluateAll_AppliesOverrides(t *testing.T) {
	// gpu_count's expected_gpu_count is a DefaultThreshold that the
	// override path must propagate into evaluation.
	m := node.NodeMeasurement{GPUCount: 8}

	cat := catalog.New(
		catalog.WithArchitecture("GB200"),
		catalog.WithExecutionContext(health.OperatorInfra),
		catalog.WithOverrides(catalog.NewOverrideSet(catalog.Override{
			CheckID:       "gpu_count",
			ThresholdName: "expected_gpu_count",
			Value:         int64(8),
		})),
	)

	nv, err := cat.EvaluateAll(m)
	if err != nil {
		t.Fatal(err)
	}

	// 8 == 8 once expected_gpu_count is overridden to 8, so the
	// verdict must flip to PASS.
	if nv.OverallSeverity != health.Pass {
		t.Errorf("overall = %v, want PASS (override not applied on the batch path)", nv.OverallSeverity)
	}

	// And the resolved threshold must record the override and its provenance.
	verdicts, err := cat.EvaluateAllRaw(m)
	if err != nil {
		t.Fatal(err)
	}
	var checked bool
	for _, v := range verdicts {
		if v.CheckID != "gpu_count" {
			continue
		}
		checked = true
		rt, ok := v.Thresholds["expected_gpu_count"]
		if !ok {
			t.Fatal("expected_gpu_count missing from verdict thresholds")
		}
		if !rt.Overridden {
			t.Error("expected_gpu_count: Overridden = false, want true")
		}
		if rt.Value != int64(8) {
			t.Errorf("expected_gpu_count value = %v, want 8", rt.Value)
		}
	}
	if !checked {
		t.Fatal("gpu_count verdict not found")
	}
}

// Disabled checks must also be honored by the batch path; like overrides
// they were dropped from the engine Params built by EvaluateAllRaw.
func TestEvaluateAll_HonorsDisabledChecks(t *testing.T) {
	cat := catalog.New(
		catalog.WithArchitecture("GB200"),
		catalog.WithExecutionContext(health.OperatorInfra),
		catalog.WithDisabledChecks("gpu_xid_79"),
	)

	verdicts, err := cat.EvaluateAllRaw(xid.XidMeasurement{
		XidCounts: map[int64]int64{79: 1}, Since: "boot", EventCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	var checked bool
	for _, v := range verdicts {
		if v.CheckID != "gpu_xid_79" {
			continue
		}
		checked = true
		if !v.Skipped || v.SkipReason != health.CheckDisabled {
			t.Errorf("gpu_xid_79: skipped=%v reason=%v, want skipped=true CHECK_DISABLED",
				v.Skipped, v.SkipReason)
		}
	}
	if !checked {
		t.Fatal("gpu_xid_79 verdict not found")
	}
}

// requiredThresholdCheck is a synthetic check whose threshold has no usable
// catalog default, standing in for per-fleet values like expected RDMA device
// names.
func requiredThresholdCheck() health.CheckDefinition {
	return health.CheckDefinition{
		ID:              "required_threshold_check",
		Category:        health.Node,
		RequiredSignals: []string{"first"},
		Thresholds: map[string]health.ThresholdSpec{
			"expected_first": {
				Value:            int64(0),
				Category:         health.DefaultThreshold,
				RequiredOverride: true,
			},
		},
		Rules: []health.Rule{{
			Expression: "first != expected_first",
			Severity:   health.Critical,
			Message:    "first ${first} != expected ${expected_first}",
		}},
	}
}

// A Required threshold the operator has not supplied leaves the check unable
// to run: it skips rather than evaluating against a placeholder default, so a
// configuration gap never reports as a node fault.
func TestEvaluate_SkipsUnconfiguredRequiredThreshold(t *testing.T) {
	cat := catalog.New(catalog.WithArchitecture(catalogtest.DummyArchitecture))
	if err := cat.Register(requiredThresholdCheck()); err != nil {
		t.Fatal(err)
	}

	v, err := cat.Evaluate("required_threshold_check", twoSignalMeasurement{First: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Skipped || v.SkipReason != health.ThresholdUnconfigured {
		t.Errorf("skipped=%v reason=%v, want skipped=true THRESHOLD_UNCONFIGURED", v.Skipped, v.SkipReason)
	}
	if !strings.Contains(v.SkipDetail, "expected_first") {
		t.Errorf("skip detail = %q, want the unconfigured threshold named", v.SkipDetail)
	}
}

// Once supplied, the Required threshold behaves like any other: the check
// evaluates against the operator's value.
func TestEvaluate_RunsConfiguredRequiredThreshold(t *testing.T) {
	cat := catalog.New(
		catalog.WithArchitecture(catalogtest.DummyArchitecture),
		catalog.WithOverrides(catalog.NewOverrideSet(catalog.Override{
			CheckID:       "required_threshold_check",
			ThresholdName: "expected_first",
			Value:         int64(1),
		})),
	)
	if err := cat.Register(requiredThresholdCheck()); err != nil {
		t.Fatal(err)
	}

	v, err := cat.Evaluate("required_threshold_check", twoSignalMeasurement{First: 1})
	if err != nil {
		t.Fatal(err)
	}
	if v.Skipped {
		t.Fatalf("check skipped (%v), want evaluated", v.SkipReason)
	}
	if v.Severity != health.Pass {
		t.Errorf("severity = %v, want PASS", v.Severity)
	}
}

// scopedCheck is a synthetic DomainScope check. The table below drives it down
// all four paths that build a verdict from a definition: pass, matched,
// skipped, and errored.
func scopedCheck() health.CheckDefinition {
	return health.CheckDefinition{
		ID:                "scoped_check",
		Category:          health.Node,
		ConditionScope:    health.DomainScope,
		RequiredSignals:   []string{"first"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "first < 0", Severity: health.Critical, Message: "negative"},
		},
	}
}

// TestEvaluate_ConditionScope drives scopedCheck down all four paths that
// build a verdict from a definition and asserts each one's severity, skip
// state, and ConditionScope.
func TestEvaluate_ConditionScope(t *testing.T) {
	tests := []struct {
		// measurement precedes name because fieldalignment scores pointer-scan
		// bytes, not padding: an 8-byte func ahead of a 16-byte string keeps the
		// scanned span at 16.
		measurement        func() twoSignalMeasurement
		name               string
		wantSeverity       health.Severity
		wantSkipReason     health.SkipReason
		wantConditionScope health.ConditionScope
		disableCheck       bool
		wantSkipped        bool
	}{
		{
			name:               "pass",
			measurement:        func() twoSignalMeasurement { return twoSignalMeasurement{First: 1} },
			wantSeverity:       health.Pass,
			wantConditionScope: health.DomainScope,
		},
		{
			// ConditionScope describes the check, not the observation, so a
			// skipped verdict still reports what that check would have
			// implicated. RecommendedAction cannot do this: a skipped verdict
			// carries none.
			name:               "disabled and skipped",
			disableCheck:       true,
			measurement:        func() twoSignalMeasurement { return twoSignalMeasurement{First: 1} },
			wantSkipped:        true,
			wantSkipReason:     health.CheckDisabled,
			wantConditionScope: health.DomainScope,
		},
		{
			// A failure to observe is node-local whatever the check would have
			// implicated: the rules never ran, so nothing off-node was seen.
			// Reporting the check's declared scope here would route a local
			// sensor failure to the fabric owner, the same mistake the
			// collection-failure action already avoids.
			name: "collection failure",
			measurement: func() twoSignalMeasurement {
				m := twoSignalMeasurement{First: 1}
				m.AddError(errors.New("sensor unavailable"), "first")
				return m
			},
			wantSeverity:       health.Critical,
			wantConditionScope: health.NodeScope,
		},
		{
			// A matched rule is a real detected condition, so it carries the
			// check's declared scope. This is the path where blast radius
			// drives triage.
			name:               "matched rule",
			measurement:        func() twoSignalMeasurement { return twoSignalMeasurement{First: -1} },
			wantSeverity:       health.Critical,
			wantConditionScope: health.DomainScope,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := []catalog.Option{
				catalog.WithArchitecture(catalogtest.DummyArchitecture),
				catalog.WithExecutionContext(health.OperatorInfra),
			}
			if tt.disableCheck {
				opts = append(opts, catalog.WithDisabledChecks("scoped_check"))
			}
			cat := catalog.NewEmpty(opts...)
			cat.MustRegister(scopedCheck())

			v, err := cat.Evaluate("scoped_check", tt.measurement())
			if err != nil {
				t.Fatal(err)
			}
			if v.Skipped != tt.wantSkipped || v.SkipReason != tt.wantSkipReason {
				t.Fatalf("skipped=%v reason=%v, want skipped=%v reason=%v", v.Skipped, v.SkipReason, tt.wantSkipped, tt.wantSkipReason)
			}
			if !tt.wantSkipped && v.Severity != tt.wantSeverity {
				t.Fatalf("severity = %v, want %v", v.Severity, tt.wantSeverity)
			}
			if v.ConditionScope != tt.wantConditionScope {
				t.Errorf("ConditionScope = %v, want %v", v.ConditionScope, tt.wantConditionScope)
			}
		})
	}
}

// A dependent that read its signals and failed its own rule is an
// independently observed fault, not a restatement of the dependency's.
func TestEvaluateAll_RuleFailureUnderFailedDependencySurvives(t *testing.T) {
	cat := catalog.New(catalog.WithArchitecture("GB200"))

	// gpu_count is Critical on the shortfall; the GPU that is present reports a
	// real thermal slowdown of its own.
	verdict, err := cat.EvaluateAll(
		node.NodeMeasurement{GPUCount: 3},
		gpu.GPUMeasurement{
			GPUID: "GPU-0", AveragePowerDrawW: 400, PowerLimitW: 1200,
			EnforcedPowerLimitW: 1200, HWThermalSlowdown: true,
		},
	)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}

	for _, v := range verdict.CheckResults {
		if v.CheckID != "gpu_sensor_throttle" || v.Severity != health.Critical {
			continue
		}
		if strings.Contains(strings.Join(v.Messages, " "), "gpu_count") {
			t.Errorf("a rule failure was attributed to gpu_count: %v", v.Messages)
		}
		return
	}
	t.Error("gpu_sensor_throttle did not report the throttle; a rule failure must survive")
}

// Disabling one check must not disable its dependents. Checks never consume
// each other's results, so a check the operator turned off says nothing about
// whether another check can read its own signals.
func TestEvaluateAll_DisablingRootKeepsDependents(t *testing.T) {
	cat := catalog.New(
		catalog.WithArchitecture("GB200"),
		catalog.WithDisabledChecks("gpu_count"),
	)

	verdict, err := cat.EvaluateAll(
		node.NodeMeasurement{GPUCount: 4},
		gpu.GPUMeasurement{GPUID: "GPU-0", HWThermalSlowdown: true},
	)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	if verdict.OverallSeverity != health.Critical {
		t.Errorf("overall = %v, want CRITICAL: a throttling GPU must not read PASS "+
			"because gpu_count was disabled", verdict.OverallSeverity)
	}
}

// healthyGPU is a GPU measurement that passes every built-in GPU check on
// GB200, so a test can tell a real domain failure from a fixture artifact.
func healthyGPU(id string) gpu.GPUMeasurement {
	return gpu.GPUMeasurement{
		GPUID:               id,
		AveragePowerDrawW:   400,
		PowerLimitW:         1200,
		EnforcedPowerLimitW: 1200,
		GPUPstate:           0,
	}
}

// The reported scenario: one of four GPUs off the bus. Every dependent names
// gpu_count as the cause instead of restating the same fault as an unexplained
// collection failure, and the GPU domain stays visible.
func TestEvaluateAll_LostGPUNamesGPUCount(t *testing.T) {
	cat := catalog.New(catalog.WithArchitecture("GB200"))
	cause := errors.New("device handle unavailable")

	verdict, err := cat.EvaluateAll(
		node.NodeMeasurement{GPUCount: 3},
		healthyGPU("GPU-0"), healthyGPU("GPU-1"), healthyGPU("GPU-2"),
		gpu.ErroredGPU("", cause, gpu.MeasurementSignals...),
		nvlink.NVLinkMeasurement{GPUID: "GPU-0"},
		nvlink.ErroredNVLink("", cause, nvlink.MeasurementSignals...),
	)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}

	named := map[string]int{}
	for _, v := range verdict.CheckResults {
		if v.Severity != health.Critical {
			continue
		}
		msg := strings.Join(v.Messages, " ")
		if strings.Contains(msg, "gpu_count") {
			named[v.CheckID]++
			if !strings.Contains(msg, "device handle unavailable") {
				t.Errorf("%s = %q, want the collection cause kept alongside the cause", v.CheckID, msg)
			}
		}
	}
	for _, id := range []string{
		"gpu_average_power_draw", "gpu_power_limit", "gpu_power_state", "gpu_sensor_throttle",
		"nvlink_active", "nvlink_bandwidth_count_speed", "nvlink_capabilities",
		"nvlink_fabric", "nvlink_fabric_health", "nvlink_p2p",
	} {
		if named[id] != 1 {
			t.Errorf("%s verdicts naming gpu_count = %d, want 1 for the one lost device", id, named[id])
		}
	}
	if verdict.SeverityByDomain[health.GPU] != health.Critical {
		t.Errorf("severity_by_domain[gpu] = %v, want CRITICAL", verdict.SeverityByDomain[health.GPU])
	}
}

// A partial per-signal read failure on a GPU that enumerated fine is a
// different event from a device falling off the bus. gpu_count explains the
// second and not the first, so only whole-entity loss is annotated.
func TestEvaluateAll_PartialSignalFailureIsNotAttributedToDependency(t *testing.T) {
	cat := catalog.New(catalog.WithArchitecture("GB200"))

	// Slot 3 off the bus: every signal unreadable.
	lost := gpu.ErroredGPU("", errors.New("device handle unavailable"), gpu.MeasurementSignals...)

	// GPU-1 enumerated fine; one power read failed on its own.
	partial := healthyGPU("GPU-1")
	partial.AddError(errors.New("DeviceGetEnforcedPowerLimit: ERROR_UNKNOWN"),
		gpu.SignalEnforcedPowerLimitW)

	verdict, err := cat.EvaluateAll(
		node.NodeMeasurement{GPUCount: 3},
		healthyGPU("GPU-0"), partial, lost,
	)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}

	var own, annotated int
	for _, v := range verdict.CheckResults {
		if v.CheckID != "gpu_power_limit" || v.Severity != health.Critical {
			continue
		}
		msg := strings.Join(v.Messages, " ")
		switch {
		case strings.Contains(msg, "gpu_count"):
			annotated++
			if strings.Contains(msg, "DeviceGetEnforcedPowerLimit") {
				t.Errorf("an unrelated partial failure was attributed to gpu_count: %q", msg)
			}
		case strings.Contains(msg, "DeviceGetEnforcedPowerLimit"):
			own++
		}
	}
	if own != 1 {
		t.Errorf("gpu_power_limit verdicts reporting the partial failure on its own = %d, want 1", own)
	}
	if annotated != 1 {
		t.Errorf("gpu_power_limit verdicts naming gpu_count = %d, want 1 for the lost device", annotated)
	}
}

// A collection failure is classified by every message carrying the
// "measurement unavailable" prefix. Naming the dependency must extend that
// message, not add a second one, or a consumer reads the verdict as an
// observed fault and recommends hardware remediation.
func TestEvaluateAll_CollectionFailureKeepsItsPrefixOnEveryMessage(t *testing.T) {
	cat := catalog.New(catalog.WithArchitecture("GB200"))
	cause := errors.New("device handle unavailable")

	verdict, err := cat.EvaluateAll(
		node.NodeMeasurement{GPUCount: 3},
		healthyGPU("GPU-0"),
		gpu.ErroredGPU("", cause, gpu.MeasurementSignals...),
	)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}

	var checked int
	for _, v := range verdict.CheckResults {
		if v.Severity != health.Critical {
			continue
		}
		if !strings.Contains(strings.Join(v.Messages, " "), "gpu_count") {
			continue
		}
		checked++
		for _, m := range v.Messages {
			if !strings.HasPrefix(m, "measurement unavailable: ") {
				t.Errorf("%s message %q lacks the collection-failure prefix", v.CheckID, m)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no annotated collection failure in the verdict")
	}
}

// A dependency that reported Critical because its own measurement could not be
// collected explains nothing: it observed no fault. Only a rule match, which is
// the path that records ObservedValues, licenses the claim.
func TestEvaluateAll_UnobservedDependencyExplainsNothing(t *testing.T) {
	cat := catalog.New(catalog.WithArchitecture("GB200"))
	cause := errors.New("nvml init failed")

	// The whole NVML source is wedged: gpu_count is Critical from its own
	// errored measurement rather than from a count it actually read.
	verdict, err := cat.EvaluateAll(
		node.ErroredNode(cause, node.MeasurementSignals...),
		gpu.ErroredGPU("", cause, gpu.MeasurementSignals...),
	)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	for _, v := range verdict.CheckResults {
		if strings.Contains(strings.Join(v.Messages, " "), "explained by dependency") {
			t.Errorf("%s credits gpu_count, which observed nothing: %v", v.CheckID, v.Messages)
		}
	}
}

// gpu_count is Blackwell-only while six dependents also declare H100, H200 or
// A100. There it is architecture-skipped, never reports Critical, and each
// dependent reports its own collection failure unannotated. AGENTS.md documents
// this as the scope limit; this pins it.
func TestEvaluateAll_NoAnnotationWhereGPUCountDoesNotApply(t *testing.T) {
	cat := catalog.New(catalog.WithArchitecture("H100"))
	cause := errors.New("device handle unavailable")

	verdict, err := cat.EvaluateAll(
		healthyGPU("GPU-0"),
		gpu.ErroredGPU("", cause, gpu.MeasurementSignals...),
	)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}

	var reported bool
	for _, v := range verdict.CheckResults {
		msg := strings.Join(v.Messages, " ")
		if strings.Contains(msg, "gpu_count") {
			t.Errorf("%s names gpu_count on H100, where it does not run: %q", v.CheckID, msg)
		}
		if v.CheckID == "gpu_power_limit" && strings.Contains(msg, "device handle unavailable") {
			reported = true
		}
	}
	if !reported {
		t.Error("gpu_power_limit did not report its collection failure on H100")
	}
}

// With several dependencies, the first declared one that reported Critical is
// named. Nothing built in declares more than one, so this records the rule.
func TestEvaluateAll_MultipleDependenciesNamesFirstDeclared(t *testing.T) {
	root := func(id string) health.CheckDefinition {
		return health.CheckDefinition{
			ID: id, Category: health.Node,
			RequiredSignals:   []string{"gpu_count"},
			ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
			Thresholds:        map[string]health.ThresholdSpec{},
			Rules: []health.Rule{
				{Expression: "gpu_count < 4", Severity: health.Critical, Message: "short"},
			},
		}
	}
	cat := catalog.NewEmpty(
		catalog.WithArchitecture(catalogtest.DummyArchitecture),
		catalog.WithExecutionContext(health.OperatorInfra),
	)
	cat.MustRegister(root("first_root"))
	cat.MustRegister(root("second_root"))
	cat.MustRegister(health.CheckDefinition{
		ID: "two_deps", Category: health.GPU,
		RequiredSignals:   []string{"gpu_id"},
		Dependencies:      []string{"first_root", "second_root"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "gpu_id == ''", Severity: health.Critical, Message: "no id"},
		},
	})

	verdicts, err := cat.EvaluateAllRaw(
		node.NodeMeasurement{GPUCount: 3},
		gpu.ErroredGPU("", errors.New("handle unavailable"), gpu.MeasurementSignals...),
	)
	if err != nil {
		t.Fatalf("EvaluateAllRaw: %v", err)
	}
	for _, v := range verdicts {
		if v.CheckID != "two_deps" || v.Severity != health.Critical {
			continue
		}
		if !strings.Contains(strings.Join(v.Messages, " "), "first_root") {
			t.Errorf("messages = %v, want the first declared failing dependency", v.Messages)
		}
		return
	}
	t.Error("two_deps produced no Critical verdict")
}

// Evaluate runs one check alone, so there is no sibling verdict to name and
// Dependencies has no effect. catalog.go documents this contract.
func TestEvaluate_SingleCheckIgnoresDependencies(t *testing.T) {
	cat := catalog.New(catalog.WithArchitecture("GB200"))
	lost := gpu.ErroredGPU("", errors.New("device handle unavailable"), gpu.MeasurementSignals...)

	v, err := cat.Evaluate("gpu_power_limit", lost)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Severity != health.Critical {
		t.Errorf("severity = %v, want CRITICAL", v.Severity)
	}
	if got := strings.Join(v.Messages, " "); strings.Contains(got, "gpu_count") {
		t.Errorf("messages = %q, want no attribution on the single-check path", got)
	}
}

// The annotated verdict's exact shape: one message, the collection prefix, the
// cause, then the dependency clause, and the collection action preserved.
func TestEvaluateAll_AnnotatedVerdictShape(t *testing.T) {
	cat := catalog.New(catalog.WithArchitecture("GB200"))

	verdict, err := cat.EvaluateAll(
		node.NodeMeasurement{GPUCount: 3},
		healthyGPU("GPU-0"),
		gpu.ErroredGPU("", errors.New("device handle unavailable"), gpu.MeasurementSignals...),
	)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}

	const want = `measurement unavailable: device handle unavailable; ` +
		`explained by dependency "gpu_count", itself CRITICAL`
	for _, v := range verdict.CheckResults {
		if v.CheckID != "gpu_power_limit" || v.Severity != health.Critical {
			continue
		}
		if len(v.Messages) != 1 {
			t.Fatalf("messages = %v, want exactly one", v.Messages)
		}
		if v.Messages[0] != want {
			t.Errorf("message =\n  %q\nwant\n  %q", v.Messages[0], want)
		}
		if !slices.Contains(v.RecommendedAction.RemediationActions,
			health.RemediationActionCheckCollection) {
			t.Errorf("action = %v, want the collection-failure action preserved",
				v.RecommendedAction.RemediationActions)
		}
		return
	}
	t.Error("no annotated gpu_power_limit verdict")
}

// Unknown ranks above Critical and means the check could not complete, so a
// dependency that reported it explains a dependent just as a Critical does.
func TestEvaluateAll_UnknownDependencyExplains(t *testing.T) {
	root := health.CheckDefinition{
		ID: "unknown_root", Category: health.Node,
		RequiredSignals:   []string{"gpu_count"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "gpu_count < 4", Severity: health.Unknown, Message: "cannot assess"},
		},
	}
	dependent := health.CheckDefinition{
		ID: "dependent", Category: health.GPU,
		RequiredSignals:   []string{"gpu_id"},
		Dependencies:      []string{"unknown_root"},
		ExecutionContexts: []health.ExecutionContext{health.OperatorInfra},
		Thresholds:        map[string]health.ThresholdSpec{},
		Rules: []health.Rule{
			{Expression: "gpu_id == ''", Severity: health.Critical, Message: "no id"},
		},
	}
	cat := catalog.NewEmpty(
		catalog.WithArchitecture(catalogtest.DummyArchitecture),
		catalog.WithExecutionContext(health.OperatorInfra),
	)
	cat.MustRegister(root)
	cat.MustRegister(dependent)

	verdicts, err := cat.EvaluateAllRaw(
		node.NodeMeasurement{GPUCount: 3},
		gpu.ErroredGPU("", errors.New("handle unavailable"), gpu.MeasurementSignals...),
	)
	if err != nil {
		t.Fatalf("EvaluateAllRaw: %v", err)
	}
	for _, v := range verdicts {
		if v.CheckID != "dependent" {
			continue
		}
		msg := strings.Join(v.Messages, " ")
		if !strings.Contains(msg, "unknown_root") {
			t.Errorf("messages = %v, want the Unknown dependency named", v.Messages)
		}
		// The severity is the dependency's own, not a hardcoded CRITICAL.
		if !strings.Contains(msg, "itself UNKNOWN") {
			t.Errorf("messages = %v, want the dependency's actual severity", v.Messages)
		}
		return
	}
	t.Error("dependent produced no verdict")
}
