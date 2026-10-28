package observe

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

// IncompleteEvidence means unambiguous JSON failed the required-field contract.
// The decoded destination is quarantined: only independently valid failure facts
// may be used, never qualification, thresholds or population judgments.
type IncompleteEvidence struct{ Err error }

func (e *IncompleteEvidence) Error() string { return e.Err.Error() }
func (e *IncompleteEvidence) Unwrap() error { return e.Err }
func IsIncompleteEvidence(err error) bool {
	var incomplete *IncompleteEvidence
	return errors.As(err, &incomplete)
}

// DecodeEvidenceLimit checks the recursive contract BEFORE product decoding.
// Contract failures still decode failure facts; ambiguous members never do.
func DecodeEvidenceLimit(data []byte, value any, limit int) error {
	if err := validateJSON(data, value, nil, limit); err != nil {
		return err
	}
	contractErr := EvidenceFields(data, reflect.TypeOf(value))
	decodeErr := json.Unmarshal(data, value)
	if decodeErr != nil {
		// A custom scalar decoder can stop json.Unmarshal before later failure
		// facts. Decode siblings independently, still using the product types.
		destination := reflect.ValueOf(value)
		if destination.Kind() == reflect.Pointer && !destination.IsNil() {
			destination.Elem().SetZero()
			decodeEvidenceFacts(data, destination.Elem())
		}
	}
	if contractErr != nil || decodeErr != nil {
		return &IncompleteEvidence{errors.Join(contractErr, decodeErr)}
	}
	return decodeErr
}

// EvidenceFields derives every required member from the actual Go types. Tags
// control names, not optionality; all exceptions live in EvidenceOptional.
func EvidenceFields(data json.RawMessage, typ reflect.Type) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	return evidenceFields(value, typ, "$")
}
func evidenceFields(value any, typ reflect.Type, path string) error {
	if value == nil {
		return fmt.Errorf("required evidence %s missing or null", path)
	}
	if object, ok := value.(map[string]any); ok && len(object) == 0 {
		return fmt.Errorf("empty evidence object %s", path)
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	custom, err := evidenceDecoderType(typ)
	if err != nil {
		return err
	}
	if custom {
		if _, ok := value.(string); !ok {
			return fmt.Errorf("expected timestamp string %s", path)
		}
		return nil
	}
	if typ.Kind() != reflect.Struct && typ.Kind() != reflect.Slice && typ.Kind() != reflect.Array && typ.Kind() != reflect.Map {
		return nil
	}

	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("expected evidence object %s", path)
		}
		for _, entry := range evidenceMembers(typ) {
			raw := object[entry.name]
			if EvidenceOptional(typ, entry.field, object) != "" && raw == nil {
				continue
			}
			if err := evidenceFields(raw, entry.field.Type, path+"."+entry.name); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		values, ok := value.([]any)
		if !ok {
			return fmt.Errorf("expected evidence array %s", path)
		}
		for i, raw := range values {
			if err := evidenceFields(raw, typ.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		values, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("expected evidence map %s", path)
		}
		for key, raw := range values {
			if err := evidenceFields(key, typ.Key(), path+"[key]"); err != nil {
				return err
			}
			if err := evidenceFields(raw, typ.Elem(), path+"["+key+"]"); err != nil {
				return err
			}
		}
	}
	return nil
}

// Check decoder methods before descending into a container or its map keys.
// time.Time is the only reviewed scalar; its wire contract has dedicated tests.
func evidenceDecoderType(typ reflect.Type) (bool, error) {
	custom := typ.Implements(reflect.TypeFor[json.Unmarshaler]()) || typ.Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) || reflect.PointerTo(typ).Implements(reflect.TypeFor[json.Unmarshaler]()) || reflect.PointerTo(typ).Implements(reflect.TypeFor[encoding.TextUnmarshaler]())
	if custom && typ != reflect.TypeFor[time.Time]() {
		return true, fmt.Errorf("unreviewed evidence decoder %s", typ)
	}
	return custom, nil
}

type evidenceMember struct {
	field reflect.StructField
	name  string
}

var evidenceTypes sync.Map

func evidenceMembers(typ reflect.Type) []evidenceMember {
	if cached, ok := evidenceTypes.Load(typ); ok {
		return cached.([]evidenceMember)
	}
	var members []evidenceMember
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
				continue
			}
			name = field.Name
		}
		members = append(members, evidenceMember{field, name})
	}
	evidenceTypes.Store(typ, members)
	return members
}

// EvidenceTypeFields validates independent observations of one product type in
// an otherwise incomplete document. It does not qualify the containing graph;
// callers must still validate identities and complete population membership.
func EvidenceTypeFields(data json.RawMessage, typ, target reflect.Type) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var walk func(any, reflect.Type) error
	walk = func(value any, typ reflect.Type) error {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		custom, err := evidenceDecoderType(typ)
		if err != nil {
			return err
		}
		if typ == target {
			if err := evidenceFields(value, typ, typ.String()); err != nil {
				return err
			}
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			return json.Unmarshal(raw, reflect.New(typ).Interface())
		}
		if value == nil || custom {
			return nil
		}
		switch typ.Kind() {
		case reflect.Struct:
			object, ok := value.(map[string]any)
			if !ok {
				return nil
			}
			for _, entry := range evidenceMembers(typ) {
				if err := walk(object[entry.name], entry.field.Type); err != nil {
					return err
				}
			}
		case reflect.Slice, reflect.Array:
			values, ok := value.([]any)
			if !ok {
				return nil
			}
			for _, raw := range values {
				if err := walk(raw, typ.Elem()); err != nil {
					return err
				}
			}
		case reflect.Map:
			values, ok := value.(map[string]any)
			if !ok {
				return nil
			}
			for key, raw := range values {
				if err := evidenceFields(key, typ.Key(), "[key]"); err != nil {
					return err
				}
				if err := walk(raw, typ.Elem()); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(value, typ)
}

// Invalid leaves stay zero in this failure-only destination. No partially
// decoded container is eligible for qualification or relative judgments.
func decodeEvidenceFacts(raw json.RawMessage, dest reflect.Value) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	if dest.Kind() == reflect.Pointer {
		dest.Set(reflect.New(dest.Type().Elem()))
		decodeEvidenceFacts(raw, dest.Elem())
		return
	}
	if dest.CanAddr() && (dest.Addr().Type().Implements(reflect.TypeFor[json.Unmarshaler]()) || dest.Addr().Type().Implements(reflect.TypeFor[encoding.TextUnmarshaler]())) {
		copy := reflect.New(dest.Type())
		if json.Unmarshal(raw, copy.Interface()) == nil {
			dest.Set(copy.Elem())
		}
		return
	}
	switch dest.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return
		}
		for _, member := range evidenceMembers(dest.Type()) {
			field := dest
			for _, index := range member.field.Index {
				if field.Kind() == reflect.Pointer {
					if field.IsNil() {
						field.Set(reflect.New(field.Type().Elem()))
					}
					field = field.Elem()
				}
				field = field.Field(index)
			}
			decodeEvidenceFacts(fields[member.name], field)
		}
	case reflect.Slice:
		if dest.Type().Elem().Kind() == reflect.Uint8 {
			_ = json.Unmarshal(raw, dest.Addr().Interface())
			return
		}
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return
		}
		dest.Set(reflect.MakeSlice(dest.Type(), len(values), len(values)))
		for i, value := range values {
			decodeEvidenceFacts(value, dest.Index(i))
		}
	case reflect.Map:
		var values map[string]json.RawMessage
		if json.Unmarshal(raw, &values) != nil || dest.Type().Key().Kind() != reflect.String {
			return
		}
		dest.Set(reflect.MakeMapWithSize(dest.Type(), len(values)))
		for key, value := range values {
			entry := reflect.New(dest.Type().Elem()).Elem()
			decodeEvidenceFacts(value, entry)
			dest.SetMapIndex(reflect.ValueOf(key).Convert(dest.Type().Key()), entry)
		}
	default:
		_ = json.Unmarshal(raw, dest.Addr().Interface())
	}
}
