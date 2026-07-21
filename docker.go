// Package containers is a small, verified dockertest harness for integration
// tests: it starts a single container, prunes any pre-existing one with the same
// name, guards against orphans via pool.Close, and blocks on a caller-supplied
// readiness probe. Adapted from the sim project's proven harness and generalized
// so the readiness check is the caller's concern (a NATS greeting-read, a SQL
// ping, a plain TCP dial, …).
package containers

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
	"github.com/ory/dockertest/v4"
)

// DefaultTimeout is used when ContainerConfig.Timeout is zero.
const DefaultTimeout = 60 * time.Second

// ReadyCheck is retried until it succeeds or the timeout elapses. It receives the
// host address (host:port) the container's mapped Port is reachable at.
type ReadyCheck func(hostPort string) error

// ContainerConfig defines the properties required to manage a singleton test container.
type ContainerConfig struct {
	Name       string        // container name (also used to prune a stale prior instance)
	Repository string        // image repository, e.g. "cockroachdb/cockroach"
	Tag        string        // image tag, e.g. "v24.3.9"
	Port       string        // container port to expose + bind, e.g. "26257"
	Cmd        []string      // optional command override
	Env        []string      // optional environment variables
	Timeout    time.Duration // readiness timeout (DefaultTimeout if zero)
	// Ready is retried until it succeeds or Timeout elapses. If nil, a plain TCP
	// dial of the mapped host:port is used (sufficient for servers that accept a
	// connection immediately; supply a real probe for databases).
	Ready ReadyCheck
}

// Resource represents a running container managed by this harness.
type Resource struct {
	pool     dockertest.ClosablePool
	resource dockertest.ClosableResource
	port     string
}

// HostPort returns the host address (host:port) the container's Port is mapped to.
func (r *Resource) HostPort() string {
	if r == nil || r.resource == nil {
		return ""
	}
	return net.JoinHostPort("127.0.0.1", r.resource.GetPort(r.port+"/tcp"))
}

// StartContainer prunes any pre-existing container with the given name, starts a
// fresh one bound to cfg.Port, and blocks until the readiness probe passes (or
// Timeout elapses). The caller must Purge the Resource to stop the container.
func StartContainer(cfg ContainerConfig) (*Resource, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}

	ctx := context.Background()
	pool, err := initPool(ctx, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	if errRemove := removeExisting(ctx, pool, cfg.Name); errRemove != nil {
		_ = pool.Close(ctx)
		return nil, errRemove
	}

	res, err := runContainer(ctx, pool, cfg)
	if err != nil {
		_ = pool.Close(ctx)
		return nil, err
	}

	r := &Resource{pool: pool, resource: res, port: cfg.Port}
	if err := waitForReady(ctx, pool, cfg, r); err != nil {
		_ = r.Purge()
		return nil, err
	}
	return r, nil
}

// Purge removes the running container from the Docker daemon. Safe on nil.
func (r *Resource) Purge() error {
	if r == nil || r.pool == nil || r.resource == nil {
		return nil
	}
	ctx := context.Background()
	if err := r.resource.Close(ctx); err != nil {
		return fmt.Errorf("failed to purge container: %w", err)
	}
	return r.pool.Close(ctx)
}

func initPool(ctx context.Context, timeout time.Duration) (dockertest.ClosablePool, error) {
	pool, err := dockertest.NewPool(ctx, "", dockertest.WithMaxWait(timeout))
	if err != nil {
		return nil, fmt.Errorf("failed to create dockertest pool: %w", err)
	}
	return pool, nil
}

func removeExisting(ctx context.Context, pool dockertest.ClosablePool, name string) error {
	opts := mobyclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true}
	_, err := pool.Client().ContainerRemove(ctx, name, opts)
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("failed to remove pre-existing container %s: %w", name, err)
	}
	return nil
}

func runContainer(ctx context.Context, pool dockertest.ClosablePool, cfg ContainerConfig) (dockertest.ClosableResource, error) {
	port, err := network.ParsePort(cfg.Port + "/tcp")
	if err != nil {
		return nil, fmt.Errorf("invalid port %q: %w", cfg.Port, err)
	}
	bindings := network.PortMap{
		port: []network.PortBinding{{
			HostIP:   netip.MustParseAddr("0.0.0.0"),
			HostPort: cfg.Port,
		}},
	}

	res, err := pool.Run(ctx, cfg.Repository,
		dockertest.WithTag(cfg.Tag),
		dockertest.WithName(cfg.Name),
		dockertest.WithCmd(cfg.Cmd),
		dockertest.WithEnv(cfg.Env),
		dockertest.WithPortBindings(bindings),
		dockertest.WithoutReuse(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to run container %s: %w", cfg.Name, err)
	}
	return res, nil
}

func waitForReady(ctx context.Context, pool dockertest.ClosablePool, cfg ContainerConfig, r *Resource) error {
	probe := cfg.Ready
	if probe == nil {
		probe = tcpDialProbe
	}
	hostPort := r.HostPort()
	if err := pool.Retry(ctx, cfg.Timeout, func() error { return probe(hostPort) }); err != nil {
		return fmt.Errorf("container %s failed ready check: %w", cfg.Name, err)
	}
	return nil
}

// tcpDialProbe is the default readiness check: a successful TCP connect.
func tcpDialProbe(hostPort string) error {
	conn, err := net.DialTimeout("tcp", hostPort, time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}
