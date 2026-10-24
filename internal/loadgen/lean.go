package loadgen

// PoolDistributedReports joins serial phases only for identity/interval trial
// judgment. Callers here counts report records, never concurrent occupancy.
// Resource rates and clocks remain in their original per-phase reports.
// Raw distributed evidence is preserved, including failed/uncertain receipts.
func PoolDistributedReports(phases []RunReport) RunReport {
	pooled := RunReport{}
	for _, r := range phases {
		pooled.Callers += r.Callers
		pooled.Processes = append(pooled.Processes, r.Processes...)
		pooled.Events = append(pooled.Events, r.Events...)
	}
	pooled.Judge()
	return pooled
}
