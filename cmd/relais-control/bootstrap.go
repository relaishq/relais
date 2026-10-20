package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type fixedWorker struct{ name, endpoint string }

// Validate the entire fixed list before listening or reaching any dependency.
// A valid but unreachable endpoint is a runtime condition, not a config error.
func parseWorkers(entries []string) ([]fixedWorker, error) {
	result := make([]fixedWorker, 0, len(entries))
	names := make(map[string]bool)
	endpoints := make(map[string]bool)
	for _, entry := range entries {
		name, endpoint, ok := strings.Cut(entry, "=")
		u, err := url.Parse(endpoint)
		if !ok || name == "" || strings.ContainsAny(name, "/?# \t\r\n") || err != nil || u.Scheme != "http" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("worker must be name=http://address")
		}
		endpoint = strings.TrimSuffix(endpoint, "/")
		if names[name] || endpoints[endpoint] {
			return nil, errors.New("duplicate worker name or endpoint")
		}
		names[name], endpoints[endpoint] = true, true
		result = append(result, fixedWorker{name, endpoint})
	}
	return result, nil
}

func bootstrapResult(err error) string {
	if err == nil {
		return "registered"
	}
	return fmt.Sprintf("retry: %v", err)
}

func waitBootstrap(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
