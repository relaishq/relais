package observe

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// canonicalFields derives names from the destination types. encoding/json's
// case-insensitive fallback must not overwrite evidence validated by exact
// presence paths. Map keys and unknown extension fields keep their own names.
func canonicalFields(data json.RawMessage, typ reflect.Type) error {
	if typ == nil {
		return nil // json.Unmarshal reports an invalid destination.
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if reflect.PointerTo(typ).Implements(reflect.TypeFor[json.Unmarshaler]()) || reflect.PointerTo(typ).Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) {
		return nil // RawMessage and scalar product types own their decoding.
	}
	switch typ.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(data, &object); err != nil {
			return err
		}
		for _, field := range reflect.VisibleFields(typ) {
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if field.PkgPath != "" || name == "-" {
				continue
			}
			if name == "" {
				embedded := field.Type
				if embedded.Kind() == reflect.Pointer {
					embedded = embedded.Elem()
				}
				if field.Anonymous && embedded.Kind() == reflect.Struct {
					continue // Promoted fields are included by VisibleFields.
				}
				name = field.Name
			}
			for key, value := range object {
				if key == name {
					if err := canonicalFields(value, field.Type); err != nil {
						return err
					}
				} else if strings.EqualFold(key, name) {
					return fmt.Errorf("noncanonical field %q; expected %q", key, name)
				}
			}
		}
	case reflect.Slice, reflect.Array:
		if typ.Kind() == reflect.Slice && typ.Elem().Kind() == reflect.Uint8 {
			return nil // encoding/json represents byte slices as base64 strings.
		}
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := canonicalFields(value, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		var values map[string]json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := canonicalFields(value, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
