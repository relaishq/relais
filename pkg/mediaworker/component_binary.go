package mediaworker

import (
	"fmt"

	"github.com/relaishq/relais/internal/componentcodec"
)

func marshalComponent(kind byte, value any) ([]byte, error) {
	b, err := componentcodec.Marshal(kind, value)
	if err != nil {
		err = fmt.Errorf("%w: %w", errBadState, err)
	}
	return b, err
}
func unmarshalComponent(data []byte, kind byte, value any) error {
	err := componentcodec.Unmarshal(data, kind, value)
	if err != nil {
		return fmt.Errorf("%w: %w", errBadState, err)
	}
	return nil
}
