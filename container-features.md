# Outbound-worker container features

Status: **implemented and locally verified on 2026-10-16**, on
`work/loom-workspace-container-harness`, based on `50ebe87` (`v0.5.0`).
Review follow-up verification on 2026-10-17 is recorded below; it supersedes the
initial whole-startup interpretation of `Timeout`.

## Scope

Extend the shared Go library for the existing Loom agent fixture, rather than
adding another Docker CLI harness. Preserve the existing TCP-service configuration,
readiness callback, ephemeral identities, ownership labels, limits and mounts.
Phase 2, consumer migration, release and service deployment are not part of this
library change. The branch was subsequently pushed and PR #4 opened for review.

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
  The caller context bounds Docker operations; `Timeout` bounds readiness only,
  starting after registration, preserving the TCP contract. Callbacks must be
  bounded (`ReadyResource` must honor its context). On failure use a separate
  bounded cleanup context, preserving cleanup failures in the error.
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

## Initial verification at `e4cfbe1` (2026-10-16)

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

Commands (with `GOWORK=off GOTOOLCHAIN=local`; the implementation environment used
an external disk-backed scratch wrapper and a provisioned Go toolchain):

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

Implementer-local logs are `.agent-scratch/final-race.log`, `worker-repeat.log` and
`startup-repeat.log`; the coverage profile is adjacent. Scratch is ignored, not
part of the repository or release, and these logs are not reviewer-accessible
artifacts. Reproduce the commands above rather than relying on local log links.
All targeted disposable worker/relaunch
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

## Review follow-up (2026-10-17)

[PR #4](https://gitea.internetisalie.net/internetisalie/containers/pulls/4) has a
self-contained [requirements and acceptance comment](https://gitea.internetisalie.net/internetisalie/containers/pulls/4#issuecomment-33213).
The independent [request-changes review](https://gitea.internetisalie.net/internetisalie/containers/pulls/4#pullrequestreview-5789)
reported four medium findings. The implementing session made these corrections;
independent acceptance of the corrections is still required:

- **M1 — late create after cancellation:** if the initial name inspection finds
  nothing owned by this attempt, a cancelled startup polls all states using the
  private per-start label for up to two seconds. Matching results are removed by
  ID; foreign results are ignored. Listing/removal failures remain errors. This
  bounds recovery, not daemon-side completion: arbitrarily late create and process
  death still need an external reaper. The deterministic test releases a delayed
  create only after initial NotFound and an initial list without the fixture;
  it verifies a subsequent search removes the created ID, not a foreign result.
  Non-cancelled failures do not pay that polling delay.
- **M2 — TCP timeout compatibility:** `Timeout` again covers only readiness, after
  pull/create/start/registration, for both TCP and worker modes. A caller deadline
  bounds the whole operation, subject to callbacks honoring their bounds. Tests
  delay uncached image pull and container creation beyond `Timeout` and require
  successful startup with a fresh readiness budget in both modes.
- **M3 — failed-start AutoRemove:** deferred failure cleanup uses `Resource.Purge`
  once registered, so explicit purge and failed readiness share observed-removal
  conflict handling. Deterministic and actual-Docker tests exercise the failure
  path, not just successful-start cleanup.
- **M4 — readiness panic:** cleanup is deferred immediately after pool creation;
  it removes the registered resource and closes the client during unwinding,
  without swallowing/replacing the original panic. Both worker and TCP callbacks
  are tested. A cleanup error cannot be returned while propagating a panic.
- Documented daemon-side exec failures in stdout with status 126/127, peer-first
  teardown hazards (purge workers first), callback bounds, stop-signal behavior at
  zero grace, the reserved startup label and the intentional Moby inspection type.
  Restored the purge-error prefix and removed the ineffective `ExposedPorts=nil`
  assignment. Removed external workspace commands/links from the public README;
  local evidence locations here are explicitly non-portable, unshipped artifacts.

Final follow-up gate: **45 top-level tests passed with race detection, no skips**,
92.1% statement coverage. Existing 20 baseline tests and `go.mod`/`go.sum` remain
unchanged. `go vet ./...`, `go build ./...`, gofumpt and diff checks passed.
All seven newly added top-level review regression tests passed five consecutive
race-enabled runs before mutation; the full suite was rerun after restoration.
The local final log/profile are `review-final-race.log` and
`review-final-coverage.out` under ignored `.agent-scratch`, not forge CI evidence.

An additional scratch Go harness ran 80 actual-Docker starts with deterministic
random caller deadlines of 0–400 ms (seed 42), AutoRemove enabled, the same bounded
NATS image fixture, and an isolated batch label. It observed **43 successful
starts, 37 cancelled starts, zero remaining batch containers** when listing all
states after each result/cleanup. The harness and `review-cancel-stress.log` are
local scratch evidence, not shipped tests. This does not establish absence of
objects appearing beyond the documented recovery window or on other daemons.

Nine additional compiling mutations were caught and restored (not a rerun of all
19 original mutations):

| Deliberate regression | Test that caught it |
| --- | --- |
| Omit late-create recovery in cleanup logic | `TestCancelledCreateIsFoundAfterInitialNotFound` |
| Do not repeat the initial label search | `TestCancelledCreateIsFoundAfterInitialNotFound` |
| Trust a foreign result's ID without checking its label | `TestCancelledCreateIsFoundAfterInitialNotFound` |
| Poll certain, non-cancelled failures | `TestUncancelledCreateFailureDoesNotPollOrRemoveForeign` |
| Hide a late-create lookup failure | `TestLateCreateLookupFailureIsReported` |
| Disable cancellation recovery at the startup call site | `TestCancelledCreateIsFoundAfterInitialNotFound` |
| Consume readiness timeout during pull/create | `TestReadinessTimeoutDoesNotIncludePullOrCreate`, TCP and worker, pull and create |
| Use pool-close directly for registered failure cleanup | `TestFailedReadinessWaitsForAutoRemoval` |
| Skip deferred cleanup on panic | `TestReadinessPanicCleansUpAndPropagates`, worker and TCP |

Reproduce the follow-up gate with the same full-race/build/vet commands above.
The seven review regressions can be repeated with:

```sh
go test -p 1 -race ./... \
  -run '^TestCancelledCreate|^TestUncancelledCreate|^TestLateCreate|^TestReadinessTimeout|^TestFailedReadiness|^TestReadinessPanic|^TestWorkerAutoRemoveFailed' \
  -count=5 -timeout=90s
```

## Delivery boundaries

Implementation and local verification were performed in the implementing session;
the independent review requested changes, and no approval is claimed. `go.mod` and
`go.sum` are unchanged. The branch is pushed for PR review; no merge, tag, release,
consumer dependency change or deployment has been performed. The README documents
the public API and its cancellation, buffering, cleanup and concurrency limits.

Loom's actual-agent binary/image scenarios were not rerun with this version:
Delivery still uses its existing dependency and Docker CLI helper. Library
readiness is not consumer adoption or permission to start Phase 2. Release and
consumer adoption must follow the project's PR/CI/review/main/tag policy; no tag
should be cut from this feature branch.

The linked PR requirements comment reproduces the relevant cross-project scope
and later consumer acceptance, without requiring access to local workspace docs.
