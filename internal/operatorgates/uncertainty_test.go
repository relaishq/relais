package operatorgates

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

func TestUnknownTransportDoesNotBecomeMalformedPayload(t *testing.T) {
	s, _ := fixtureSession(t)
	p := &responsePeer{response: operator.Reply{Transport: operator.Unknown}}
	_, _ = s.Exchange(context.Background(), p, operator.HostExecute, "shared", "status", nil, nil, time.Minute, func(operator.Reply) operator.Certainty { return operator.Unknown })
	rows := s.Uncertainties()
	if len(rows) != 1 || rows[0].Rule != "transport-unknown" {
		t.Fatal("empty transport reply obscured the certainty rule", rows)
	}
}

func TestUnknownCarriesRequestAndRule(t *testing.T) {
	for _, rule := range []string{"timeout", "truncated-output", "malformed-reply", "lost-reply"} {
		s, clock := fixtureSession(t)
		var logged []operator.Uncertainty
		s.SetUncertaintySink(func(u operator.Uncertainty) { logged = append(logged, u) })
		r := issue(t, s, operator.HostExecute, "status")
		clock.Advance(time.Second)
		response := reply(r, operator.Unknown)
		response.UncertaintyRule = rule
		if _, err := s.Accept(s.ID, response, operator.Unknown); err != nil {
			t.Fatal(err)
		}
		rows := s.Uncertainties()
		if len(rows) != 1 || len(logged) != 1 || rows[0] != logged[0] || rows[0].Kind != "status.read" || rows[0].Target != "shared" || rows[0].RequestID != r.ID || rows[0].Rule != rule || rows[0].RemainingMS != 59000 || rows[0].DeadlineUnixMS != r.DeadlineUnixMS {
			t.Fatal("unknown lacks self-describing certainty boundary", rows, logged)
		}
		if len(s.Final(nil).Uncertainties) != 1 {
			t.Fatal("unknown missing from final report")
		}
	}
}
