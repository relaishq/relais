package operatorrehearsal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/relaishq/relais/internal/relayloss"
)

// runRelayLoss uses tested-head components. The pinned releases used by the
// four comparison gates predate relay sets. The existing netns driver owns the
// isolated network, Redis and processes, and blackholes both host directions.
func (r *rehearsal) runRelayLoss(ctx context.Context) *relayloss.Report {
	path := filepath.Join(r.work, "relay-loss.json")
	bin := filepath.Dir(r.bridge)
	redis := r.c.RedisExecutable
	if redis == "" {
		redis = "redis-server"
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	result := r.runner.Run(bounded, Command{Label: "relay-host-loss", Args: []string{
		filepath.Join(bin, "crash-run"), "-bin=" + bin, "-topology=netns",
		"-relay-loss-netns", "-runs=1", "-after=5s", "-relay-loss-report=" + path, "-redis-server=" + redis,
	}})
	// Raw child diagnostics are private to its owned run directory. Only the
	// fixed report projection reaches public artifacts.
	report := &relayloss.Report{}
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, report)
	}
	if err != nil || result.Err != nil || result.Code == nil || *result.Code != 0 || result.Truncated {
		report.Completed = false
		if report.FailedPhase == "" {
			report.FailedPhase = "completion"
		}
	}
	if err != nil {
		report.FailedPhase = "unavailable"
	}
	report.FailedPhase = relayloss.PublicPhase(report.FailedPhase)
	_, _ = fmt.Fprintf(r.diagnostics, "relay-host-loss failed_phase=%s completed=%t calls=%d passed=%t\n", report.FailedPhase, report.Completed, len(report.Calls), report.Passed())
	return report
}
