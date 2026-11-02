package operatorrehearsal

import (
	"regexp"
	"strings"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/operator"
)

var fullCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)
var fullDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var cpuRow = regexp.MustCompile(`^[0-9]+,[0-9]+,[0-9]+,(Y|N)$`)

// Accept complete fixed P2/P3 codes, never arbitrary colon suffixes or classes.
func publicPreconditionCategory(s string) bool {
	s = strings.TrimPrefix(s, "precondition-failed:")
	switch s {
	case "inventory", "status", "clocks", "observation", "caller", "caller-receipt", "caller-evidence", "media-pool", "live-calls", "control-not-settled", "control-not-empty", "control-unavailable", "not-draining", "control-owner", "worker-not-empty", "not-empty", "signal-bracket",
		"oversized", "store-incomplete", "store-refused", "store-oversized", "store-incomplete-bulk", "store-invalid", "store-too-large", "store-scan-incomplete", "store-topology", "store-unknown-owner", "store-transfer", "store-owner", "local-inventory", "relay-unit-unknown", "invalid-target", "invalid-request", "deadline-before-submission",
		"value-error", "key-error", "type-error", "file-not-found-error", "permission-error", "timeout-error", "timeout-expired", "called-process-error", "connection-refused-error", "connection-reset-error", "url-error", "ssl-error", "ssl-cert-verification-error", "json-decode-error":
		return true
	}
	for _, slot := range Slots {
		if code, ok := strings.CutPrefix(s, slot+"-"); ok {
			switch code {
			case "shape", "participant", "manifest-identity", "architecture", "release-differs", "unavailable", "topology", "not-running", "service-config", "manifest", "archive", "boot-script", "unit-start", "binary", "budgets", "build-identity", "value-error", "key-error", "type-error", "file-not-found-error", "permission-error", "timeout-expired", "called-process-error", "not-alive", "draining", "not-empty", "local-inventory", "inventory-unknown":
				return true
			}
		}
	}
	return false
}

func publicVerdict(v operator.Verdict) operator.Verdict {
	switch v {
	case operator.Passed, operator.Failure, operator.Inconclusive, operator.NotRun:
		return v
	}
	return operator.NotRun
}
func publicCPUList(s string) string {
	cpus, e := ParseCPUList(s)
	if e != nil {
		return "unavailable"
	}
	return CPUList(cpus)
}
func publicTopology(s string) string {
	var rows []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if cpuRow.MatchString(line) {
			rows = append(rows, line)
		}
	}
	return strings.Join(rows, "\n")
}
func sanitizePool(p *operatorevidence.Summary) *operatorevidence.Summary {
	if p == nil {
		return nil
	}
	copy := p.PublicDiagnostics()
	copy.Status = publicVerdict(p.Status)
	return &copy
}
func (r *Report) sanitize() {
	r.Uncertainties = append([]operator.Uncertainty{}, r.Uncertainties...)
	for i := range r.Uncertainties {
		r.Uncertainties[i] = r.Uncertainties[i].Public()
	}
	for _, p := range []*string{&r.Commit, &r.HeadCommit} {
		if *p != "" && !fullCommit.MatchString(*p) {
			*p = "unavailable"
		}
	}
	for _, p := range []*string{&r.ScenarioSHA256, &r.TemplateSHA256, &r.BinarySHA256, &r.CallerSHA256} {
		if *p != "" && !fullDigest.MatchString(*p) {
			*p = "unavailable"
		}
	}
	r.JudgeSHA256 = append([]string{}, r.JudgeSHA256...)
	r.ReleaseCommits = append([]string{}, r.ReleaseCommits...)
	r.ArchiveSHA256 = append([]string{}, r.ArchiveSHA256...)
	for _, values := range [][]string{r.JudgeSHA256, r.ArchiveSHA256} {
		for i, s := range values {
			if !fullDigest.MatchString(s) {
				values[i] = "unavailable"
			}
		}
	}
	for i, s := range r.ReleaseCommits {
		if !fullCommit.MatchString(s) {
			r.ReleaseCommits[i] = "unavailable"
		}
	}
	r.CPU.Topology = publicTopology(r.CPU.Topology)
	r.CPU.DriverAffinity = publicCPUList(r.CPU.DriverAffinity)
	r.CPU.CallerAffinities = append([]string{}, r.CPU.CallerAffinities...)
	for i, s := range r.CPU.CallerAffinities {
		r.CPU.CallerAffinities[i] = publicCPUList(s)
	}
	r.CPU.Calibration = cleanCalibration(r.CPU.Calibration)
	r.CPU.Phases = append([]Calibration{}, r.CPU.Phases...)
	for i, c := range r.CPU.Phases {
		r.CPU.Phases[i] = cleanCalibration(c)
	}
	r.Pool = sanitizePool(r.Pool)
	r.Clocks = append([]clocks.Evidence{}, r.Clocks...)
	for i, e := range r.Clocks {
		rows := map[string]clocks.Sample{}
		for _, slot := range Slots {
			if s, ok := e.Clocks[slot]; ok {
				s.Tracking = ""
				if !regexp.MustCompile(`^[0-9]{19}$`).MatchString(s.MeasuredUnixNS) {
					s.MeasuredUnixNS = "unavailable"
				}
				rows[slot] = s
			}
		}
		r.Clocks[i].Clocks = rows
	}
	r.Gates = append([]GateReport{}, r.Gates...)
	for i := range r.Gates {
		g := &r.Gates[i]
		if !g.Gate.Valid() {
			g.Gate = "unavailable"
		}
		g.Status = publicVerdict(g.Status)
		g.Reason = publicCode(g.Reason)
		g.Category = publicCode(g.Category)
		for _, s := range []*string{&g.Process.RelayWindows, &g.Process.RelayLocalTiming, &g.Process.RelayConnectionInvariants} {
			if *s != "" && *s != "inconclusive" {
				*s = "unavailable"
			}
		}
		if g.Media != nil {
			m := *g.Media
			m.Status = publicVerdict(m.Status)
			diagnostics := (operatorevidence.Summary{UndecidedMetrics: m.UndecidedMetrics, PhaseDiagnostics: m.PhaseDiagnostics}).PublicDiagnostics()
			m.UndecidedMetrics, m.PhaseDiagnostics = diagnostics.UndecidedMetrics, diagnostics.PhaseDiagnostics
			g.Media = &m
		}
	}
	r.Errors = append([]string{}, r.Errors...)
	for i, s := range r.Errors {
		r.Errors[i] = publicCode(s)
	}
}
