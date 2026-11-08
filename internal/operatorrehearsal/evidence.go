package operatorrehearsal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
)

// SanitizeMediaEvidence retains JSON structure (including absent/null/duplicate
// members and exact number tokens), not a zero-filled product re-encoding.
// Opaque text/identities become stable aliases; all timestamps share one shift.
func SanitizeMediaEvidence(pool operatorevidence.Pool) ([]byte, error) {
	coverage := pool.ClockCoverage
	pool.ClockCoverage = nil
	raw, err := json.Marshal(pool)
	if err != nil {
		return nil, err
	}
	s := evidenceSanitizer{fields: map[reflect.Type]map[string]reflect.Type{}}
	if pool.ClockReceiptSHA256 != "" || coverage != nil && observe.IsBound(coverage.ClockReceiptSHA256, coverage.Binding) {
		if coverage == nil || coverage.ValidateBinding(pool.ClockReceiptSHA256) != nil || coverage.Binding.Receipt.RunID != pool.RunID {
			return nil, fmt.Errorf("clock-coverage-binding")
		}
		s.publicRun, s.receiptSHA = pool.RunID, pool.ClockReceiptSHA256
	}

	clean, err := s.document(raw, reflect.TypeFor[operatorevidence.Pool]())
	if err != nil || coverage == nil {
		return clean, err
	}
	public := sanitizeCoverageTimes(*coverage, s.anchor)
	extra, err := json.Marshal(public)
	if err != nil {
		return nil, err
	}
	// Append only the diagnostic member. Re-encoding an envelope map would
	// reorder existing fields and change their published bytes.
	result := append([]byte{}, clean[:len(clean)-1]...)
	if len(clean) > 2 {
		result = append(result, ',')
	}
	result = append(result, `"clockCoverage":`...)
	result = append(result, extra...)
	result = append(result, '}')
	return result, nil
}

type evidenceSanitizer struct {
	publicRun, receiptSHA string
	fields                map[reflect.Type]map[string]reflect.Type
	anchor                time.Time
}

func evidenceAlias(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}
func evidenceType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}
func (s *evidenceSanitizer) member(t reflect.Type, key string) (string, reflect.Type) {
	t = evidenceType(t)
	if t != nil && t.Kind() == reflect.Map {
		name := key
		switch key {
		case "resume", "first_content", "first_new_content", "first_live", "audio_freshness", "video_freshness", "resume_observed", "kill_to_live":
		default:
			name = evidenceAlias(key)
		}
		return name, t.Elem()
	}
	if t != nil && t.Kind() == reflect.Struct {
		fields, ok := s.fields[t]
		if !ok {
			fields = map[string]reflect.Type{}
			for _, field := range reflect.VisibleFields(t) {
				name := strings.Split(field.Tag.Get("json"), ",")[0]
				if field.PkgPath != "" || name == "-" {
					continue
				}
				if name == "" {
					if field.Anonymous && evidenceType(field.Type).Kind() == reflect.Struct {
						continue
					}
					name = field.Name
				}
				fields[name] = field.Type
			}
			s.fields[t] = fields
		}
		for name, typ := range fields {
			if strings.EqualFold(key, name) {
				return key, typ
			}
		}
	}
	return evidenceAlias(key), nil
}

// Only reason fields consumed by the diagnostic classifier get canonical text.
// Identical-looking identities and unknown extension fields stay opaque.
func evidenceTimingReason(t reflect.Type, key string) bool {
	switch evidenceType(t) {
	case reflect.TypeFor[callharness.MetricVerdict]():
		return strings.EqualFold(key, "Reasons")
	case reflect.TypeFor[callharness.DistributedMeasurement]():
		return strings.EqualFold(key, "causes")
	case reflect.TypeFor[callharness.DistributedRecording]():
		return strings.EqualFold(key, "causes") || strings.EqualFold(key, "clock_error")
	}
	return false
}
func (s *evidenceSanitizer) text(v string, timingReason bool) string {
	// These commitments are already public in the immutable receipt.
	if v != "" && (v == s.publicRun || v == s.receiptSHA) {
		return v
	}
	if timingReason {
		if canonical, ok := observe.CanonicalTimingReason(v); ok {
			return canonical
		}
	}
	if v == "" {
		return ""
	}

	switch v {
	case "rollout", "control-restart", "upgrade-rollback", "relay-deploy", "audio", "video", "move", "takeover", "drain", "kill", "dial", "call", "done", "stats", "ready", "connected", "failed", "passed", "inconclusive", "pass", "fail", "valid", "invalid", "unknown", "untrusted", "not-run", "pending":
		return v
	}
	// A hex alias is also valid base64 for diagnostic byte slices. Equality of
	// identities/diagnostics is preserved without retaining arbitrary strings.
	return evidenceAlias(v)
}
func (s *evidenceSanitizer) document(raw []byte, typ reflect.Type) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var out bytes.Buffer
	if err := s.value(d, &out, "", typ, false); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("evidence-json-trailing")
	}
	return out.Bytes(), nil
}
func (s *evidenceSanitizer) value(d *json.Decoder, out *bytes.Buffer, key string, typ reflect.Type, timingReason bool) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); ok {
		out.WriteByte(byte(delim))
		first := true
		for d.More() {
			if !first {
				out.WriteByte(',')
			}
			first = false
			childKey := key
			childTimingReason := timingReason && delim == '['
			var childType reflect.Type
			if t := evidenceType(typ); t != nil && delim == '[' && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
				childType = t.Elem()
			}
			if delim == '{' {
				k, e := d.Token()
				if e != nil {
					return e
				}
				childKey = k.(string)
				childTimingReason = evidenceTimingReason(typ, childKey)
				name, memberType := s.member(typ, childKey)
				childType = memberType

				encoded, _ := json.Marshal(name)
				out.Write(encoded)
				out.WriteByte(':')
			}
			if err := s.value(d, out, childKey, childType, childTimingReason); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil {
			return err
		}
		out.WriteByte(byte(end.(json.Delim)))
		return nil
	}
	if v, ok := token.(string); ok {
		if key == "callerJsonl" && typ != nil {
			// Parse opaque product messages individually, preserving duplicate keys
			// and missing members. Invalid syntax remains an explicit invalid sentinel.
			decoder := json.NewDecoder(strings.NewReader(v))
			var lines []string
			for {
				var raw json.RawMessage
				err := decoder.Decode(&raw)
				if err == io.EOF {
					break
				}
				if err != nil {
					lines = append(lines, "{\"invalid-json\":")
					break
				}
				clean, err := s.document(raw, reflect.TypeFor[loadgen.Message]())
				if err != nil {
					return err
				}
				lines = append(lines, string(clean))
			}
			token = strings.Join(lines, "\n")
			if len(lines) > 0 {
				token = token.(string) + "\n"
			}
		} else if evidenceType(typ) == reflect.TypeFor[time.Time]() {
			t, e := time.Parse(time.RFC3339Nano, v)
			if e != nil {
				token = evidenceAlias(v)
			} else if !t.IsZero() {
				if s.anchor.IsZero() {
					s.anchor = t
				}
				delta := t.Sub(s.anchor)
				if !s.anchor.Add(delta).Equal(t) {
					token = "invalid-timestamp"
				} else {
					token = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Add(delta).Format(time.RFC3339Nano)
				}
			}
		} else if typ == nil && v != "" {
			token = evidenceAlias(v)
		} else {
			token = s.text(v, timingReason)
		}
	}
	if _, ok := token.(json.Number); ok && typ == nil {
		token = json.Number("0")
	}
	encoded, err := json.Marshal(token)
	if err != nil {
		return err
	}
	out.Write(encoded)
	return nil
}

// headCallerBuild names the head-built caller in head-caller-evidence.json with
// the field names of report.json's headCaller section.
type headCallerBuild struct {
	Commit       string `json:"commit"`
	CallerSHA256 string `json:"callerSha256"`
}

// saveMediaEvidence writes the judge input. head is nil for the pinned caller.
func saveMediaEvidence(output, file string, pool operatorevidence.Pool, judgeSHA string, head *headCallerBuild) error {
	if !fullCommit.MatchString(pool.SourceCommit) || !fullDigest.MatchString(judgeSHA) || head != nil && (!fullCommit.MatchString(head.Commit) || !fullDigest.MatchString(head.CallerSHA256)) {
		return fmt.Errorf("invalid-evidence-provenance")
	}
	raw, err := SanitizeMediaEvidence(pool)
	if err != nil {
		return err
	}
	artifact := struct {
		SchemaVersion int              `json:"schemaVersion"`
		SourceCommit  string           `json:"sourceCommit"`
		JudgeSHA256   string           `json:"judgeSha256"`
		HeadCaller    *headCallerBuild `json:"headCaller,omitempty"`
		Pool          json.RawMessage  `json:"pool"`
	}{1, pool.SourceCommit, judgeSHA, head, raw}
	data, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	name := filepath.Join(output, file)
	if err = os.WriteFile(name+".tmp", append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(name+".tmp", name)
}
