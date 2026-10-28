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
)

// SanitizeMediaEvidence retains JSON structure (including absent/null/duplicate
// members and exact number tokens), not a zero-filled product re-encoding.
// Opaque text/identities become stable aliases; all timestamps share one shift.
func SanitizeMediaEvidence(pool operatorevidence.Pool) ([]byte, error) {
	raw, err := json.Marshal(pool)
	if err != nil {
		return nil, err
	}
	s := evidenceSanitizer{fields: map[reflect.Type]map[string]reflect.Type{}}
	return s.document(raw, reflect.TypeFor[operatorevidence.Pool]())
}

type evidenceSanitizer struct {
	fields map[reflect.Type]map[string]reflect.Type
	anchor time.Time
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
func (s *evidenceSanitizer) text(v string) string {
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
	if err := s.value(d, &out, "", typ); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("evidence-json-trailing")
	}
	return out.Bytes(), nil
}
func (s *evidenceSanitizer) value(d *json.Decoder, out *bytes.Buffer, key string, typ reflect.Type) error {
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
				name, memberType := s.member(typ, childKey)
				childType = memberType

				encoded, _ := json.Marshal(name)
				out.Write(encoded)
				out.WriteByte(':')
			}
			if err := s.value(d, out, childKey, childType); err != nil {
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
			token = s.text(v)
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
func saveMediaEvidence(output string, pool operatorevidence.Pool, judgeSHA string) error {
	if !fullCommit.MatchString(pool.SourceCommit) || !fullDigest.MatchString(judgeSHA) {
		return fmt.Errorf("invalid-evidence-provenance")
	}
	raw, err := SanitizeMediaEvidence(pool)
	if err != nil {
		return err
	}
	artifact := struct {
		SchemaVersion int             `json:"schemaVersion"`
		SourceCommit  string          `json:"sourceCommit"`
		JudgeSHA256   string          `json:"judgeSha256"`
		Pool          json.RawMessage `json:"pool"`
	}{1, pool.SourceCommit, judgeSHA, raw}
	data, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	name := filepath.Join(output, "media-evidence.json")
	if err = os.WriteFile(name+".tmp", append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(name+".tmp", name)
}
