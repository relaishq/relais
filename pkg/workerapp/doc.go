// Package workerapp provides the worker process lifecycle used by relais-worker:
// flags and environment configuration, stores, media and relay attachment,
// control heartbeats and rejoin, private HTTP, readiness, metrics, and drain.
//
// This is a supported pre-1.0 API. Pin releases: public signatures remain
// compatible within a minor version, with deprecation before removal in a later
// minor and migration notes for breaking minor versions. Snapshot compatibility
// is a separate contract.
package workerapp
