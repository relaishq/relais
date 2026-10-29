package observe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Require checks field presence only; Decode and DecodeLimit additionally
// enforce unique canonical members and product-owned JSON types.
func Require(data []byte, contract Contract) error {
	if !json.Valid(data) {
		return fmt.Errorf("invalid JSON")
	}
	for _, field := range contract {
		path := strings.Split(field, ".")
		for _, segment := range path {
			key := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(segment, "~"), "?"), "[]")
			if key == "" || strings.ContainsAny(key, "~?[]") {
				return fmt.Errorf("invalid presence path %q", field)
			}
		}
		if err := present(data, path); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
	}
	return nil
}

func present(data []byte, path []string) error {
	// A terminal array consumes the last segment before visiting its rows.
	if len(path) == 0 {
		if !json.Valid(data) {
			return fmt.Errorf("invalid JSON")
		}
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return fmt.Errorf("expected object")
	}
	key := path[0]
	optional := strings.HasPrefix(key, "~")
	key = strings.TrimPrefix(key, "~")
	nullable := strings.HasSuffix(key, "?")
	key = strings.TrimSuffix(key, "?")
	array := strings.HasSuffix(key, "[]")
	key = strings.TrimSuffix(key, "[]")
	if key == "" || strings.ContainsAny(key, "~?[]") {
		return fmt.Errorf("invalid presence path")
	}
	raw, ok := object[key]
	if !ok {
		if optional {
			return nil
		}
		return fmt.Errorf("missing field")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if nullable || array {
			return nil
		}
		return fmt.Errorf("null field")
	}
	if array {
		var rows []json.RawMessage
		if err := json.Unmarshal(raw, &rows); err != nil {
			return err
		}
		for _, row := range rows {
			if err := present(row, path[1:]); err != nil {
				return err
			}
		}
	} else if len(path) > 1 {
		return present(raw, path[1:])
	}
	return nil
}

func unique(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := t.(json.Delim); ok {
		keys := map[string]bool{}
		for d.More() {
			if delim == '{' {
				t, err = d.Token()
				if err != nil {
					return err
				}
				key, ok := t.(string)
				if !ok || keys[key] {
					return fmt.Errorf("duplicate or invalid JSON key")
				}
				keys[key] = true
			}
			if err = unique(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
	}
	return err
}
