// Package containers is a small, verified dockertest harness for integration
// tests: it starts a single container, prunes any pre-existing one with the same
// name, guards against orphans via pool.Close, and blocks on a caller-supplied
// readiness probe. Adapted from the sim project's proven harness and generalized
// so the readiness check is the caller's concern (a NATS greeting-read, a SQL
// ping, a plain TCP dial, …).
package containers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	// Labels are stamped on the container. They are how anything OUTSIDE this
	// process identifies what it started: the container is a host Docker object,
	// so a CI reaper, an operator or a sweep has the daemon's view and nothing
	// else. A process token cannot serve — it is meaningless in another namespace
	// — and once Ephemeral randomises the name, labels are the only handle left.
	//
	// Prefer fleet-neutral keys owned by this package's import path, e.g.
	// "code.internetisalie.net/containers.pod-uid", over consumer-namespaced ones:
	// a reaper written against one spelling silently reaps nothing when a consumer
	// picks another, and silence is indistinguishable from a clean fleet.
	Labels map[string]string

	// Ephemeral makes this container safe to run beside another of the same kind:
	// the name gets a random suffix and the host port is assigned by the daemon
	// rather than fixed.
	//
	// Without it, Name is the identity and StartContainer force-removes any prior
	// holder — so two concurrent callers destroy each other's container. That is
	// not hypothetical: it cost sim a day of rotating, mis-attributed test failures
	// whose symptoms named everything except the shared name, and it also explains
	// a daemon handing out an unexpected host port when a create raced a remove.
	// The blast radius is the DAEMON, not the process: -p 1 and t.Parallel
	// discipline bound one test binary, and any second process on the same daemon
	// is outside both.
	Ephemeral bool
}

// Resource represents a running container managed by this harness.
type Resource struct {
	pool     dockertest.ClosablePool
	resource dockertest.ClosableResource
	port     string
	hostPort string
	name     string
}

// Name is the container's actual name on the daemon. It is not necessarily the
// Name that was configured: an Ephemeral container is given a random suffix, and
// the caller needs the real one to inspect, exec against or reap it.
func (r *Resource) Name() string {
	if r == nil {
		return ""
	}
	return r.name
}

// HostPort returns the host address (host:port) this container was PROVEN
// reachable at — the readiness probe succeeded against it before StartContainer
// returned.
//
// It used to return 127.0.0.1 unconditionally, which is right only where the
// daemon shares the caller's network namespace. It does on a workstation and in a
// dind sidecar; it does not when dockerd runs on a VM host beside the caller's
// k3s pod, where loopback is refused and the bridge gateway answers. Deriving the
// answer needs a fact about the environment that is not available — "am I in
// Kubernetes?" is TRUE on both of one fleet's CI pools and only one of them needs
// a non-loopback address — so the address is measured rather than computed.
func (r *Resource) HostPort() string {
	if r == nil || r.resource == nil {
		return ""
	}
	if r.hostPort != "" {
		return r.hostPort
	}
	return net.JoinHostPort(localhost, r.resource.GetPort(r.port+"/tcp"))
}

// StartContainer prunes any pre-existing container with the given name, starts a
// fresh one bound to cfg.Port, and blocks until the readiness probe passes (or
// Timeout elapses). The caller must Purge the Resource to stop the container.
func StartContainer(cfg ContainerConfig) (*Resource, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Ephemeral {
		cfg.Name = uniqueName(cfg.Name)
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

	r := &Resource{pool: pool, resource: res, port: cfg.Port, name: cfg.Name}
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
	// An Ephemeral container asks the daemon to choose the host port. A fixed one
	// is a second shared resource beside the name: two concurrent callers collide
	// on it, and the loser either fails to start or is quietly given a different
	// port than it asked for.
	hostPort := cfg.Port
	if cfg.Ephemeral {
		hostPort = ""
	}
	bindings := network.PortMap{
		port: []network.PortBinding{{
			HostIP:   netip.MustParseAddr("0.0.0.0"),
			HostPort: hostPort,
		}},
	}

	res, err := pool.Run(ctx, cfg.Repository,
		dockertest.WithTag(cfg.Tag),
		dockertest.WithName(cfg.Name),
		dockertest.WithCmd(cfg.Cmd),
		dockertest.WithEnv(cfg.Env),
		dockertest.WithLabels(cfg.Labels),
		dockertest.WithPortBindings(bindings),
		dockertest.WithoutReuse(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to run container %s: %w", cfg.Name, err)
	}
	return res, nil
}

// waitForReady blocks until the container answers the caller's probe at one of the
// candidate addresses, and records which one answered. The probe does double duty:
// it establishes reachability AND identity, because it is the caller's own — a
// bare TCP connect would accept any stranger listening on that address, which is a
// claim about a port wearing the clothes of a claim about a service.
//
// Candidates race rather than queue. Sequentially, an address that is
// unroutable-but-not-refused burns the whole dial timeout before the next is
// tried, and rootless Docker, Docker Desktop and a remote DOCKER_HOST all put the
// bridge gateway somewhere that blackholes. Racing makes the cost the fastest
// candidate instead of the sum of the failures ahead of it.
func waitForReady(ctx context.Context, pool dockertest.ClosablePool, cfg ContainerConfig, r *Resource) error {
	probe := cfg.Ready
	if probe == nil {
		probe = tcpDialProbe
	}
	port := r.resource.GetPort(r.port + "/tcp")
	hosts := hostCandidates(ctx, pool)

	err := pool.Retry(ctx, cfg.Timeout, func() error {
		type result struct {
			hostPort string
			err      error
		}
		results := make(chan result, len(hosts))
		for _, host := range hosts {
			go func(host string) {
				addr := net.JoinHostPort(host, port)
				results <- result{addr, probe(addr)}
			}(host)
		}
		var lastErr error
		for range hosts {
			got := <-results
			if got.err == nil {
				r.hostPort = got.hostPort
				return nil
			}
			lastErr = got.err
		}
		return lastErr
	})
	if err != nil {
		return fmt.Errorf("container %s unreachable at any of %v: %w", cfg.Name, hosts, err)
	}
	return nil
}

// localhost is the address that works wherever the daemon shares the caller's
// network namespace: a workstation, and a job container sharing a pod with dind.
const localhost = "127.0.0.1"

// uniqueName suffixes a name so concurrent callers cannot collide on it. Docker
// names must stay in [a-zA-Z0-9][a-zA-Z0-9_.-]*, which hex satisfies.
func uniqueName(base string) string {
	const suffixBytes = 4
	buf := make([]byte, suffixBytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is not a reason to hand back a colliding name; the
		// nanosecond clock is a weaker but sufficient tiebreaker for a test fixture.
		return fmt.Sprintf("%s-%d", base, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%s", base, hex.EncodeToString(buf))
}

// hostCandidates returns the addresses worth trying for a published port, best
// first. The daemon is ASKED for its own bridge gateway rather than the
// environment being interrogated: a job pod on a separate CNI can route to that
// gateway even though loopback is refused.
//
// Loopback stays in the list and stays last. It is not an error fallback — it is
// the right answer wherever the daemon and the caller share a namespace, and the
// only candidate available if the daemon reports no bridge gateway at all.
func hostCandidates(ctx context.Context, pool dockertest.ClosablePool) []string {
	const bridgeNetwork = "bridge"

	hosts := make([]string, 0, 2)
	if pool != nil && pool.Client() != nil {
		// A daemon with no bridge network, or one that refuses the query, is not an
		// error: it means one candidate instead of two.
		if res, err := pool.Client().NetworkInspect(ctx, bridgeNetwork, mobyclient.NetworkInspectOptions{}); err == nil {
			for _, ipam := range res.Network.IPAM.Config {
				// IsLoopback rather than a comparison against the literal: the
				// gateway is a netip.Addr, and asking what it IS beats asking
				// whether it matches one spelling of what it might be.
				if ipam.Gateway.IsValid() && !ipam.Gateway.IsLoopback() {
					hosts = append(hosts, ipam.Gateway.String())
					break
				}
			}
		}
	}
	return append(hosts, localhost)
}

// tcpDialProbe is the default readiness check: a successful TCP connect.
func tcpDialProbe(hostPort string) error {
	conn, err := net.DialTimeout("tcp", hostPort, time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}
