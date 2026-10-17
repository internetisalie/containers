# Outbound-worker container features

Status: **implemented and locally verified on 2026-10-16**, on
`work/loom-workspace-container-harness`, based on `50ebe87` (`v0.5.0`).

## Scope

Extend the shared Go library for the existing Loom agent fixture, rather than
adding another Docker CLI harness. Preserve the existing TCP-service configuration,
readiness callback, ephemeral identities, ownership labels, limits and mounts.
Phase 2, consumer migration, forge delivery/release and service deployment are not
part of this implementation request.

## Delivered API and implementation

- An empty `Port` selects an outbound worker and requires `ReadyResource`, a
  context-aware callback receiving the resource. Do not publish image-declared
  ports or perform TCP discovery in that mode. Reject ambiguous readiness modes
  before contacting Docker. Poll serially, fail on process exit, and retain the
  probe's diagnostic error when startup times out.
- Added `User`, `Entrypoint` and `NetworkMode` configuration. In particular,
  `container:<ID>` must work without accidentally publishing ports. Leave ordinary
  service defaults unchanged.
- Added `StartContainerContext`; the existing `StartContainer` remains a wrapper.
  Bound startup by configuration timeout and caller cancellation; on failure use
  a separate bounded cleanup context, preserving cleanup failures in the error.
- Added resource `ID`, fresh `Inspect`, `Stop`, `Kill`, `Wait`, `Logs` and `Exec`.
  Address operations by immutable container ID, never by reusable name. Stop
  accepts a nonnegative grace duration; Kill sends SIGKILL; Wait reports the real
  exit status. Preserve stdout, stderr and command exit status separately. Context
  cancellation closes attached exec transport; it does not claim to kill a remote
  command. Relaunch is a new Start over the caller's retained mounts, not an
  automatic in-place restart/replay policy.
- Reuse dockertest's SDK and resource operations where they meet these contracts.
  No dependency-version bump, custom generic Docker client, privileged mode or
  production-service access is needed.

## Verification

- Baseline: `go test -p 1 -race ./... -count=1 -timeout=180s` passed on 2026-10-16
  with Go 1.26.6 and Docker 29.1.3. Dependencies stay pinned; `GOWORK=off` and
  `GOTOOLCHAIN=local`. All commands use the workspace disk-backed scratch wrapper.
- Added fail-closed daemon-backed tests: configuration/readiness, delayed readiness,
  graceful stop versus kill, fresh inspection/wait status, logs and exec errors,
  cancellation, failure cleanup, same-name replacement identity and retained
  filesystem effects across relaunch. Explicitly fail if Docker is unavailable in
  the new acceptance tests; never treat a feature failure as a skip.
- Existing TCP tests are unchanged. All 38 top-level tests passed with race
  detection and no skips (20 existing tests, 18 new). Final statement coverage is
  92.2%; this is not a claim that every error branch was mutation-proved.
- `go vet ./...`, `go build ./...`, gofumpt and `git diff --check` passed.
- Worker/startup/purge tests passed three consecutive race-enabled runs. The
  startup cancellation/early-cancellation tests passed 100 consecutive
  race-enabled runs after the response/cancellation fix described below.
- gopls MCP is not available in this session's tool catalog. Dependency API/source
  inspection and Go compiler/tests are used instead; no exhaustive semantic
  reference-search claim is made.

Final commands (through `../run-with-worktree-scratch "$PWD"`, sourcing the
Glimmer environment and setting `GOWORK=off GOTOOLCHAIN=local`):

```sh
go test -p 1 -race ./... -count=1 -timeout=180s -v \
  -coverprofile=.agent-scratch/final-coverage.out
go test -p 1 -race ./... -run '^TestWorker|^TestStartup|^TestPurge' \
  -count=3 -timeout=180s
go test -p 1 -race ./... -run '^TestStartup|^TestAlready' \
  -count=100 -timeout=90s
go vet ./...
go build ./...
```

Local logs are `.agent-scratch/final-race.log`, `worker-repeat.log` and
`startup-repeat.log`; the coverage profile is adjacent. Scratch is ignored, not
part of the library source or release. All targeted disposable worker/relaunch
containers were absent after verification. The original Glimmer checkout remains
clean on `main`; no installed service was a test target.

### Findings corrected during verification

- Docker retains image `EXPOSE` metadata even with no published ports. The test
  checks that all actual host bindings are empty, rather than mistaking metadata
  for publication. Shared-peer namespace connectivity is tested over localhost.
- Docker AutoRemove races explicit removal and can return conflict while deleting.
  Purge now confirms disappearance, untracks the resource and closes the client;
  other removal failures remain errors. A deterministic fake-daemon test proves
  this race handling, with real-daemon coverage as well.
- dockertest can fail between create and resource registration, and its attempted
  removal uses the cancelled startup context. Failure cleanup now inspects with a
  fresh bounded context, compares a reserved per-start label, and removes by ID.
  Tests cover start/inspect cancellation, a replacement owning the lookup name,
  and cleanup refusal without hiding either failure. Caller label maps are copied.
- A late full race run exposed a response/cancellation race: an HTTP error could
  win transport completion after caller cancellation and omit the context cause.
  Startup errors now preserve both causes. The failing run is retained locally
  as `.agent-scratch/startup-response-race.log`; the corrected repeated runs and
  final full suite passed. Reintroducing the defect was caught in 100 targeted
  iterations, not counted as deterministic single-run coverage.
- Exec explicitly closes the hijacked attachment on cancellation. After output
  closes, it also waits for a completed inspected exit status rather than
  returning an incomplete status. The cancellation test waits for a remote
  command effect before cancelling, proving the attached phase was reached.

### Mutation evidence

Nineteen distinct compiling defects were introduced and caught by targeted tests,
then restored from current-tree snapshots. None survived; invalid/build-failure
results were not counted. Startup cleanup/ownership/error-preservation mutations
were rerun after the response/cancellation correction. All mutation snapshots
were closed before the final race suite. A test-owned fallback cleanup runs only
after assertions, so a failed mutation cannot leave a fixture behind or fabricate
a successful cleanup assertion.

| Deliberate defect | Test that caught it |
| --- | --- |
| Publish image ports for a worker | `TestWorkerConfigurationAndGracefulStop` |
| Drop user override | `TestWorkerConfigurationAndGracefulStop` |
| Ignore non-nil empty entrypoint | `TestWorkerClearsImageEntrypoint` |
| Drop network namespace selection | `TestWorkerSharesPeerNetwork` |
| Admit a worker that exited during its successful probe | `TestWorkerFailedReadinessCleanup/exit-during-probe` |
| Omit unregistered-resource cleanup | `TestStartupCancellationBeforeResourceRegistration/start` |
| Remove another start's container without checking its label | `TestStartupCancellationBeforeResourceRegistration/foreign` |
| Truncate positive stop grace to seconds | `TestStopGraceReachesDockerInWholeSeconds` |
| Change Kill to SIGTERM | `TestWorkerKillAndWaitCancellation` |
| Invent zero process exit status | `TestWorkerKillAndWaitCancellation` |
| Leave cancelled exec attached | `TestWorkerExecCancellation` |
| Kill by reusable name | `TestWorkerRelaunchPreservesStateAndFencesOldResource` |
| Exec by reusable name | `TestWorkerRelaunchPreservesStateAndFencesOldResource` |
| Return creation-time inspection snapshot | `TestWorkerConfigurationAndGracefulStop` |
| Omit AutoRemove race handling | `TestPurgeWaitsForDaemonAutoRemoval` |
| Treat daemon Wait error as successful exit | `TestWaitDoesNotTurnDaemonFailureIntoExitSuccess` |
| Bypass readiness validation at Start | `TestReadinessConfiguration` |
| Hide failed-start cleanup error | `TestStartupCancellationBeforeResourceRegistration/cleanup-error` |
| Lose cancellation when daemon response wins | `TestStartupCancellationBeforeResourceRegistration`, 100 targeted iterations |

## Delivery boundaries

Implementation and verification were performed in the implementing session; no
independent review approval is claimed. `go.mod` and `go.sum` are unchanged. No
remote push, PR, merge, tag, release, consumer dependency change or deployment has
been performed. The README documents the public API and its cancellation,
buffering, cleanup and concurrency limits.

Loom's actual-agent binary/image scenarios were not rerun with this version:
Delivery still uses its existing dependency and Docker CLI helper. Library
readiness is not consumer adoption or permission to start Phase 2. Release and
consumer adoption must follow the project's PR/CI/review/main/tag policy; no tag
should be cut from this feature branch.

Related sequencing/consumer acceptance:
`../loom-proxy-observation-api-scope.md`, “Container-library prerequisite”.
