package relayjudge

import (
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/relaishq/relais/pkg/operator"
)

func Run(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("relay-loss-judge-input")
	}
	f, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, operator.MaxBlobBytes+1))
	if err != nil {
		return err
	}
	in, err := DecodeInput(raw)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(Judge(in))
}
