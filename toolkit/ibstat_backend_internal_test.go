package toolkit

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/dsx-ai-factory/health-validation/catalog/infiniband"
)

func ibstatWith(run runFunc) IBStatBackend { return &ibstatBackend{run: run} }

// ibstat not being installed is how a node without InfiniBand looks. It
// must leave the port checks unassessed, not fail them.
func TestIBStatBackend_NotInstalled(t *testing.T) {
	b := ibstatWith(func(context.Context, ...string) ([]byte, error) {
		return nil, &exec.Error{Name: "ibstat", Err: exec.ErrNotFound}
	})

	ms, err := b.Measure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 0 {
		t.Errorf("got %d measurements, want none so the checks skip: %+v", len(ms), ms)
	}
}

// ibstat installed on a host with no adapters prints nothing and exits 0.
func TestIBStatBackend_NoAdapters(t *testing.T) {
	b := ibstatWith(func(context.Context, ...string) ([]byte, error) { return nil, nil })

	ms, err := b.Measure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 0 {
		t.Errorf("got %d measurements, want none: %+v", len(ms), ms)
	}
}

// ibstat present but failing is a real observation failure: the port state
// is unknown, so the checks must fail rather than silently pass.
func TestIBStatBackend_CommandFails(t *testing.T) {
	b := ibstatWith(func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("exit status 1")
	})

	ms, err := b.Measure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
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
		t.Errorf("failed signals = %d, want all %d: nothing about the port is known",
			got, len(infiniband.PortMeasurementSignals))
	}
}

func TestIBStatBackend_ParsesOutput(t *testing.T) {
	b := ibstatWith(func(context.Context, ...string) ([]byte, error) {
		return []byte("CA 'mlx5_0'\n\tPort 1:\n\t\tState: Active\n" +
			"\t\tPhysical state: LinkUp\n\t\tRate: 400\n\t\tLink layer: InfiniBand\n"), nil
	})

	ms, err := b.Measure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Fatalf("got %d measurements, want one per port", len(ms))
	}
	m := ms[0].(infiniband.PortMeasurement)
	if m.Device != "mlx5_0" || m.Port != 1 || m.State != "Active" || m.RateGbps != 400 {
		t.Errorf("measurement = %+v, want mlx5_0 port 1 Active at 400 Gb/s", m)
	}
}

// ibstat takes no arguments: the backend must not pass any.
func TestIBStatBackend_RunsWithoutArguments(t *testing.T) {
	var got []string
	b := ibstatWith(func(_ context.Context, args ...string) ([]byte, error) {
		got = args
		return nil, nil
	})
	if _, err := b.Measure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("ibstat args = %v, want none", got)
	}
}
