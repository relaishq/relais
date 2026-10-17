package ledger

import (
	"context"
	"encoding/json"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/workerapp"
)

// StallName is a separate versioned diagnostic workload. Unlike Name, it waits
// for the first audio input, so a process driver can pause Redis before intents
// exist. The normal teaching registration and its state encoding are unchanged.
const StallName = "local-ledger/store-stall-v1"

type stallAgent struct {
	Started  bool                         `json:"started"`
	Outcomes map[agent.Turn]agent.Outcome `json:"outcomes,omitempty"`
}

func (a *stallAgent) Handle(_ context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.AudioInput && e.Input != nil && !a.Started {
		a.Started = true
		return agent.Effects{StartTasks: []agent.TaskSpec{
			{Step: "commit", Kind: TaskKind, Turn: 1, Class: agent.SideEffecting, Resume: agent.Retry, Request: []byte("commit after store recovery"), Timeout: 15 * time.Second},
			{Step: "expire", Kind: TaskKind, Turn: 2, Class: agent.SideEffecting, Resume: agent.Retry, Request: []byte("must never launch"), Timeout: 2 * time.Second},
		}}, nil
	}
	if e.Kind == agent.TaskMessage && e.Result != nil && e.Result.Final {
		if a.Outcomes == nil {
			a.Outcomes = map[agent.Turn]agent.Outcome{}
		}
		// This registration creates exactly these two turns; unknown input never
		// grows saved state or changes the versioned diagnostic contract.
		if e.Result.Turn == 1 || e.Result.Turn == 2 {
			a.Outcomes[e.Result.Turn] = e.Result.Outcome
		}
	}
	return agent.Effects{}, nil
}
func (a *stallAgent) Save(context.Context) (agent.State, error) {
	b, err := json.Marshal(a)
	return agent.State{Version: 1, Bytes: b}, err
}
func (a *stallAgent) Restore(_ context.Context, s agent.State) error {
	if s.Version != 1 {
		return agent.ErrVersion
	}
	return json.Unmarshal(s.Bytes, a)
}
func StallRegistration(origin string) workerapp.Registration {
	return workerapp.Registration{Name: StallName, Factory: func(agent.FactoryArgs) agent.Agent { return &stallAgent{} }, Tasks: map[string]agent.TaskHandler{TaskKind: Handler(origin)}}
}
