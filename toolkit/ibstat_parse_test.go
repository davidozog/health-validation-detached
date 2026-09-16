package toolkit_test

import (
	"strings"
	"testing"

	"github.com/dsx-ai-factory/health-validation/toolkit"
)

// twoAdapterOutput is a two-port, two-adapter ibstat capture: mlx5_0 is a
// healthy NDR fabric port, mlx5_1 never trained.
const twoAdapterOutput = `CA 'mlx5_0'
	CA type: MT4129
	Number of ports: 1
	Firmware version: 28.39.1002
	Hardware version: 0
	Node GUID: 0xa088c20300bb1234
	System image GUID: 0xa088c20300bb1234
	Port 1:
		State: Active
		Physical state: LinkUp
		Rate: 400
		Base lid: 235
		LMC: 0
		SM lid: 3
		Capability mask: 0xa651e848
		Port GUID: 0xa088c20300bb1234
		Link layer: InfiniBand
CA 'mlx5_1'
	CA type: MT4129
	Number of ports: 1
	Port 1:
		State: Down
		Physical state: Polling
		Rate: 10
		Base lid: 65535
		LMC: 0
		SM lid: 0
		Capability mask: 0xa651e848
		Port GUID: 0xa088c20300bb5678
		Link layer: InfiniBand`

func parse(t *testing.T, output string) []portView {
	t.Helper()
	var views []portView
	for _, m := range toolkit.ParseIBStat(strings.Split(output, "\n")) {
		views = append(views, portView{
			device: m.Device, port: m.Port,
			state: m.State, physical: m.PhysicalState,
			rate: m.RateGbps, linkLayer: m.LinkLayer,
			errCount: len(m.Errors()),
		})
	}
	return views
}

type portView struct {
	device    string
	state     string
	physical  string
	linkLayer string
	rate      float64
	port      int64
	errCount  int
}

func TestParseIBStat_AdaptersAndPorts(t *testing.T) {
	got := parse(t, twoAdapterOutput)
	want := []portView{
		{device: "mlx5_0", port: 1, state: "Active", physical: "LinkUp", rate: 400, linkLayer: "InfiniBand"},
		{device: "mlx5_1", port: 1, state: "Down", physical: "Polling", rate: 10, linkLayer: "InfiniBand"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d ports, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("port %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A dual-port adapter keeps its device name across both port blocks.
func TestParseIBStat_MultiPortAdapter(t *testing.T) {
	got := parse(t, `CA 'mlx5_bond_0'
	Number of ports: 2
	Port 1:
		State: Active
		Physical state: LinkUp
		Rate: 200
		Link layer: Ethernet
	Port 2:
		State: Active
		Physical state: LinkUp
		Rate: 200
		Link layer: Ethernet`)

	if len(got) != 2 {
		t.Fatalf("parsed %d ports, want 2: %+v", len(got), got)
	}
	for i, p := range got {
		if p.device != "mlx5_bond_0" || p.port != int64(i+1) {
			t.Errorf("port %d = %s/%d, want mlx5_bond_0/%d", i, p.device, p.port, i+1)
		}
		if p.linkLayer != "Ethernet" {
			t.Errorf("port %d link layer = %q, want Ethernet preserved for RoCE ports", i, p.linkLayer)
		}
	}
}

// The slowest widths report a fractional rate, so truncating to an integer
// would silently rewrite the measurement.
func TestParseIBStat_FractionalRate(t *testing.T) {
	got := parse(t, `CA 'mlx5_0'
	Port 1:
		State: Active
		Physical state: LinkUp
		Rate: 2.5
		Link layer: InfiniBand`)

	if len(got) != 1 || got[0].rate != 2.5 {
		t.Fatalf("parsed %+v, want a single port at 2.5 Gb/s", got)
	}
}

// A host with no adapters is not a failure: ibstat prints nothing.
func TestParseIBStat_NoAdapters(t *testing.T) {
	if got := toolkit.ParseIBStat(nil); len(got) != 0 {
		t.Errorf("ParseIBStat(nil) = %+v, want no ports", got)
	}
}

// A field ibstat never printed becomes an errored signal rather than a
// zero value the checks would read as a real (and alarming) measurement.
func TestParseIBStat_MissingFieldsErrorTheirSignals(t *testing.T) {
	ports := toolkit.ParseIBStat(strings.Split(`CA 'mlx5_0'
	Port 1:
		State: Active
		Link layer: InfiniBand`, "\n"))

	if len(ports) != 1 {
		t.Fatalf("parsed %d ports, want 1", len(ports))
	}
	var failed []string
	for _, e := range ports[0].Errors() {
		failed = append(failed, e.FailedSignals()...)
	}
	want := map[string]bool{"ib_physical_state": true, "ib_rate_gbps": true}
	if len(failed) != len(want) {
		t.Fatalf("failed signals = %v, want %v", failed, want)
	}
	for _, s := range failed {
		if !want[s] {
			t.Errorf("unexpected failed signal %q; State was reported and must not be marked failed", s)
		}
	}
}

// Lines before the first adapter, and keys no check consumes, are ignored
// rather than mistaken for port fields.
func TestParseIBStat_IgnoresPreambleAndUnknownKeys(t *testing.T) {
	got := parse(t, `Fabric unit: something
CA 'mlx5_0'
	CA type: MT4129
	Port 1:
		State: Active
		Physical state: LinkUp
		Rate: 400
		Capability mask: 0xa651e848
		Link layer: InfiniBand`)

	want := portView{device: "mlx5_0", port: 1, state: "Active", physical: "LinkUp", rate: 400, linkLayer: "InfiniBand"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("parsed %+v, want %+v", got, want)
	}
}
