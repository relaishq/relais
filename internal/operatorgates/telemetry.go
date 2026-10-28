package operatorgates

import (
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/mediaworker"
)

// HostTelemetry is the host procedure's observation envelope. Session ages use
// the product's own type; absent metrics and counters remain absent.
type HostTelemetry struct {
	Metrics []string                         `json:"metrics"`
	ENA     map[string]map[string]*uint64    `json:"ena"`
	Ages    []mediaworker.AgentSessionStatus `json:"ages"`
}

// DecodeHostTelemetry validates product age presence at the host JSON boundary.
// Missing host telemetry is permitted and remains absent in the summary.
func DecodeHostTelemetry(raw json.RawMessage) (HostTelemetry, error) {
	var host HostTelemetry
	err := observe.Decode(raw, &host, observe.Contract{"~ages[].checkpoint_known", "~ages[].checkpoint_age_ms", "~ages[].snapshot_age_ms"})
	return host, err
}

var metricLine = regexp.MustCompile(`^(\S+)\s+([0-9.eE+-]+)$`)
var histogramBound = regexp.MustCompile(`le="([^"]+)"`)
var writeOperation = regexp.MustCompile(`op="(?:set|hset|eval|evalsha|pipeline_exec)"`)

func numericMetrics(host HostTelemetry) map[string]float64 {
	out := map[string]float64{}
	for _, line := range host.Metrics {
		match := metricLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		value, err := strconv.ParseFloat(match[2], 64)
		if err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) {
			out[match[1]] = value
		}
	}
	return out
}
func metricDifferences(a, b MeasurementObservation, accept func(string) bool) (map[string]float64, bool) {
	result := map[string]float64{}
	for _, slot := range []string{"shared", "relay", "w1", "w2"} {
		first, last := a.Hosts[slot], b.Hosts[slot]
		if first.Metrics == nil || last.Metrics == nil {
			return nil, false
		}
		before := numericMetrics(first)
		for key, value := range numericMetrics(last) {
			if !accept(key) {
				continue
			}
			old, exists := before[key]
			if !exists || value < old {
				return nil, false
			}
			result[key] += value - old
		}
	}
	return result, len(result) > 0
}
func histogramTail(a, b MeasurementObservation, name string, writes bool) *float64 {
	rows, ok := metricDifferences(a, b, func(k string) bool {
		return strings.HasPrefix(k, name+"_bucket{") && (!writes || writeOperation.MatchString(k))
	})
	if !ok {
		return nil
	}
	buckets := map[float64]float64{}
	for key, n := range rows {
		match := histogramBound.FindStringSubmatch(key)
		if match == nil {
			return nil
		}
		bound, err := strconv.ParseFloat(match[1], 64)
		if err != nil || math.IsNaN(bound) {
			return nil
		}
		buckets[bound] += n
	}
	total := buckets[math.Inf(1)]
	if total <= 0 {
		return nil
	}
	bounds := []float64{}
	for bound := range buckets {
		bounds = append(bounds, bound)
	}
	slices.Sort(bounds)
	for _, bound := range bounds {
		if buckets[bound] >= total*.99 {
			if math.IsInf(bound, 0) {
				return nil
			}
			return &bound
		}
	}
	return nil
}

// SummarizeObservations preserves missing/reset/unbounded evidence. Known ENA
// violations survive partial telemetry and still fail the measurement gate.
func SummarizeObservations(rows []MeasurementObservation) Measurement {
	e := Measurement{Samples: len(rows)}
	for _, row := range rows {
		for _, slot := range []string{"shared", "relay", "w1", "w2"} {
			if len(row.Hosts[slot].Metrics) == 0 {
				e.MissingSamples++
				break
			}
		}
		for _, slot := range []string{"w1", "w2"} {
			for _, age := range row.Hosts[slot].Ages {
				if !age.CheckpointKnown {
					continue
				}
				for _, value := range []struct {
					number float64
					target **float64
				}{{age.SnapshotAgeMS, &e.SnapshotAgeMaxMS}, {age.CheckpointAgeMS, &e.CheckpointAgeMaxMS}} {
					if value.number >= 0 && !math.IsInf(value.number, 0) && !math.IsNaN(value.number) && (*value.target == nil || value.number > **value.target) {
						number := value.number
						*value.target = &number
					}
				}
			}
		}
	}
	if len(rows) < 2 || !rows[len(rows)-1].At.After(rows[0].At) {
		return e
	}
	a, b := rows[0], rows[len(rows)-1]
	e.RedisWriteP99UpperSeconds = histogramTail(a, b, "relais_redis_op_seconds", true)
	e.CheckpointP99UpperSeconds = histogramTail(a, b, "relais_checkpoint_age_seconds", false)
	for _, v := range []struct {
		prefix string
		target **float64
	}{
		{"relais_store_commands_total", &e.StoreCommandsPerSecond},
		{"relais_transport_bytes_total", &e.StoreBytesPerSecond},
		{"relais_relay_public_udp_bytes_out_total", &e.PublicUDPBytesPerSecond},
	} {
		delta, ok := metricDifferences(a, b, func(k string) bool {
			return strings.HasPrefix(k, v.prefix) && (v.prefix != "relais_transport_bytes_total" || strings.Contains(k, `transport="redis"`))
		})
		if ok {
			var sum float64
			for _, value := range delta {
				sum += value
			}
			sum /= b.At.Sub(a.At).Seconds()
			*v.target = &sum
		}
	}
	var ena uint64
	complete := true
	for _, slot := range []string{"shared", "relay", "w1", "w2"} {
		first, last := a.Hosts[slot].ENA, b.Hosts[slot].ENA
		if len(first) == 0 || len(first) != len(last) {
			complete = false
			continue
		}
		for device, old := range first {
			current, exists := last[device]
			if !exists {
				complete = false
				continue
			}
			for _, key := range []string{"bw_in_allowance_exceeded", "bw_out_allowance_exceeded", "pps_allowance_exceeded", "conntrack_allowance_exceeded", "linklocal_allowance_exceeded"} {
				before, after := old[key], current[key]
				if before == nil || after == nil || *after < *before || *after > maxSafeEpoch || *before > maxSafeEpoch {
					complete = false
					continue
				}
				delta := *after - *before
				if delta > maxSafeEpoch-ena {
					complete = false
					continue
				}
				ena += delta
			}
		}
	}
	if complete {
		e.ENAAllowanceDelta = &ena
	}
	if ena > 0 {
		e.ENAKnownAllowanceDelta = &ena
	}
	return e
}
