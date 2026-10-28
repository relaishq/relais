package observe

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"
)

// MaxInteger is the exact integer range of the operator JSON contract.
const MaxInteger int64 = 1<<53 - 1

// IsNumberDecodeError identifies an unrepresentable product number after strict
// member validation. Callers can mark the enclosing evidence incomplete while
// retaining independently decoded failure facts. Ambiguous JSON is never eligible.
func IsNumberDecodeError(err error) bool {
	var typed *json.UnmarshalTypeError
	if !errors.As(err, &typed) || !strings.HasPrefix(typed.Value, "number ") {
		return false
	}
	switch typed.Type.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// AddCount refuses negative, inexact or overflowing evidence totals.
func AddCount(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > MaxInteger || b > MaxInteger-a {
		return 0, false
	}
	return a + b, true
}
func AddSigned(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// Numbers validates product-owned values before arithmetic. Clock offsets and
// rate intervals are signed coordinates, not counts or elapsed durations. The
// two content identities are opaque uint64 identifiers, never aggregated.
// The absolute integer budget leaves fourfold headroom for window expressions;
// it is a representability check, not a media acceptance threshold.
func Numbers(value any) error {
	var total int64
	var first, last time.Time
	var walk func(reflect.Value, bool, bool) error
	add := func(n int64) error {
		if n < 0 {
			n = -n
		}
		if n > math.MaxInt64/4-total {
			return fmt.Errorf("numeric arithmetic budget exceeded")
		}
		total += n
		return nil
	}
	walk = func(v reflect.Value, signed, opaque bool) error {
		if !v.IsValid() {
			return nil
		}
		if v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
			if v.IsNil() {
				return nil
			}
			return walk(v.Elem(), signed, opaque)
		}
		if v.Type() == reflect.TypeFor[time.Time]() {
			t := v.Interface().(time.Time)
			if t.IsZero() {
				return nil
			}
			if t.Year() < 1 || t.Year() > 9999 {
				return fmt.Errorf("timestamp outside JSON range")
			}
			if first.IsZero() || t.Before(first) {
				first = t
			}
			if last.IsZero() || t.After(last) {
				last = t
			}
			return nil
		}
		switch v.Kind() {
		case reflect.Struct:
			typ := v.Type()
			product := typ.PkgPath() == "github.com/relaishq/relais/pkg/callharness"
			coordinates := product && (typ.Name() == "OffsetInterval" || typ.Name() == "ClockRateInterval")
			if product {
				switch typ.Name() {
				case "OffsetInterval", "DurationInterval":
					if v.FieldByName("Lower").Int() > v.FieldByName("Upper").Int() {
						return fmt.Errorf("reversed numeric interval")
					}
				case "ClockRateInterval":
					if v.FieldByName("LowerPPM").Float() > v.FieldByName("UpperPPM").Float() {
						return fmt.Errorf("reversed rate interval")
					}
				case "TimeInterval":
					if v.FieldByName("Latest").Interface().(time.Time).Before(v.FieldByName("Earliest").Interface().(time.Time)) {
						return fmt.Errorf("reversed time interval")
					}
				}
			}
			for _, pair := range [][2]string{{"Start", "End"}, {"StartedAt", "FinishedAt"}, {"At", "FinishedAt"}, {"CallerSend", "CallerReceive"}, {"ServerReceive", "ServerSend"}} {
				a, b := v.FieldByName(pair[0]), v.FieldByName(pair[1])
				if !a.IsValid() || !b.IsValid() || a.Type() != b.Type() {
					continue
				}
				if a.Type() == reflect.TypeFor[time.Time]() {
					x, y := a.Interface().(time.Time), b.Interface().(time.Time)
					if !x.IsZero() && !y.IsZero() && y.Before(x) {
						return fmt.Errorf("reversed timestamp range")
					}
				}
				if a.Type() == reflect.TypeFor[time.Duration]() && b.Int() < a.Int() {
					return fmt.Errorf("reversed duration range")
				}
			}
			for i := 0; i < v.NumField(); i++ {
				field := typ.Field(i)
				if field.PkgPath != "" {
					continue
				}
				identity := product && typ.Name() == "EventMeasurement" && (field.Name == "FirstContentIdentity" || field.Name == "FirstNewContentIdentity")
				if err := walk(v.Field(i), coordinates, identity); err != nil {
					return fmt.Errorf("%s: %w", field.Name, err)
				}
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				if err := walk(v.Index(i), signed, opaque); err != nil {
					return err
				}
			}
		case reflect.Map:
			it := v.MapRange()
			for it.Next() {
				if err := walk(it.Value(), signed, opaque); err != nil {
					return err
				}
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			n := v.Int()
			if n > MaxInteger || n < -MaxInteger || !signed && n < 0 {
				return fmt.Errorf("negative or out-of-range number")
			}
			return add(n)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			if opaque {
				return nil
			}
			n := v.Uint()
			if n > uint64(MaxInteger) {
				return fmt.Errorf("out-of-range counter")
			}
			return add(int64(n))
		case reflect.Float32, reflect.Float64:
			n := v.Float()
			if math.IsNaN(n) || math.IsInf(n, 0) || math.Abs(n) > float64(MaxInteger) || !signed && n < 0 {
				return fmt.Errorf("negative or out-of-range measurement")
			}
		}
		return nil
	}
	if err := walk(reflect.ValueOf(value), false, false); err != nil {
		return err
	}
	if !first.IsZero() && last.Sub(first) > time.Duration(MaxInteger) {
		return fmt.Errorf("timestamp span outside arithmetic range")
	}
	return nil
}
