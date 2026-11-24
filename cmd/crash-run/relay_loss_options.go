package main

import "github.com/relaishq/relais/internal/relayloss"

// Only the operator report mode uses extra operational wait margin. Product
// budgets remain in relayloss.Budget and relayLossMediaProgress.
type relayLossOptions struct {
	ReportPath string
	Evidence   *relayloss.Report
}

func (o relayLossOptions) margin() int {
	if o.ReportPath != "" {
		return 2
	}
	return 1
}
