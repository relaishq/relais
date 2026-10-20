package mediaworker

import (
	"fmt"

	"github.com/relaishq/relais/internal/componentcodec"
	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// The v10 profile reserves hot independently of setup and history.
// Count b/m/t and framing once, and each component's encryption overhead once.
const componentMaxHistoryBytes = componentcodec.DecodeLimit - 6

func splitComponentBudgets(hot, history int) componentBudgets {
	history = max(8448, history+6+componentCipherOverhead)
	return componentBudgets{Hot: hot, Setup: 4096, History: history, Total: max(20480, hot+4096+history)}
}

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
	return size, b.admitSizes(size)
}

func (b componentBudgets) admitSizes(size componentSizes) error {
	for _, part := range []struct {
		name      string
		size, cap int
	}{{"hot", size.Hot, b.Hot}, {"setup", size.Setup, b.Setup}, {"history", size.History, b.History}, {"transaction", size.Total, b.Total}} {
		if part.cap <= 0 || part.size > part.cap {
			return fmt.Errorf("%w: v10 %s encoding %d bytes exceeds %d", agent.ErrCapacity, part.name, part.size, part.cap)
		}
	}
	return nil
}
