package containers

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
	"github.com/ory/dockertest/v4"
)

// ID returns the immutable Docker ID. All operations use this ID, so a stale
// resource can never control a replacement that happens to have the same name.
func (r *Resource) ID() string {
	if r == nil || r.resource == nil {
		return ""
	}
	return r.resource.ID()
}

func (r *Resource) checkUsable() error {
	if r == nil || r.resource == nil || r.pool == nil || r.pool.Client() == nil {
		return fmt.Errorf("container resource is not initialized or has been purged")
	}
	return nil
}

// dockertest deliberately exposes a smaller client interface. Its actual client
// is the Moby SDK client; use the SDK interface for the two extra lifecycle calls
// rather than opening another connection or shelling out to Docker.
func (r *Resource) containerAPI() (mobyclient.ContainerAPIClient, error) {
	if err := r.checkUsable(); err != nil {
		return nil, err
	}
	api, ok := r.pool.Client().(mobyclient.ContainerAPIClient)
	if !ok {
		return nil, fmt.Errorf("container client does not support lifecycle operations")
	}
	return api, nil
}

// Inspect fetches current daemon state, not the snapshot taken at creation. It
// includes network addresses/gateways, configuration, mounts and process state.
func (r *Resource) Inspect(ctx context.Context) (container.InspectResponse, error) {
	if err := r.checkUsable(); err != nil {
		return container.InspectResponse{}, err
	}
	result, err := r.pool.Client().ContainerInspect(ctx, r.ID(), mobyclient.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, fmt.Errorf("inspect container %s: %w", r.Name(), err)
	}
	return result.Container, nil
}

// Stop asks Docker to stop the container gracefully, then SIGKILL it if it has
// not exited within grace. Positive durations round up to whole seconds; zero
// means immediate forceful termination. The container remains available for
// inspection/logs unless AutoRemove was configured. Purge is still required.
func (r *Resource) Stop(ctx context.Context, grace time.Duration) error {
	if grace < 0 {
		return fmt.Errorf("stop grace must not be negative")
	}
	if err := r.checkUsable(); err != nil {
		return err
	}
	seconds := int(grace / time.Second)
	if grace%time.Second != 0 {
		seconds++
	}
	_, err := r.pool.Client().ContainerStop(ctx, r.ID(), mobyclient.ContainerStopOptions{Timeout: &seconds})
	if err != nil {
		return fmt.Errorf("stop container %s: %w", r.Name(), err)
	}
	return nil
}

// Kill sends SIGKILL, without removing the container. Use Wait to observe the
// exit status. With AutoRemove, inspection/wait after exit may report not found.
func (r *Resource) Kill(ctx context.Context) error {
	api, err := r.containerAPI()
	if err != nil {
		return err
	}
	_, err = api.ContainerKill(ctx, r.ID(), mobyclient.ContainerKillOptions{Signal: "SIGKILL"})
	if err != nil {
		return fmt.Errorf("kill container %s: %w", r.Name(), err)
	}
	return nil
}

// Wait waits for the container to stop and returns its actual exit status. A
// nonzero process exit is a result, not a transport error. It does not remove it.
func (r *Resource) Wait(ctx context.Context) (int64, error) {
	api, err := r.containerAPI()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := api.ContainerWait(ctx, r.ID(), mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-result.Error:
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("wait container %s: %w", r.Name(), err)
	case status := <-result.Result:
		if status.Error != nil {
			return 0, fmt.Errorf("wait container %s: %s", r.Name(), status.Error.Message)
		}
		return status.StatusCode, nil
	}
}

// Logs returns a finite snapshot of stdout and stderr, without following. Output
// is buffered in memory; callers should keep test-container output bounded.
func (r *Resource) Logs(ctx context.Context) (stdout, stderr string, err error) {
	if err := r.checkUsable(); err != nil {
		return "", "", err
	}
	return r.resource.Logs(ctx)
}

// ExecResult separates command output from its process exit code. A nonzero
// ExitCode is not a Docker/transport error; callers must check both.
type ExecResult struct {
	StdOut   string
	StdErr   string
	ExitCode int
}

// Exec executes argv directly, without a shell or TTY, as the container's user.
// It buffers output in memory. Cancellation closes the attached transport and
// returns an error; it does not guarantee termination of the remote command.
func (r *Resource) Exec(ctx context.Context, cmd []string) (ExecResult, error) {
	if err := r.checkUsable(); err != nil {
		return ExecResult{}, err
	}
	if len(cmd) == 0 {
		return ExecResult{}, fmt.Errorf("exec requires a command")
	}
	created, err := r.pool.Client().ExecCreate(ctx, r.ID(), mobyclient.ExecCreateOptions{
		Cmd: cmd, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec create: %w", err)
	}
	attached, err := r.pool.Client().ExecAttach(ctx, created.ID, mobyclient.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec attach: %w", err)
	}
	defer attached.Conn.Close()
	// dockertest's Exec does not close the hijacked connection on ctx cancellation.
	// The SDK's ordinary request cancellation no longer owns it after the upgrade.
	stop := context.AfterFunc(ctx, func() { _ = attached.Conn.Close() })
	defer stop()
	var stdout, stderr bytes.Buffer
	_, readErr := stdcopy.StdCopy(&stdout, &stderr, attached.Reader)
	result := ExecResult{StdOut: stdout.String(), StdErr: stderr.String(), ExitCode: -1}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if readErr != nil {
		return result, fmt.Errorf("exec read: %w", readErr)
	}
	for {
		info, err := r.pool.Client().ExecInspect(ctx, created.ID, mobyclient.ExecInspectOptions{})
		if err != nil {
			return result, fmt.Errorf("exec inspect: %w", err)
		}
		if !info.Running {
			result.ExitCode = info.ExitCode
			return result, nil
		}
		// Output can close before process exit (including a command that closes
		// its own descriptors). Do not return an invented/incomplete exit code.
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func waitRemoved(ctx context.Context, pool dockertest.ClosablePool, id string) error {
	for {
		_, err := pool.Client().ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{})
		if errdefs.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("observe removal of container %s: %w", id, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
