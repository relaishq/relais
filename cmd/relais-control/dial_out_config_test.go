package main

import "testing"

func TestFixedMixedWorkerConfiguration(t *testing.T) {
	entries, err := parseWorkers([]string{"a=dial-out", "b=dial-out", "c=http://127.0.0.1:17001"})
	if err != nil || len(entries) != 3 {
		t.Fatalf("mixed fixed pool rejected: %v", err)
	}
	if _, err := parseWorkers([]string{"a=dial-out", "a=http://127.0.0.1:17001"}); err == nil {
		t.Fatal("duplicate worker name accepted")
	}
}
