package toolkit

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/dsx-ai-factory/health-validation/catalog/infiniband"
)

// ibstat structures its output as a channel adapter header followed by an
// indented block per port:
//
//	CA 'mlx5_0'
//		CA type: MT4129
//		Number of ports: 1
//		Port 1:
//			State: Active
//			Physical state: LinkUp
//			Rate: 400
//			Link layer: InfiniBand
//
// Indentation is not load-bearing here — every line is trimmed and the two
// headers delimit the blocks — so the parser is insensitive to the tabs vs.
// spaces difference between infiniband-diags releases.
var (
	ibstatCARE   = regexp.MustCompile(`^CA\s+'([^']+)'`)
	ibstatPortRE = regexp.MustCompile(`^Port\s+(\d+):`)
)

// ibstatPort accumulates one port block. The saw* flags distinguish a field
// ibstat never printed from one it printed as a zero value, so a truncated
// or unrecognized block is reported as an unread signal rather than as a
// port that is genuinely down at 0 Gb/s.
type ibstatPort struct {
	infiniband.PortMeasurement
	sawState bool
	sawPhys  bool
	sawRate  bool
}

// ParseIBStat turns ibstat's output into one [infiniband.PortMeasurement]
// per port across every channel adapter, in the order ibstat reported them.
// Lines outside a port block, and keys no check consumes, are ignored.
//
// Unlike the journal parsers, an empty result is meaningful and is not an
// error: a host with no InfiniBand adapters produces no ports, and the
// caller emits no measurements so the port checks are skipped rather than
// failed. A port block missing a field a check requires yields a
// measurement with that signal marked errored, which fails only the checks
// that needed it.
func ParseIBStat(lines []string) []infiniband.PortMeasurement {
	var (
		ports  []infiniband.PortMeasurement
		device string
		cur    *ibstatPort
	)

	flush := func() {
		if cur == nil {
			return
		}
		ports = append(ports, finishIBStatPort(cur))
		cur = nil
	}

	for _, line := range lines {
		field := strings.TrimSpace(line)

		if g := ibstatCARE.FindStringSubmatch(field); g != nil {
			flush()
			device = g[1]
			continue
		}
		if g := ibstatPortRE.FindStringSubmatch(field); g != nil {
			flush()
			n, err := strconv.ParseInt(g[1], 10, 64)
			if err != nil {
				continue
			}
			cur = &ibstatPort{PortMeasurement: infiniband.PortMeasurement{Device: device, Port: n}}
			continue
		}
		if cur == nil {
			continue
		}

		key, value, ok := strings.Cut(field, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "state":
			cur.State, cur.sawState = value, true
		case "physical state":
			cur.PhysicalState, cur.sawPhys = value, true
		case "rate":
			// A rate is fractional at the slowest link widths (2.5 for SDR
			// 1x), so an unparseable value is a format the parser does not
			// understand, not a slow link.
			rate, err := strconv.ParseFloat(value, 64)
			if err != nil {
				continue
			}
			cur.RateGbps, cur.sawRate = rate, true
		case "link layer":
			cur.LinkLayer = value
		}
	}
	flush()

	return ports
}

// finishIBStatPort marks each required field ibstat did not report as an
// unread signal, so its checks fail on the missing data while the checks
// fed by the fields that did arrive still evaluate.
func finishIBStatPort(p *ibstatPort) infiniband.PortMeasurement {
	missing := func(field, signal string) {
		p.AddError(fmt.Errorf("ibstat reported no %s for %s port %d", field, p.Device, p.Port), signal)
	}
	if !p.sawState {
		missing("State", infiniband.SignalIBState)
	}
	if !p.sawPhys {
		missing("Physical state", infiniband.SignalIBPhysicalState)
	}
	if !p.sawRate {
		missing("Rate", infiniband.SignalIBRateGbps)
	}
	return p.PortMeasurement
}
