package observe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
)

// Contract describes presence only. Product structs own values and JSON types.
// A trailing [] checks each element; ? permits null (product nullable slices).
// A leading ~ permits omission but still rejects an explicit null.
type Contract []string

// Decode bounds host JSON and rejects duplicates, including unknown nested
// fields, and noncanonical aliases of typed fields. Unknown members are
// otherwise allowed for forward product evolution.
func Decode(data []byte, value any, contract Contract) error {
	return DecodeLimit(data, value, contract, 256000)
}

// DecodeLimit shares the strict decoder with larger bounded raw-media evidence.
func DecodeLimit(data []byte, value any, contract Contract, limit int) error {
	if err := validateJSON(data, value, contract, limit); err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func validateJSON(data []byte, value any, contract Contract, limit int) error {
	if len(data) > limit {
		return fmt.Errorf("observation exceeds %d bytes", limit)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if err := unique(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	if err := Require(data, contract); err != nil {
		return err
	}
	if err := canonicalFields(data, reflect.TypeOf(value)); err != nil {
		return err
	}
	return nil
}
