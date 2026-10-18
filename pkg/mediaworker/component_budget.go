package mediaworker

import (
	"fmt"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// No defaults are selected here. Slice A callers supply proposed budgets for
// measurement; slice #195 must admit actual encodings before enabling v10.
// Count b/m/t and framing once, and each component's encryption overhead once.
type componentBudgets struct{ Hot, Setup, History, Total int }
type componentSizes struct{ Hot, Setup, History, Total, References int }

func measureComponents(c sessionComponents, m componentMetadata) (componentSizes, error) {
	packed, err := m.marshal()
	if err != nil {
		return componentSizes{}, err
	}
	reserve := len(packed) + componentCommitTimeBytes + componentFieldNameBytes + componentProtocolReserve
	size := componentSizes{
		Hot:        len(c.Transport) + len(c.Work) + 2*componentCipherOverhead + reserve,
		Setup:      len(c.Setup) + componentCipherOverhead,
		History:    len(c.History) + componentCipherOverhead,
		References: reserve,
	}
	size.Total = size.Hot + size.Setup + size.History
	return size, nil
}
func (b componentBudgets) admit(c sessionComponents, m componentMetadata) (componentSizes, error) {
	size, err := measureComponents(c, m)
	if err != nil {
		return size, err
	}
	for _, part := range []struct {
		name      string
		size, cap int
	}{{"hot", size.Hot, b.Hot}, {"setup", size.Setup, b.Setup}, {"history", size.History, b.History}, {"transaction", size.Total, b.Total}} {
		if part.cap <= 0 || part.size > part.cap {
			return size, fmt.Errorf("%w: v10 %s encoding %d bytes exceeds %d", agent.ErrCapacity, part.name, part.size, part.cap)
		}
	}
	return size, nil
}
