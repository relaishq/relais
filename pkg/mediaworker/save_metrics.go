package mediaworker

import (
	"context"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

func (s *session) measuredSave(ctx context.Context, app agent.Agent) (agent.State, error) {
	finishCPU := beginSaveCPU()
	started := time.Now()
	state, err := app.Save(ctx)
	elapsed := time.Since(started)
	cpu, known := finishCPU()
	m := &s.worker.metrics
	m.saveCalls.Add(1)
	m.saveWallNS.Add(uint64(elapsed))
	if known {
		m.saveCPUNS.Add(cpu)
	} else {
		m.saveCPUUnknown.Add(1)
	}
	return state, err
}
