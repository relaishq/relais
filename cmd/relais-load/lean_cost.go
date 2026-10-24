package main

import (
	"fmt"
	"math"
	"slices"
	"time"
)

// Each category is explicit, even when inapplicable. A resource belongs to one
// category; each resource/meter is charged once, including shared hosts.
var leanCostCategories = []string{"workers", "relay", "control_signaling", "store_nodes", "store_sync", "public_egress", "inter_az", "ingress", "nat_endpoints", "disk", "ipv4", "observability_api", "secrets", "retained_surge"}

type leanPriceLine struct {
	Category              string   `json:"category"`
	Resource              string   `json:"resource"`
	Meter                 string   `json:"meter"`
	Quantity              *float64 `json:"quantity"`
	Rate                  *float64 `json:"rate"`
	SteadyQuantityPerHour *float64 `json:"steady_quantity_per_hour"`
	ZeroReason            string   `json:"zero_reason,omitempty"`
}
type leanPrices struct {
	Currency string          `json:"currency"`
	Region   string          `json:"region"`
	Date     string          `json:"date"`
	Source   string          `json:"source"`
	Lines    []leanPriceLine `json:"lines"`
}
type leanCost struct {
	Status                 string   `json:"status"`
	Unknown                []string `json:"unknown,omitempty"`
	Currency               string   `json:"currency,omitempty"`
	CallHours              float64  `json:"call_hours"`
	CallHoursWindow        string   `json:"call_hours_window"`
	EgressWindow           string   `json:"egress_window"`
	SteadyPublicGiBPerHour float64  `json:"measured_steady_T1_public_gib_per_hour"`
	Total                  *float64 `json:"allocated_total,omitempty"`
	PerCallHour            *float64 `json:"allocated_per_call_hour,omitempty"`
	SteadyPerCallHour      *float64 `json:"steady_T1_per_call_hour,omitempty"`
	PublicGiB              float64  `json:"measured_public_gib"`
	Note                   string   `json:"note"`
}

func calculateLeanCost(m leanManifest, r leanReport) leanCost {
	c := leanCost{Status: "NOT-RUN", Note: "Integral connected-call allocation, including setup waits and boundary occupancy; separate steady estimate at survivor-safe T=1. Whole hosts, both store nodes and supplied tariffs. Excludes AI fees, taxes and discounts. No monthly bill, price-minimum or cheaper-than-hosted claim. Prices and quantities are supplied observations, not provider lookups."}
	c.CallHoursWindow = "caller ConnectedAt through HungUpAt; includes time outside service telemetry windows"
	c.EgressWindow = "per-phase service telemetry WindowSeconds"
	telemetryCallSeconds := 0.0
	for i, p := range m.Phases {
		for _, process := range p.Run.Processes {
			for _, message := range process.Calls {
				if message.Call != nil && message.Call.ConnectedAt > 0 && message.Call.HungUpAt > message.Call.ConnectedAt {
					c.CallHours += (message.Call.HungUpAt - message.Call.ConnectedAt).Hours()
				}
			}
		}
		if i < len(r.Phases) {
			c.PublicGiB += r.Phases[i].PublicBytesPerCallSecond / (1 << 30) * p.Run.Telemetry.WindowSeconds * float64(p.Run.Callers)
			telemetryCallSeconds += p.Run.Telemetry.WindowSeconds * float64(p.Run.Callers)
		}
	}
	if telemetryCallSeconds > 0 {
		c.SteadyPublicGiBPerHour = c.PublicGiB * 3600 / telemetryCallSeconds
	}
	unknown := func(s string) { c.Unknown = append(c.Unknown, s) }
	if r.Outcome != "pass" {
		unknown("service load not validated")
	}
	p := m.Prices
	if p == nil {
		for _, name := range leanCostCategories {
			unknown(name + ": prices/quantities missing")
		}
		return c
	}
	c.Currency = p.Currency
	if p.Currency == "" || p.Region != m.Region || p.Source == "" {
		unknown("currency, matching region and dated source required")
	}
	if _, err := time.Parse("2006-01-02", p.Date); err != nil {
		unknown("price date must be YYYY-MM-DD")
	}
	categories, meters := map[string]bool{}, map[string]bool{}
	resources := map[string]string{}
	quantities, hourly := map[string]float64{}, map[string]float64{}
	publicLines := 0
	total, steady := 0.0, 0.0
	for _, line := range p.Lines {
		if !slices.Contains(leanCostCategories, line.Category) {
			unknown("unrecognized category " + line.Category)
			continue
		}
		categories[line.Category] = true
		key := line.Resource + "/" + line.Meter
		if line.Resource == "" || line.Meter == "" || meters[key] {
			unknown("missing or duplicate resource/meter " + key)
		}
		meters[key] = true
		if category, exists := resources[line.Resource]; exists && category != line.Category {
			unknown("resource charged under multiple categories: " + line.Resource)
		}
		resources[line.Resource] = line.Category
		if line.Quantity == nil || line.Rate == nil || line.SteadyQuantityPerHour == nil {
			unknown(line.Category + ": missing rate/quantity/steady quantity")
			continue
		}
		q, rate, h := *line.Quantity, *line.Rate, *line.SteadyQuantityPerHour
		if !finiteNonnegative(q) || !finiteNonnegative(rate) || !finiteNonnegative(h) {
			unknown(line.Category + ": nonfinite/negative input")
			continue
		}
		if (q == 0 || rate == 0 || h == 0) && line.ZeroReason == "" {
			unknown(line.Category + ": explicit zero requires reason")
		}
		if line.Category == "public_egress" && math.Abs(q-c.PublicGiB) > max(1e-9, c.PublicGiB*1e-6) {
			unknown("public egress quantity differs from measured payload plus IPv4/UDP headers")
		}
		if line.Category == "public_egress" && ((h == 0 && c.SteadyPublicGiBPerHour > 0) || math.Abs(h-c.SteadyPublicGiBPerHour) > max(1e-9, c.SteadyPublicGiBPerHour*1e-6)) {
			unknown("steady public egress quantity differs from measured telemetry rate at T=1")
		}
		quantities[line.Category] += q
		hourly[line.Category] += h
		if line.Category == "public_egress" {
			publicLines++
		}
		total += q * rate
		steady += h * rate
	}
	for _, name := range leanCostCategories {
		if !categories[name] {
			unknown(name + ": missing ledger category")
		}
	}
	for category, count := range map[string]float64{"workers": 2, "relay": 1, "control_signaling": 1, "store_nodes": 2} {
		if hourly[category] < count {
			unknown(category + ": steady allocation omits required hosts/nodes")
		}
		if quantities[category] < count*r.ObservedWallSeconds/3600 {
			unknown(category + ": actual allocation omits observed node-hours")
		}
	}
	if publicLines != 1 {
		unknown("public egress must have exactly one charge line")
	}
	if c.CallHours <= 0 || !finiteNonnegative(total) || !finiteNonnegative(steady) {
		unknown("invalid call-hour denominator or cost overflow")
	}
	if len(c.Unknown) > 0 {
		return c
	}
	per := total / c.CallHours
	c.Total = &total
	c.PerCallHour = &per
	c.SteadyPerCallHour = &steady
	c.Status = fmt.Sprintf("VALIDATED(%s %.6f/call-hour allocated estimate)", c.Currency, per)
	return c
}
