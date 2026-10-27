package observe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

func paths(data []byte, contract Contract) error {
	for _, field := range contract {
		if err := present(data, strings.Split(field, ".")); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
	}
	return nil
}

func present(data []byte, path []string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return fmt.Errorf("expected object")
	}
	key := path[0]
	optional := strings.HasPrefix(key, "~")
	key = strings.TrimPrefix(key, "~")
	nullable := strings.HasSuffix(key, "?")
	array := strings.HasSuffix(key, "[]")
	key = strings.TrimSuffix(strings.TrimSuffix(key, "?"), "[]")
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
