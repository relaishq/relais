package componentcodec

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"time"
)

// The v10 binary grammar is big endian, fixed-width numbers (int is int64),
// uint32 byte/count lengths, one-byte booleans/presence, declaration-order
// exported struct fields, and ascending uint32 map keys. Nil and empty slices
// and maps have one encoding. Times are zero-marker/Unix seconds/nanoseconds in
// UTC; monotonic clock readings and local time zones are not durable state.
// Only the frozen v10 wire structs below use this grammar. Changing their field
// order/types requires a new component version and new golden fixtures.
// Above the documented 64 KiB configurable working-state ceiling plus wire
// framing, 8 KiB history, 4 KiB setup and 7 KiB hot proposals. Move-only audio
// and task payloads are separate export data, never these components.

func Marshal(kind byte, value any) ([]byte, error) {
	var b bytes.Buffer
	b.Write([]byte{10, kind})
	if err := writeComponentValue(&b, reflect.ValueOf(value)); err != nil {
		return nil, err
	}
	if b.Len() > DecodeLimit {
		return nil, fmt.Errorf("%w: component exceeds wire limit", ErrInvalid)
	}
	return b.Bytes(), nil
}

func Unmarshal(data []byte, kind byte, value any) error {
	if len(data) < 2 || len(data) > DecodeLimit || data[0] != 10 || data[1] != kind {
		return fmt.Errorf("%w: component version/kind/length", ErrInvalid)
	}
	r := bytes.NewReader(data[2:])
	if err := readComponentValue(r, reflect.ValueOf(value).Elem()); err != nil {
		return fmt.Errorf("%w: component decode: %v", ErrInvalid, err)
	}
	encoded, err := Marshal(kind, reflect.ValueOf(value).Elem().Interface())
	if err != nil {
		return err
	}
	if r.Len() != 0 || !bytes.Equal(data, encoded) {
		return fmt.Errorf("%w: noncanonical component", ErrInvalid)
	}
	return nil
}

func writeComponentValue(b *bytes.Buffer, v reflect.Value) error {
	if v.Type() == reflect.TypeFor[time.Time]() {
		t := v.Interface().(time.Time)
		if t.IsZero() {
			b.WriteByte(0)
			return nil
		}
		b.WriteByte(1)
		_ = binary.Write(b, binary.BigEndian, t.Unix())
		return binary.Write(b, binary.BigEndian, uint32(t.Nanosecond()))
	}
	switch v.Kind() {
	case reflect.Bool:
		if v.Bool() {
			b.WriteByte(1)
		} else {
			b.WriteByte(0)
		}
	case reflect.Float64:
		if math.IsNaN(v.Float()) || math.IsInf(v.Float(), 0) {
			return fmt.Errorf("%w: nonfinite component number", ErrInvalid)
		}
		return binary.Write(b, binary.BigEndian, v.Float())
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return binary.Write(b, binary.BigEndian, v.Interface())
	case reflect.Int, reflect.Int64:
		return binary.Write(b, binary.BigEndian, v.Int())
	case reflect.String:
		if v.Len() > DecodeLimit {
			return fmt.Errorf("%w: component string", ErrInvalid)
		}
		_ = binary.Write(b, binary.BigEndian, uint32(v.Len()))
		b.WriteString(v.String())
	case reflect.Pointer:
		if v.IsNil() {
			b.WriteByte(0)
			return nil
		}
		b.WriteByte(1)
		return writeComponentValue(b, v.Elem())
	case reflect.Slice:
		if v.Len() > DecodeLimit {
			return fmt.Errorf("%w: component slice", ErrInvalid)
		}
		_ = binary.Write(b, binary.BigEndian, uint32(v.Len()))
		if v.Type().Elem().Kind() == reflect.Uint8 {
			_, err := b.Write(v.Bytes())
			return err
		}
		for i := 0; i < v.Len(); i++ {
			if err := writeComponentValue(b, v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := writeComponentValue(b, v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if !v.Type().Field(i).IsExported() {
				return fmt.Errorf("%w: private wire field", ErrInvalid)
			}
			if err := writeComponentValue(b, v.Field(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.Uint32 {
			return fmt.Errorf("%w: component map key", ErrInvalid)
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].Uint() < keys[j].Uint() })
		_ = binary.Write(b, binary.BigEndian, uint32(len(keys)))
		for _, k := range keys {
			if err := writeComponentValue(b, k); err != nil {
				return err
			}
			if err := writeComponentValue(b, v.MapIndex(k)); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: component type %s", ErrInvalid, v.Type())
	}
	return nil
}

func readComponentValue(r *bytes.Reader, v reflect.Value) error {
	if v.Type() == reflect.TypeFor[time.Time]() {
		marker, err := r.ReadByte()
		if err != nil {
			return err
		}
		if marker == 0 {
			v.Set(reflect.ValueOf(time.Time{}))
			return nil
		}
		if marker != 1 {
			return fmt.Errorf("invalid time marker")
		}
		var sec int64
		var nano uint32
		if err := binary.Read(r, binary.BigEndian, &sec); err != nil {
			return err
		}
		if err := binary.Read(r, binary.BigEndian, &nano); err != nil {
			return err
		}
		if nano >= 1e9 {
			return fmt.Errorf("invalid nanoseconds")
		}
		v.Set(reflect.ValueOf(time.Unix(sec, int64(nano)).UTC()))
		return nil
	}
	switch v.Kind() {
	case reflect.Bool:
		n, err := r.ReadByte()
		if err != nil {
			return err
		}
		if n > 1 {
			return fmt.Errorf("invalid boolean")
		}
		v.SetBool(n == 1)
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float64:
		return binary.Read(r, binary.BigEndian, v.Addr().Interface())
	case reflect.Int, reflect.Int64:
		var n int64
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return err
		}
		if v.OverflowInt(n) {
			return fmt.Errorf("integer overflow")
		}
		v.SetInt(n)
	case reflect.String, reflect.Slice, reflect.Map:
		var n uint32
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return err
		}
		// Check the minimum encoded bytes per element before allocating any
		// output collection. Counts bounded only by remaining bytes amplify a
		// truncated [][]byte or map into much larger allocations.
		minimum := uint64(1)
		var err error
		switch v.Kind() {
		case reflect.Slice:
			minimum, err = minimumComponentBytes(v.Type().Elem())
		case reflect.Map:
			if v.Type().Key().Kind() != reflect.Uint32 {
				return fmt.Errorf("invalid map key")
			}
			minimum, err = minimumComponentBytes(v.Type().Elem())
			minimum += 4 // uint32 key
		}
		if err != nil {
			return err
		}
		if minimum == 0 || n > DecodeLimit || uint64(n) > uint64(r.Len())/minimum {
			return io.ErrUnexpectedEOF
		}
		switch v.Kind() {
		case reflect.String:
			b := make([]byte, n)
			if _, err := io.ReadFull(r, b); err != nil {
				return err
			}
			v.SetString(string(b))
		case reflect.Slice:
			if v.Type().Elem().Kind() == reflect.Uint8 {
				b := make([]byte, int(n))
				if _, err := io.ReadFull(r, b); err != nil {
					return err
				}
				v.SetBytes(b)
				return nil
			}
			v.Set(reflect.MakeSlice(v.Type(), int(n), int(n)))
			for i := 0; i < int(n); i++ {
				if err := readComponentValue(r, v.Index(i)); err != nil {
					return err
				}
			}
		case reflect.Map:
			if v.Type().Key().Kind() != reflect.Uint32 {
				return fmt.Errorf("invalid map key")
			}
			v.Set(reflect.MakeMapWithSize(v.Type(), int(n)))
			var previous uint64
			for i := uint32(0); i < n; i++ {
				k := reflect.New(v.Type().Key()).Elem()
				x := reflect.New(v.Type().Elem()).Elem()
				if err := readComponentValue(r, k); err != nil {
					return err
				}
				if i > 0 && k.Uint() <= previous {
					return fmt.Errorf("unordered/duplicate map key")
				}
				previous = k.Uint()
				if err := readComponentValue(r, x); err != nil {
					return err
				}
				v.SetMapIndex(k, x)
			}
		}
	case reflect.Pointer:
		n, err := r.ReadByte()
		if err != nil {
			return err
		}
		if n == 0 {
			v.SetZero()
			return nil
		}
		if n != 1 {
			return fmt.Errorf("invalid presence")
		}
		v.Set(reflect.New(v.Type().Elem()))
		return readComponentValue(r, v.Elem())
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := readComponentValue(r, v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if !v.Type().Field(i).IsExported() {
				return fmt.Errorf("private wire field")
			}
			if err := readComponentValue(r, v.Field(i)); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported wire type %s", v.Type())
	}
	return nil
}

// Minimum bytes of a value, not its in-memory Go size. Lengths/presence allow
// empty collections and nil pointers; time's zero marker occupies one byte.
func minimumComponentBytes(t reflect.Type) (uint64, error) {
	if t == reflect.TypeFor[time.Time]() {
		return 1, nil
	}
	switch t.Kind() {
	case reflect.Bool, reflect.Uint8, reflect.Pointer:
		return 1, nil
	case reflect.Uint16:
		return 2, nil
	case reflect.Uint32, reflect.String, reflect.Slice, reflect.Map:
		return 4, nil
	case reflect.Uint64, reflect.Int, reflect.Int64, reflect.Float64:
		return 8, nil
	case reflect.Array:
		size, err := minimumComponentBytes(t.Elem())
		if err != nil {
			return 0, err
		}
		return uint64(t.Len()) * size, nil
	case reflect.Struct:
		var size uint64
		for i := 0; i < t.NumField(); i++ {
			if !t.Field(i).IsExported() {
				return 0, fmt.Errorf("private wire field")
			}
			field, err := minimumComponentBytes(t.Field(i).Type)
			if err != nil {
				return 0, err
			}
			size += field
		}
		return size, nil
	default:
		return 0, fmt.Errorf("unsupported wire type %s", t)
	}
}
