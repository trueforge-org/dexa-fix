// Package workers is the parent folder of the worker processes' code: one package per provider
// (pkg/workers/withings, pkg/workers/strengthlevel, ...) next to the generic loop in
// pkg/workers/runner. A worker has no database: it leases jobs from the backend over
// pkg/shared/workerapi, talks to the outside service, and reports back. boundary_test.go keeps it so.
package workers
