package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
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

// bootstrapWorkers retries each configured worker independently, so one slow
// host cannot prevent a healthy host from registering. Every request has a
// deadline and retries back off from 100 ms to at most 5 s until shutdown.
func bootstrapWorkers(ctx context.Context, plane *controlplane.Plane, entries []fixedWorker, firstRound chan<- struct{}) {
	var wg, first sync.WaitGroup
	first.Add(len(entries))
	go func() { first.Wait(); close(firstRound) }()
	defer wg.Wait()
	for _, entry := range entries {
		wg.Go(func() {
			worker := &controlplane.RemoteWorker{URL: entry.endpoint}
			var initial sync.Once
			defer initial.Do(first.Done)
			for attempt, delay := 1, 100*time.Millisecond; ; attempt, delay = attempt+1, min(delay*2, 5*time.Second) {
				if ctx.Err() != nil {
					return
				}
				bounded, cancel := context.WithTimeout(ctx, time.Second)
				status, err := worker.Status(bounded)
				cancel()
				if err == nil {
					err = plane.Register(entry.name, status.Address, worker)
				}
				initial.Do(first.Done)
				log.Printf("worker bootstrap name=%s attempt=%d result=%s", entry.name, attempt, bootstrapResult(err))
				if err == nil {
					return
				}
				if !waitBootstrap(ctx, delay) {
					return
				}
			}
		})
	}
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
