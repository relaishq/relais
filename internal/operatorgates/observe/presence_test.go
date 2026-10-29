package observe

import "testing"

func TestRequireTerminalArray(t *testing.T) {
	// The calibration caller report has populated Tracks, unlike the old
	// empty-track fixture. A terminal [] consumes the last path segment.
	data := []byte(`{"call":{"Tracks":[{"Kind":"audio","DuplicatePackets":0},{"Kind":"video","DuplicatePackets":0}]}}`)
	if err := Require(data, Contract{"call.Tracks[]", "call.Tracks[].DuplicatePackets"}); err != nil {
		t.Fatal(err)
	}
}

func TestPresencePaths(t *testing.T) {
	for _, row := range []struct {
		name, data, field string
		valid             bool
	}{
		{"empty-path", `{}`, "", false},
		{"root", `{}`, ".", false},
		{"empty-segment", `{"a":{}}`, "a..b", false},
		{"empty-array-path", `{"a":[]}`, "a[].", false},
		{"array", `{"a":[{},{}]}`, "a[]", true},
		{"empty-array", `{"a":[]}`, "a[].b", true},
		{"nested-empty-array", `{"a":[{"b":[]}]}`, "a[].b[].c", true},
		{"nested-empty-map", `{"a":[{}]}`, "a[].b", false},
		{"missing-array", `{"a":{}}`, "a[]", false},
		{"scalar-row", `{"a":[0]}`, "a[].b", false},
		{"malformed", `{"a":[`, "a[]", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			if err := Require([]byte(row.data), Contract{row.field}); (err == nil) != row.valid {
				t.Fatal("unexpected presence result", err)
			}
		})
	}
	for _, raw := range []string{`{}`, `[]`, `{"a":[{},[]]}`, `null`, `0`, `broken`} {
		if err := present([]byte(raw), nil); (err == nil) != (raw != "broken") {
			t.Fatal("empty recursive path did not validate its JSON value", raw, err)
		}
	}
	if err := Require([]byte(`broken`), nil); err == nil {
		t.Fatal("empty contract accepted malformed JSON")
	}
}

func FuzzRequire(f *testing.F) {
	for _, seed := range []struct{ data, field string }{{`{"a":[{}]}`, "a[]"}, {`{}`, ""}, {`[]`, "."}, {`{"a":[{"b":[]}]}`, "a[].b[]"}} {
		f.Add([]byte(seed.data), seed.field)
	}
	f.Fuzz(func(_ *testing.T, data []byte, field string) {
		_ = Require(data, Contract{field})
		_ = present(data, nil)
	})
}
