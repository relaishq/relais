package observe

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// EvidenceIssue contains only a contract location and a fixed reason. Never
// publish decoder error text: it can contain values, map keys or identities.
type EvidenceIssue struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

var evidencePathPart = regexp.MustCompile(`\.[A-Za-z_][A-Za-z_0-9]*|\[[0-9]+\]|\[\*\]`)
var evidenceArrayIndex = regexp.MustCompile(`^\[[0-9]+\]$`)
var evidenceMapIndex = regexp.MustCompile(`\[[^\]]*\]`)

func DescribeEvidence(err error) EvidenceIssue {
	if err == nil {
		return EvidenceIssue{}
	}
	text := err.Error()
	for _, row := range []struct{ prefix, reason string }{
		{"required evidence ", "missing-or-null"}, {"empty evidence object ", "empty-object"},
		{"expected timestamp string ", "wrong-type"}, {"expected evidence object ", "wrong-type"},
		{"expected evidence array ", "wrong-type"}, {"expected evidence map ", "wrong-type"},
	} {
		if rest, ok := strings.CutPrefix(text, row.prefix); ok {
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				return EvidenceIssue{Path: "$", Reason: "invalid-evidence"}
			}
			path := fields[0]
			path = evidenceMapIndex.ReplaceAllStringFunc(path, func(s string) string {
				if evidenceArrayIndex.MatchString(s) {
					return s
				}
				return "[*]"
			})
			return EvidenceIssue{Path: path, Reason: row.reason}
		}
	}
	var typed *json.UnmarshalTypeError
	if errors.As(err, &typed) {
		return EvidenceIssue{Path: "$." + typed.Field, Reason: "wrong-type"}
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return EvidenceIssue{Path: "$", Reason: "invalid-json"}
	}
	if strings.Contains(text, "noncanonical field") {
		return EvidenceIssue{Path: "$", Reason: "noncanonical-field"}
	}
	if strings.Contains(text, "duplicate or invalid JSON key") {
		return EvidenceIssue{Path: "$", Reason: "duplicate-json-key"}
	}
	parts := strings.Split(text, ": ")
	if reason, ok := evidenceReasons[parts[len(parts)-1]]; ok {
		path := "$"
		if len(parts) > 1 {
			path += "." + strings.Join(parts[:len(parts)-1], ".")
		}
		return EvidenceIssue{Path: path, Reason: reason}
	}
	return EvidenceIssue{Path: "$", Reason: "invalid-evidence"}
}

// EvidenceNames derives allowed diagnostic field names from the same product
// types as decoding. Unknown extension names and opaque map keys are not public.
func EvidenceNames(types ...reflect.Type) map[string]bool {
	names, seen := map[string]bool{}, map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		if seen[t] {
			return
		}
		seen[t] = true
		switch t.Kind() {
		case reflect.Pointer, reflect.Array, reflect.Slice, reflect.Map:
			walk(t.Elem())
		case reflect.Struct:
			for _, m := range evidenceMembers(t) {
				names[m.name] = true
				names[m.field.Name] = true
				walk(m.field.Type)
			}
		}
	}
	for _, t := range types {
		walk(t)
	}
	return names
}
func (p EvidenceIssue) Public(types ...reflect.Type) EvidenceIssue {
	if p.Path == "" && p.Reason == "" {
		return p
	}
	if !publicEvidenceReasons[p.Reason] {
		p.Reason = "invalid-evidence"
	}

	names := EvidenceNames(types...)
	rest := strings.TrimPrefix(p.Path, "$")
	if rest == p.Path {
		p.Path = "$"
		return p
	}
	for _, part := range evidencePathPart.FindAllString(rest, -1) {
		if !strings.HasPrefix(rest, part) || part[0] == '.' && !names[part[1:]] {
			p.Path = "$"
			return p
		}
		if part[0] == '[' && part != "[*]" {
			index, err := strconv.ParseUint(part[1:len(part)-1], 10, 64)
			if err != nil || index > 64<<20 {
				p.Path = "$"
				return p
			}
		}
		rest = strings.TrimPrefix(rest, part)
	}
	if rest != "" {
		p.Path = "$"
	}
	return p
}

// Fixed contract reasons, never raw values from a decoder or server.
var evidenceReasons = map[string]string{
	"ambiguous caller sessions":                        "ambiguous-caller-sessions",
	"ambiguous track inventory":                        "ambiguous-track-inventory",
	"armed event without move":                         "armed-event-without-move",
	"arrival count exceeds packets":                    "arrival-count-exceeds-packets",
	"caller and server receipts differ":                "caller-and-server-receipts-differ",
	"caller receipt identity mismatch":                 "caller-receipt-identity-mismatch",
	"caller registration mismatch":                     "caller-registration-mismatch",
	"caller total differs from inventory":              "caller-total-differs-from-inventory",
	"dial and call inventories differ":                 "dial-and-call-inventories-differ",
	"duplicate receipt":                                "duplicate-receipt",
	"event and move inventories differ":                "event-and-move-inventories-differ",
	"event identity mismatch":                          "event-identity-mismatch",
	"first baseline population changed":                "first-baseline-population-changed",
	"gap differs from track inventory":                 "gap-differs-from-track-inventory",
	"incomplete armed inventory":                       "incomplete-armed-inventory",
	"incomplete dial inventory":                        "incomplete-dial-inventory",
	"incomplete event collections":                     "incomplete-event-collections",
	"incomplete event mapping":                         "incomplete-event-mapping",
	"incomplete event membership":                      "incomplete-event-membership",
	"incomplete process calls":                         "incomplete-process-calls",
	"incomplete registration collections":              "incomplete-registration-collections",
	"incomplete track inventory":                       "incomplete-track-inventory",
	"inconsistent content baseline windows":            "inconsistent-content-baseline-windows",
	"inconsistent first baselines":                     "inconsistent-first-baselines",
	"invalid armed inventory":                          "invalid-armed-inventory",
	"invalid caller message identity":                  "invalid-caller-message-identity",
	"invalid content denominator":                      "invalid-content-denominator",
	"invalid event kind":                               "invalid-event-kind",
	"invalid event sessions":                           "invalid-event-sessions",
	"invalid expected sessions":                        "invalid-expected-sessions",
	"invalid probe numbers":                            "invalid-probe-numbers",
	"invalid probe population":                         "invalid-probe-population",
	"invalid receipt scope":                            "invalid-receipt-scope",
	"missing caller collections":                       "missing-caller-collections",
	"missing classification population":                "missing-classification-population",
	"missing event duration bound":                     "missing-event-duration-bound",
	"missing or invalid generator stats fields":        "missing-or-invalid-generator-stats-fields",
	"missing report collections":                       "missing-report-collections",
	"missing trials":                                   "missing-trials",
	"missing video evidence":                           "missing-video-evidence",
	"mixed report runs":                                "mixed-report-runs",
	"move differs from armed inventory":                "move-differs-from-armed-inventory",
	"move session mismatch":                            "move-session-mismatch",
	"negative or out-of-range measurement":             "negative-or-out-of-range-measurement",
	"negative or out-of-range number":                  "negative-or-out-of-range-number",
	"numeric arithmetic budget exceeded":               "numeric-arithmetic-budget-exceeded",
	"out-of-range counter":                             "out-of-range-counter",
	"pacing total overflow":                            "pacing-total-overflow",
	"receipt and move inventories differ":              "receipt-and-move-inventories-differ",
	"receipt without move":                             "receipt-without-move",
	"relay phase contains trials or incomplete clocks": "relay-phase-contains-trials-or-incomplete-clocks",
	"reversed duration range":                          "reversed-duration-range",
	"reversed numeric interval":                        "reversed-numeric-interval",
	"reversed rate interval":                           "reversed-rate-interval",
	"reversed time interval":                           "reversed-time-interval",
	"reversed timestamp range":                         "reversed-timestamp-range",
	"timestamp outside JSON range":                     "timestamp-outside-json-range",
	"timestamp span outside arithmetic range":          "timestamp-span-outside-arithmetic-range",
}
var publicEvidenceReasons = func() map[string]bool {
	reasons := map[string]bool{}
	for _, reason := range []string{"missing-or-null", "empty-object", "wrong-type", "invalid-json", "noncanonical-field", "duplicate-json-key", "invalid-evidence", "numeric-evidence-invalid", "collection-incomplete", "process-incomplete", "invalid-sample", "qualification-incomplete"} {
		reasons[reason] = true
	}
	for _, reason := range evidenceReasons {
		reasons[reason] = true
	}
	return reasons
}()
