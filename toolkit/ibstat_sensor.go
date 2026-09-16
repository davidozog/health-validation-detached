package toolkit

import (
	"context"

	"github.com/dsx-ai-factory/health-validation/catalog/health"
	"github.com/dsx-ai-factory/health-validation/catalog/infiniband"
)

// IBStatSensor collects the current state of every InfiniBand port through
// a pluggable backend. It implements [Sensor].
//
// It is the present-state counterpart to [RDMALinkFlapSensor], which reads
// the same domain's history out of the journal. Both emit InfiniBand
// measurements; their schemas are disjoint, so each of the domain's checks
// routes to exactly one of them.
type IBStatSensor struct {
	backend IBStatBackend
}

// NewIBStatSensor creates an InfiniBand port sensor backed by the given
// backend.
//
// For production:
//
//	sensor := toolkit.NewIBStatSensor(toolkit.IBStat())
//
// For testing, back it with [MockIBStat].
func NewIBStatSensor(backend IBStatBackend) *IBStatSensor {
	return &IBStatSensor{backend: backend}
}

func (s *IBStatSensor) Name() string { return "ibstat" }

// Measure forwards the context to the backend: ibstat is a cancellable
// command, so it honors the deadline (see [Sensor.Measure]).
func (s *IBStatSensor) Measure(ctx context.Context) ([]health.Measurement, error) {
	return s.backend.Measure(ctx)
}

// Errored reports a port of unknown identity as unobservable so the port
// checks fail Critical when collection could not run at all. This is the
// hard-failure path — a timeout or a backend error — and is distinct from
// the backend's "no InfiniBand on this host" result, which returns no
// measurements and leaves the checks skipped.
func (s *IBStatSensor) Errored(err error) []health.Measurement {
	return []health.Measurement{
		infiniband.ErroredPort("", 0, err, infiniband.PortMeasurementSignals...),
	}
}
