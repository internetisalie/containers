package containers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	mobyclient "github.com/moby/moby/client"
)

func TestReadinessConfiguration(t *testing.T) {
	probe := func(context.Context, *Resource) error { return nil }
	cases := []struct {
		name string
		cfg  ContainerConfig
		want string
	}{
		{"no readiness", ContainerConfig{}, "empty Port requires ReadyResource"},
		{"TCP callback without port", ContainerConfig{Ready: tcpDialProbe, ReadyResource: probe}, "Ready requires Port"},
		{"worker callback with port", ContainerConfig{Port: "4222", ReadyResource: probe}, "ReadyResource requires empty Port"},
		{"bad port", ContainerConfig{Port: "not-a-port"}, "invalid port"},
		{"negative timeout", ContainerConfig{Port: "4222", Timeout: -time.Second}, "Timeout must not be negative"},
		{"shared namespace and port", ContainerConfig{Port: "4222", NetworkMode: "container:peer"}, "Port cannot be published"},
		{"host and port", ContainerConfig{Port: "4222", NetworkMode: "host"}, "Port cannot be published"},
		{"none and port", ContainerConfig{Port: "4222", NetworkMode: "none"}, "Port cannot be published"},
		{"missing peer", ContainerConfig{ReadyResource: probe, NetworkMode: "container:"}, "requires a peer"},
	}
	// A feature refusal must occur before daemon access, not merely fail later
	// with an unrelated Docker connection error.
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "no-daemon.sock"))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := StartContainer(tc.cfg)
			if r != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("StartContainer = %v, %v; want %q before daemon access", r, err, tc.want)
			}
		})
	}
	for _, cfg := range []ContainerConfig{
		{Port: "4222"},
		{Port: "4222", Ready: tcpDialProbe},
		{ReadyResource: probe},
		{ReadyResource: probe, NetworkMode: "container:peer"},
		{ReadyResource: probe, NetworkMode: "host"},
		{ReadyResource: probe, NetworkMode: "none"},
		{Port: "4222", NetworkMode: "custom-network"},
	} {
		if err := validateReadiness(cfg); err != nil {
			t.Errorf("valid configuration rejected: %v", err)
		}
	}
}

func TestUninitializedResourceOperations(t *testing.T) {
	ctx := t.Context()
	for _, r := range []*Resource{nil, {}} {
		if r.ID() != "" || r.Name() != "" || r.HostPort() != "" {
			t.Fatal("uninitialized resource has an identity or address")
		}
		if err := r.Purge(); err != nil {
			t.Fatal(err)
		}
		_, inspectErr := r.Inspect(ctx)
		_, waitErr := r.Wait(ctx)
		_, _, logsErr := r.Logs(ctx)
		_, execErr := r.Exec(ctx, []string{"true"})
		for _, err := range []error{inspectErr, waitErr, logsErr, execErr, r.Stop(ctx, time.Second), r.Kill(ctx)} {
			if err == nil {
				t.Error("operation on uninitialized resource returned success")
			}
		}
	}
}

func observer(t *testing.T) *mobyclient.Client {
	t.Helper()
	c, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := c.Ping(ctx, mobyclient.PingOptions{}); err != nil {
		t.Fatalf("Docker is required for the worker acceptance tests: %v", err)
	}
	return c
}

func readyFile(ctx context.Context, r *Resource) error {
	result, err := r.Exec(ctx, []string{"test", "-f", "/tmp/worker-ready"})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("worker has not written its ready file")
	}
	return nil
}

const workerScript = `trap 'echo graceful >&2; exit 23' TERM
echo boot-out
echo boot-err >&2
touch /tmp/worker-ready
while :; do sleep 0.1; done`

func workerConfig() ContainerConfig {
	return ContainerConfig{
		Name: "containers-worker", Repository: "nats", Tag: "2.10-alpine",
		Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{workerScript},
		Ephemeral: true, Timeout: 10 * time.Second, ReadyResource: readyFile,
		Limits: Limits{MemoryBytes: 64 << 20, CPUs: 0.5},
	}
}

func startWorker(t *testing.T, cfg ContainerConfig) *Resource {
	t.Helper()
	r, err := StartContainerContext(t.Context(), cfg)
	if err != nil {
		t.Fatal(err) // Never convert a configuration/feature failure into a skip.
	}
	t.Cleanup(func() {
		if t.Failed() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			out, stderr, err := r.Logs(ctx)
			t.Logf("container logs: stdout=%q stderr=%q error=%v", out, stderr, err)
		}
		if err := r.Purge(); err != nil {
			t.Errorf("purge %s: %v", r.Name(), err)
		}
	})
	return r
}

func assertRemoved(t *testing.T, c *mobyclient.Client, id string) {
	t.Helper()
	if id == "" {
		t.Fatal("test never observed the container; cleanup has not been exercised")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := c.ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Fatalf("container %s was not removed: %v", id, err)
	}
}

func TestWorkerConfigurationAndGracefulStop(t *testing.T) {
	c := observer(t)
	t.Setenv(podUIDEnv, "worker-test-pod")
	state := t.TempDir()
	if err := os.Chmod(state, 0o777); err != nil {
		t.Fatal(err)
	}
	facts := t.TempDir()
	if err := os.Chmod(facts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(facts, "config"), []byte("fixture-only"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := workerConfig()
	cfg.User = "1000:1000"
	cfg.Env = []string{"WORKER_PROBE=fixture-env"}
	cfg.Mounts = []Mount{{Source: state, Target: "/state"}, {Source: facts, Target: "/facts", ReadOnly: true}}
	cfg.Labels = map[string]string{"fixture": "worker"}
	cfg.Cmd = []string{"echo effect >> /state/effects\n" + workerScript}
	calls := 0
	cfg.ReadyResource = func(ctx context.Context, r *Resource) error {
		calls++
		if calls < 3 {
			return errors.New("waiting for external observation")
		}
		return readyFile(ctx, r)
	}
	r := startWorker(t, cfg)
	if calls != 3 {
		t.Fatalf("resource readiness was not retried serially: %d calls", calls)
	}
	if r.HostPort() != "" || r.ID() == "" || r.Name() == cfg.Name {
		t.Fatalf("worker identity/address: name=%q id=%q hostPort=%q", r.Name(), r.ID(), r.HostPort())
	}
	info, err := r.Inspect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.User != "1000:1000" || !reflect.DeepEqual([]string(info.Config.Entrypoint), []string{"/bin/sh", "-c"}) {
		t.Fatalf("user/entrypoint not passed to daemon: %+v", info.Config)
	}
	if info.HostConfig.PublishAllPorts || len(info.HostConfig.PortBindings) != 0 {
		t.Fatalf("outbound worker unexpectedly publishes ports: %+v", info.NetworkSettings)
	}
	// Docker may retain image EXPOSE metadata with empty bindings. That is not
	// publication: only a nonempty host binding exposes a port.
	for port, bindings := range info.NetworkSettings.Ports {
		if len(bindings) != 0 {
			t.Fatalf("outbound worker published %s: %v", port, bindings)
		}
	}
	if info.Config.Labels[PodUIDLabel] != "worker-test-pod" || info.Config.Labels["fixture"] != "worker" {
		t.Fatalf("worker ownership/caller labels missing: %v", info.Config.Labels)
	}
	if len(cfg.Labels) != 1 {
		t.Fatal("caller's labels were mutated")
	}
	if info.HostConfig.Memory != 64<<20 || info.HostConfig.NanoCPUs != 500000000 {
		t.Fatalf("worker limits missing: %+v", info.HostConfig.Resources)
	}
	result, err := r.Exec(t.Context(), []string{"sh", "-c", `id -u; echo "$WORKER_PROBE"; cat /facts/config; echo command-error >&2; exit 7`})
	if err != nil || result.StdOut != "1000\nfixture-env\nfixture-only" || result.StdErr != "command-error\n" || result.ExitCode != 7 {
		t.Fatalf("exec result = %+v, %v", result, err)
	}
	result, err = r.Exec(t.Context(), []string{"sh", "-c", "echo forbidden > /facts/config"})
	if err != nil || result.ExitCode == 0 {
		t.Fatalf("read-only mount was writable: %+v, %v", result, err)
	}
	if _, err := r.Exec(t.Context(), nil); err == nil {
		t.Fatal("empty exec command was accepted")
	}
	if err := r.Stop(t.Context(), -time.Second); err == nil {
		t.Fatal("negative stop grace was accepted")
	}
	if err := r.Stop(t.Context(), 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	exit, err := r.Wait(t.Context())
	if err != nil || exit != 23 {
		t.Fatalf("graceful stop did not run the TERM trap: exit=%d err=%v", exit, err)
	}
	info, err = r.Inspect(t.Context())
	if err != nil || info.State.Running || info.State.ExitCode != 23 {
		t.Fatalf("Inspect did not fetch current stopped state: %+v, %v", info.State, err)
	}
	out, stderr, err := r.Logs(t.Context())
	if err != nil || out != "boot-out\n" || stderr != "boot-err\ngraceful\n" {
		t.Fatalf("logs = %q, %q, %v", out, stderr, err)
	}
	if err := r.Purge(); err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, c, r.ID())
	if err := r.Purge(); err != nil {
		t.Fatalf("repeated purge: %v", err)
	}
	if _, err := r.Inspect(t.Context()); err == nil {
		t.Fatal("purged resource inspection returned success")
	}
}

func TestWorkerSharesPeerNetwork(t *testing.T) {
	observer(t)
	peer, err := StartContainer(ContainerConfig{
		Name: "containers-worker-peer", Repository: "nats", Tag: "2.10-alpine",
		Port: "4222", Ephemeral: true, Ready: natsGreeting, Timeout: 10 * time.Second,
		Limits: Limits{MemoryBytes: 64 << 20, CPUs: 0.5},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := peer.Purge(); err != nil {
			t.Error(err)
		}
	})
	cfg := workerConfig()
	cfg.NetworkMode = "container:" + peer.ID()
	r := startWorker(t, cfg)
	info, err := r.Inspect(t.Context())
	if err != nil || string(info.HostConfig.NetworkMode) != cfg.NetworkMode {
		t.Fatalf("shared network not configured: %+v, %v", info.HostConfig, err)
	}
	// Localhost must reach the peer, not merely have a convincing config string.
	result, err := r.Exec(t.Context(), []string{"sh", "-c", "nc -w 1 127.0.0.1 4222 | head -c 4"})
	if err != nil || result.ExitCode != 0 || result.StdOut != "INFO" {
		t.Fatalf("worker could not reach the peer over shared localhost: %+v, %v", result, err)
	}
	peerInfo, err := peer.Inspect(t.Context())
	if err != nil || len(peerInfo.NetworkSettings.Networks) == 0 {
		t.Fatalf("peer inspection lacks network facts: %v", err)
	}
}

func TestWorkerKillAndWaitCancellation(t *testing.T) {
	observer(t)
	r := startWorker(t, workerConfig())
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := r.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for a live worker did not honor cancellation: %v", err)
	}
	if err := r.requireRunning(t.Context()); err != nil {
		t.Fatalf("cancelled wait changed the worker lifecycle: %v", err)
	}
	if err := r.Kill(t.Context()); err != nil {
		t.Fatal(err)
	}
	exit, err := r.Wait(t.Context())
	if err != nil || exit != 137 {
		t.Fatalf("SIGKILL result: %d, %v", exit, err)
	}
	out, stderr, err := r.Logs(t.Context())
	if err != nil || out != "boot-out\n" || stderr != "boot-err\n" {
		t.Fatalf("kill ran graceful trap or lost logs: %q, %q, %v", out, stderr, err)
	}
}

func TestWorkerExecCancellation(t *testing.T) {
	observer(t)
	r := startWorker(t, workerConfig())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.Exec(ctx, []string{"sh", "-c", "touch /tmp/exec-attached; exec sleep 30"})
		done <- err
	}()
	// Do not cancel until ExecAttach really started the remote command. Otherwise
	// the test could pass by cancelling an ordinary HTTP request before upgrade.
	until := time.Now().Add(3 * time.Second)
	for {
		result, err := r.Exec(t.Context(), []string{"test", "-f", "/tmp/exec-attached"})
		if err != nil {
			t.Fatal(err)
		}
		if result.ExitCode == 0 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("exec never reached its attached transport")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("exec cancellation result: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = r.Kill(t.Context())
		t.Fatal("cancelled exec remained blocked on its hijacked connection")
	}
	if err := r.requireRunning(t.Context()); err != nil {
		t.Fatalf("exec cancellation killed the worker: %v", err)
	}
}

func TestWorkerFailedReadinessCleanup(t *testing.T) {
	c := observer(t)
	for _, mode := range []string{"timeout", "cancel", "exited", "exit-during-probe"} {
		t.Run(mode, func(t *testing.T) {
			cfg := workerConfig()
			cfg.Timeout = 3 * time.Second
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var id string
			// Keep a test-owned fallback for deliberately broken cleanup/admission
			// mutations. The assertion below still observes deletion BEFORE this
			// fallback runs, so it cannot manufacture a passing cleanup test.
			t.Cleanup(func() {
				if id == "" {
					return
				}
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cleanupCancel()
				_, err := c.ContainerRemove(cleanupCtx, id, mobyclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
				if err != nil && !errdefs.IsNotFound(err) {
					t.Errorf("test fallback cleanup: %v", err)
				}
			})
			probeErr := errors.New("external registration not yet visible")
			cfg.ReadyResource = func(ctx context.Context, r *Resource) error {
				id = r.ID()
				switch mode {
				case "cancel":
					cancel()
				case "exited", "exit-during-probe":
					if err := r.Stop(ctx, time.Second); err != nil {
						return err
					}
					if mode == "exit-during-probe" {
						return nil
					}
				}
				return probeErr
			}
			r, err := StartContainerContext(ctx, cfg)
			if r != nil {
				t.Cleanup(func() { _ = r.Purge() })
			}
			if r != nil || err == nil {
				t.Fatalf("failed worker was admitted: %v, %v", r, err)
			}
			switch mode {
			case "timeout":
				if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, probeErr) {
					t.Fatalf("timeout lost context/probe diagnostics: %v", err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("startup did not propagate caller cancellation: %v", err)
				}
			default:
				if !strings.Contains(err.Error(), "not running") {
					t.Fatalf("exited worker did not fail promptly: %v", err)
				}
			}
			assertRemoved(t, c, id)
		})
	}
}

func TestWorkerRelaunchPreservesStateAndFencesOldResource(t *testing.T) {
	observer(t)
	state := t.TempDir()
	cfg := workerConfig()
	// This unique test-local singleton lets the second Start replace the first
	// using existing library semantics, without touching another test's container.
	cfg.Name = uniqueName("containers-relaunch")
	cfg.Ephemeral = false
	cfg.User = "0:0"
	cfg.Mounts = []Mount{{Source: state, Target: "/state"}}
	cfg.Cmd = []string{"echo effect >> /state/effects\n" + workerScript}
	first := startWorker(t, cfg)
	second := startWorker(t, cfg)
	if first.ID() == second.ID() || first.Name() != second.Name() {
		t.Fatal("relaunch did not create a distinct container with the same logical name")
	}
	ctx := t.Context()
	_, inspectErr := first.Inspect(ctx)
	_, waitErr := first.Wait(ctx)
	_, execErr := first.Exec(ctx, []string{"touch", "/state/foreign-effect"})
	_, _, logsErr := first.Logs(ctx)
	for _, err := range []error{inspectErr, waitErr, execErr, logsErr, first.Kill(ctx), first.Stop(ctx, 0)} {
		if !errdefs.IsNotFound(err) {
			t.Errorf("stale resource did not address its removed ID: %v", err)
		}
	}
	if err := first.Purge(); err != nil {
		t.Fatal(err)
	}
	if err := second.requireRunning(ctx); err != nil {
		t.Fatalf("stale resource controlled the replacement: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(state, "effects"))
	if err != nil || string(data) != "effect\neffect\n" {
		t.Fatalf("retained effects were reset or duplicated: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(state, "foreign-effect")); !os.IsNotExist(err) {
		t.Fatalf("stale resource exec reached replacement: %v", err)
	}
}

func TestWorkerAutoRemoveStillPurges(t *testing.T) {
	c := observer(t)
	cfg := workerConfig()
	cfg.AutoRemove = true
	r := startWorker(t, cfg)
	if err := r.Stop(t.Context(), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := r.Purge(); err != nil {
		t.Fatalf("purge after daemon AutoRemove: %v", err)
	}
	assertRemoved(t, c, r.ID())
}

func TestWorkerAutoRemoveFailedReadiness(t *testing.T) {
	c := observer(t)
	for _, mode := range []string{"error", "panic"} {
		t.Run(mode, func(t *testing.T) {
			cfg := workerConfig()
			cfg.AutoRemove = true
			var observed *Resource
			probeErr := errors.New("fixture readiness rejected")
			t.Cleanup(func() {
				if observed != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_, err := c.ContainerRemove(ctx, observed.ID(), mobyclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
					if err != nil && !errdefs.IsNotFound(err) {
						t.Error(err)
					}
				}
			})
			cfg.ReadyResource = func(ctx context.Context, r *Resource) error {
				observed = r
				if err := r.Kill(ctx); err != nil {
					t.Fatal(err)
				}
				if mode == "panic" {
					panic(probeErr)
				}
				return probeErr
			}
			var r *Resource
			var err error
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				r, err = StartContainerContext(t.Context(), cfg)
			}()
			if mode == "panic" {
				if recovered != probeErr {
					t.Fatalf("panic was swallowed/replaced: %v", recovered)
				}
			} else if recovered != nil || r != nil || !errors.Is(err, probeErr) || strings.Contains(err.Error(), "failed startup cleanup") {
				t.Fatalf("failed readiness lost its cause or invented cleanup failure: %v, %v, %v", r, err, recovered)
			}
			if observed == nil {
				t.Fatal("readiness callback not reached")
			}
			assertRemoved(t, c, observed.ID())
			if observed.pool.Client() != nil {
				t.Fatal("failed readiness leaked the client")
			}
		})
	}
}

func TestWorkerClearsImageEntrypoint(t *testing.T) {
	observer(t)
	cfg := workerConfig()
	cfg.Entrypoint = []string{}
	cfg.Cmd = []string{"/bin/sh", "-c", workerScript}
	r := startWorker(t, cfg)
	info, err := r.Inspect(t.Context())
	if err != nil || len(info.Config.Entrypoint) != 0 {
		t.Fatalf("non-nil empty Entrypoint did not clear image default: %+v, %v", info.Config, err)
	}
}

func TestWorkerCancelledOperationsHaveNoLifecycleEffect(t *testing.T) {
	observer(t)
	r := startWorker(t, workerConfig())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, inspectErr := r.Inspect(ctx)
	_, execErr := r.Exec(ctx, []string{"touch", "/tmp/cancelled-effect"})
	_, _, logsErr := r.Logs(ctx)
	for _, err := range []error{inspectErr, execErr, logsErr, r.Stop(ctx, 0), r.Kill(ctx)} {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("operation ignored cancelled context: %v", err)
		}
	}
	if err := r.requireRunning(t.Context()); err != nil {
		t.Fatalf("cancelled operations affected the worker: %v", err)
	}
	result, err := r.Exec(t.Context(), []string{"test", "!", "-e", "/tmp/cancelled-effect"})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("cancelled exec created an effect: %+v, %v", result, err)
	}
}

func TestWorkerExecReportsExitAfterOutputCloses(t *testing.T) {
	observer(t)
	r := startWorker(t, workerConfig())
	result, err := r.Exec(t.Context(), []string{"sh", "-c", "exec 1>&- 2>&-; sleep 0.2; exit 9"})
	if err != nil || result.ExitCode != 9 || result.StdOut != "" || result.StdErr != "" {
		t.Fatalf("exec reported an incomplete exit status: %+v, %v", result, err)
	}
}
