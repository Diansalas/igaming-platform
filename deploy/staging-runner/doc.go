// Package stagingrunner holds the static guards (runner_static_test.go) for the staging runner image's shell
// modes and SQL packs (STAGING-PREP-3, docs/plans/staging-prep-3-record.md). The runner itself is bash and SQL,
// run as a one-off ECS task of deploy/docker/staging-runner.Dockerfile.
package stagingrunner
