package containers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
	"github.com/ory/dockertest/v4"
)

const startupLabel = "code.internetisalie.net/containers.startup-id"

// Recover a container that dockertest has not yet registered when Run fails.
// The lookup name alone is NOT proof of ownership. Inspect with a fresh context,
// check our per-start token, then remove by immutable ID, never by name.
func cleanupUntracked(pool dockertest.ClosablePool, cfg ContainerConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	info, err := pool.Client().ContainerInspect(ctx, cfg.Name, mobyclient.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect failed startup for cleanup: %w", err)
	}
	if info.Container.Config == nil || info.Container.Config.Labels[startupLabel] != cfg.Labels[startupLabel] {
		return nil // a different start owns the name now
	}
	_, err = pool.Client().ContainerRemove(ctx, info.Container.ID, mobyclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove failed startup: %w", err)
	}
	return nil
}

// validateReadiness runs before a pool is created or a singleton name is pruned.
func validateReadiness(cfg ContainerConfig) error {
	if cfg.Timeout < 0 {
		return fmt.Errorf("Timeout must not be negative")
	}
	if cfg.Port == "" {
		if cfg.ReadyResource == nil {
			return fmt.Errorf("empty Port requires ReadyResource")
		}
		if cfg.Ready != nil {
			return fmt.Errorf("Ready requires Port; use ReadyResource for outbound workers")
		}
	} else {
		if _, err := network.ParsePort(cfg.Port + "/tcp"); err != nil {
			return fmt.Errorf("invalid port %q: %w", cfg.Port, err)
		}
		if cfg.ReadyResource != nil {
			return fmt.Errorf("ReadyResource requires empty Port")
		}
		if strings.HasPrefix(cfg.NetworkMode, "container:") || cfg.NetworkMode == "host" || cfg.NetworkMode == "none" {
			return fmt.Errorf("Port cannot be published with NetworkMode %q", cfg.NetworkMode)
		}
	}
	if cfg.NetworkMode == "container:" {
		return fmt.Errorf("container NetworkMode requires a peer name or ID")
	}
	return nil
}

func waitForResource(ctx context.Context, cfg ContainerConfig, r *Resource) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("container %s readiness: %w", cfg.Name, errors.Join(err, lastErr))
		}
		if err := r.requireRunning(ctx); err != nil {
			return errors.Join(err, lastErr)
		}
		lastErr = cfg.ReadyResource(ctx, r)
		if lastErr == nil {
			// A successful external observation must not admit a container that
			// exited while its callback was running.
			return r.requireRunning(ctx)
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func (r *Resource) requireRunning(ctx context.Context) error {
	info, err := r.Inspect(ctx)
	if err != nil {
		return err
	}
	if info.State == nil || !info.State.Running || info.State.Restarting || info.State.Paused || info.State.Dead {
		return fmt.Errorf("container %s is not running: %+v", r.Name(), info.State)
	}
	return nil
}
