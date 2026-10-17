# containers

A small Go integration-test harness built on `dockertest/v4` and the Moby SDK.
Start TCP services or outbound-only workers, wait for explicit readiness, and
clean up the containers you own. No Docker CLI subprocess is required.

## TCP services

Existing configurations retain their port discovery and readiness behavior:

```go
db, err := containers.StartContainer(containers.ContainerConfig{
    Name: "test-db", Repository: "postgres", Tag: "17",
    Port: "5432", Ephemeral: true,
    Env: []string{"POSTGRES_HOST_AUTH_METHOD=trust"}, // isolated test data only
    Limits: containers.Limits{MemoryBytes: 512 << 20, CPUs: 1},
    Ready: func(address string) error {
        return checkDatabase(address) // supply a bounded, real identity/readiness probe
    },
})
if err != nil {
    return err
}
defer db.Purge() // production test code should check cleanup errors
address := db.HostPort()
```

`Ready` is optional for TCP services; without it, readiness is a successful TCP
connection. A custom `Ready` runs once per readiness attempt, after TCP address
selection. `Ephemeral` randomizes the name and asks Docker for a free host port.
Without it, starting a singleton **force-removes any existing container of that
name**. Prefer ephemeral fixtures on shared daemons.

## Outbound workers

Leave `Port` empty and supply `ReadyResource`. No host ports are published, even
when the image declares `EXPOSE`. `HostPort()` returns `""`. Do not set `Ready` in
this mode. Both modes retain mounts, limits, labels and automatic pod ownership.

```go
worker, err := containers.StartContainerContext(ctx, containers.ContainerConfig{
    Name: "test-worker", Repository: "test-agent", Tag: "local",
    Ephemeral: true, Timeout: 30 * time.Second,
    User: "1000:1000",
    Entrypoint: []string{"/usr/local/bin/agent"},
    Cmd: []string{"--workspace", "/workspace"},
    NetworkMode: "container:" + proxy.ID(),
    Mounts: []containers.Mount{
        {Source: workspace, Target: "/workspace"},
        {Source: facts, Target: "/workspace/.supervisor", ReadOnly: true},
    },
    Limits: containers.Limits{MemoryBytes: 768 << 20, CPUs: 1},
    ReadyResource: func(ctx context.Context, _ *containers.Resource) error {
        return checkProxyRegistration(ctx, workspaceID) // application-specific observation
    },
})
if err != nil {
    return err
}
defer worker.Purge()
```

`ReadyResource` is retried serially, with a startup-bounded context. It may use
`Inspect`/`Exec` or observe an external API, and **must honor its context**. Startup
fails if the worker exits or is paused/restarting, including when it exits during
a successful callback. The harness does not infer readiness merely from creation.

- `User` overrides the image user; empty retains the image default.
- `Entrypoint == nil` retains the image default; a non-nil empty slice clears it.
- `NetworkMode` is Docker's network mode/name, including `container:<peer ID>`.
  Empty retains Docker's default. Published ports cannot be combined with
  `container:`, `host` or `none` networking.
- Locally present images are reused without pulling. Missing images follow
  dockertest's normal pull behavior.

## Lifecycle and diagnostics

| Method | Contract |
| --- | --- |
| `ID()` / `Name()` | Immutable Docker ID / actual name (including ephemeral suffix). |
| `Inspect(ctx)` | Fetch fresh daemon state, including networking, mounts and exit status. |
| `Stop(ctx, grace)` | Graceful stop, then SIGKILL after the grace period. Durations round up to seconds; zero waits no grace; negative is rejected. |
| `Kill(ctx)` | Send SIGKILL. Does not purge; use `Wait` to observe completion. |
| `Wait(ctx)` | Wait until not running; return the actual exit code. Nonzero exit is not a transport error. |
| `Logs(ctx)` | Finite, separately decoded stdout/stderr snapshot. |
| `Exec(ctx, argv)` | Direct argv, no implicit shell or TTY, using the configured user. Returns `StdOut`, `StdErr`, `ExitCode`; check **both** error and exit code. |
| `Purge()` | Stop/remove the container and close the owned client. Nil/repeated calls are safe; cleanup errors are returned. |

All controls target the **ID**, not the name. A stale resource cannot control a
same-name replacement. A relaunch is another explicit `StartContainer[Context]`
using the caller's retained mounts; the library never replays application work or
automatically restarts a container.

Startup is bounded by both the caller context and `Timeout` (default 60 seconds),
including image pull/create. Once startup succeeds, cancelling that context does
not stop the container. Failure cleanup uses a separate bounded context. A private
per-start label fences cleanup of containers created but not yet registered by
dockertest; cleanup failures are included in the returned error. This is not a
crash-proof reaper: process death, unreachable Docker or an uncertain daemon-side
create completing after cleanup can still require external ownership-based reaping.

`Stop`/`Kill` retain diagnostics unless `AutoRemove` was configured. With
`AutoRemove`, Docker deletes on exit; subsequent `Wait`/`Inspect`/`Logs` may return
not found. `Purge` is still needed to close the client and handles concurrent
daemon removal. It closes its client even on cleanup failure; a failed cleanup
must not be interpreted as confirmed deletion. **Do not race `Purge` with other
resource operations.**

Logs and exec output are buffered in memory, so keep fixture output bounded.
Cancelling `Exec` closes its attached transport promptly, but **does not guarantee
the remote command is killed**. Use the container lifecycle controls when the
test needs termination, then purge.

## Verification

```sh
go test -p 1 -race ./... -count=1 -timeout=180s
go vet ./...
```

The worker acceptance tests require Docker and fail rather than skip when Docker
or a feature is unavailable. They use ephemeral or uniquely named disposable
containers, not installed services. The default test image is `nats:2.10-alpine`.
In the OpenCode development workspace, run commands through
`../run-with-worktree-scratch "$PWD" COMMAND ...` so temporary files and caches
use disk-backed worktree scratch rather than the host's `/tmp`.

Implementation scope and verification evidence: [container-features.md](container-features.md).
