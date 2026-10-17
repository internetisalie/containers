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
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
	"github.com/ory/dockertest/v4"
)

// DefaultTimeout is used for readiness when ContainerConfig.Timeout is zero,
// and bounds independent failure cleanup.
const DefaultTimeout = 60 * time.Second

// ReadyCheck is retried until it succeeds or the timeout elapses. It receives the
// host address (host:port) the container's mapped Port is reachable at.
//
// It is called ONCE per readiness attempt, serially, against the single address
// selected for that attempt. It is never called concurrently and never against an
// address that is then discarded, so a probe may hold state, write to a *testing.T,
// or open a pooled connection. Candidate addresses are narrowed by a built-in TCP
// dial before this runs, precisely so that the concurrency stays on this side of
// the callback boundary.
type ReadyCheck func(hostPort string) error

// ResourceReadyCheck probes an outbound worker without a listening port. It is
// called serially until it succeeds, the container exits, or startup is cancelled.
// The callback must honor ctx; it may inspect/exec the resource or observe an
// external service to which the worker connects. It must not retain or purge r.
type ResourceReadyCheck func(ctx context.Context, r *Resource) error

// ContainerConfig defines the properties required to manage a singleton test container.
type ContainerConfig struct {
	Name       string        // container name (also used to prune a stale prior instance)
	Repository string        // image repository, e.g. "cockroachdb/cockroach"
	Tag        string        // image tag, e.g. "v24.3.9"
	Port       string        // TCP port to publish, e.g. "26257"; empty requires ReadyResource
	Cmd        []string      // optional command override
	Env        []string      // optional environment variables
	Timeout    time.Duration // readiness timeout (DefaultTimeout if zero)
	// Ready is retried until it succeeds or Timeout elapses. If nil, a plain TCP
	// dial of the mapped host:port is used (sufficient for servers that accept a
	// connection immediately; supply a real probe for databases).
	Ready ReadyCheck

	// ReadyResource is required when Port is empty. No ports are published in
	// this mode, including ports exposed by the image. Mutually exclusive with
	// Ready and Port; TCP-service behavior is unchanged when Port is set.
	ReadyResource ResourceReadyCheck

	// User and Entrypoint override the image defaults. A nil Entrypoint retains
	// the image default; a non-nil empty slice clears it.
	User       string
	Entrypoint []string
	// NetworkMode selects Docker networking, e.g. "container:<ID>" to share a
	// peer's network namespace. Empty retains Docker's default. Published ports
	// cannot be combined with container, host or none networking. Purge workers
	// before the peer; its loss is not supervised by this harness.
	NetworkMode string

	// Labels are stamped on the container. They are how anything OUTSIDE this
	// process identifies what it started: the container is a host Docker object,
	// so a CI reaper, an operator or a sweep has the daemon's view and nothing
	// else. A process token cannot serve — it is meaningless in another namespace
	// — and once Ephemeral randomises the name, labels are the only handle left.
	//
	// Prefer fleet-neutral keys owned by this package's import path over
	// consumer-namespaced ones: a reaper written against one spelling silently
	// reaps nothing when a consumer picks another, and silence is
	// indistinguishable from a clean fleet.
	//
	// These are ADDITIONAL to the ownership label this package stamps on its own
	// (PodUIDLabel, from the pod's downward-API UID). Nothing here is required for
	// a container to be reapable — see ownership.go for why that is not the
	// caller's job.
	// The reserved code.internetisalie.net/containers.startup-id key is always
	// replaced with a private per-start token, without changing the caller's map.
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

	// Limits bound what this container may consume. Zero is unbounded, which is
	// Docker's default. See Limits.
	Limits Limits

	// AutoRemove asks the DAEMON to delete the container once it exits, so a
	// container that crashes or is stopped leaves nothing behind for the next run
	// to trip over.
	//
	// READ WHAT THIS DOES NOT DO, because the name invites the wrong conclusion. It
	// fires when the CONTAINER exits. It does not fire when the caller's process
	// dies: a SIGKILLed test leaves its container RUNNING, and a running container
	// is exactly what AutoRemove never touches. Purge on the happy path and a
	// reaper for the killed one are still both required; this closes only the gap
	// between them, where the container has stopped and nobody removed the corpse.
	AutoRemove bool

	// Mounts attaches host filesystem paths into the container.
	//
	// Each mount binds an absolute host Source to an absolute container Target.
	// ReadOnly should be set to true whenever the container does not need write
	// access, bounding the blast radius against host mutation. See Mount.
	Mounts []Mount
}

// Resource represents a container managed by this harness, including after stop
// or kill. Operations may run concurrently, but Purge must not race other calls.
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
// returned. It returns an empty string for outbound workers.
//
// It used to return 127.0.0.1 unconditionally, which is right only where the
// daemon shares the caller's network namespace. It does on a workstation and in a
// dind sidecar; it does not when dockerd runs on a VM host beside the caller's
// k3s pod, where loopback is refused and the bridge gateway answers. Deriving the
// answer needs a fact about the environment that is not available — "am I in
// Kubernetes?" is TRUE on both of one fleet's CI pools and only one of them needs
// a non-loopback address — so the address is measured rather than computed.
func (r *Resource) HostPort() string {
	if r == nil || r.resource == nil || r.port == "" {
		return ""
	}
	if r.hostPort != "" {
		return r.hostPort
	}
	return net.JoinHostPort(localhost, r.resource.GetPort(r.port+"/tcp"))
}

// StartContainer starts a TCP service or outbound worker and waits for readiness.
// It is StartContainerContext with a background context. The caller must Purge
// the Resource, even after stopping or killing it.
//
// A NON-Ephemeral container is a singleton keyed on its name: any pre-existing
// holder of that name is force-removed first, so two concurrent callers destroy
// each other's container. An Ephemeral one has a randomised name, so there is
// nothing to prune and nothing to collide with — the prune is skipped rather than
// issued as a round-trip that can only miss.
func StartContainer(cfg ContainerConfig) (*Resource, error) {
	return StartContainerContext(context.Background(), cfg)
}

// StartContainerContext uses ctx for Docker operations and cfg.Timeout for the
// readiness phase only, preserving StartContainer's TCP timeout semantics. Use a
// caller deadline to bound the whole startup; readiness callbacks must themselves
// be bounded (ReadyResource must honor its context). Failure cleanup uses a fresh
// bounded context, including on panic; an original panic is not swallowed. The
// returned resource's lifetime is not tied to ctx once startup succeeds.
func StartContainerContext(ctx context.Context, cfg ContainerConfig) (result *Resource, err error) {
	if err := validateReadiness(cfg); err != nil {
		return nil, fmt.Errorf("container %s: %w", cfg.Name, err)
	}
	if err := cfg.Limits.validate(); err != nil {
		return nil, fmt.Errorf("container %s: %w", cfg.Name, err)
	}
	if err := validateMounts(cfg.Mounts); err != nil {
		return nil, fmt.Errorf("container %s: %w", cfg.Name, err)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Ephemeral {
		cfg.Name = uniqueName(cfg.Name)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pool, err := initPool(ctx, cfg.Timeout)
	if err != nil {
		return nil, errors.Join(err, ctx.Err())
	}
	var r *Resource
	// Also runs during panic/Goexit. No recover is needed: the original panic
	// propagates after cleanup. Once registered, use exactly the Purge path so
	// failed readiness gets the same AutoRemove handling as explicit cleanup.
	defer func() {
		if result != nil {
			return
		}
		var cleanupErr error
		if r != nil {
			cleanupErr = r.Purge()
		} else {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
			defer cancel()
			cleanupErr = pool.Close(cleanupCtx)
		}
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("failed startup cleanup: %w", cleanupErr))
		}
	}()
	if !cfg.Ephemeral {
		if errRemove := removeExisting(ctx, pool, cfg.Name); errRemove != nil {
			return nil, errors.Join(errRemove, ctx.Err())
		}
	}

	// The dependency may fail between Docker creation and tracking the resource.
	// A private per-start label lets failure cleanup identify ONLY this attempt,
	// even if another caller has already replaced the reusable singleton name.
	cfg.Labels = maps.Clone(cfg.Labels)
	if cfg.Labels == nil {
		cfg.Labels = make(map[string]string)
	}
	cfg.Labels[startupLabel] = uniqueName("start")
	res, err := runContainer(ctx, pool, cfg)
	if err != nil {
		return nil, errors.Join(err, ctx.Err(), cleanupUntracked(pool, cfg, ctx.Err() != nil))
	}

	r = &Resource{pool: pool, resource: res, port: cfg.Port, name: cfg.Name}
	readyCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	if cfg.Port == "" {
		err = waitForResource(readyCtx, cfg, r)
	} else {
		err = waitForReady(readyCtx, pool, cfg, r)
	}
	if err == nil {
		err = readyCtx.Err()
	}
	if err != nil {
		return nil, errors.Join(err, readyCtx.Err())
	}
	return r, nil
}

// Purge stops/removes the container and closes its owned client. Safe on nil and
// repeated calls. Do not call concurrently with other resource operations.
func (r *Resource) Purge() error {
	if r == nil || r.pool == nil || r.resource == nil || r.pool.Client() == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	// Close the pool even if resource removal fails, so its owned client is not
	// leaked and tracked-resource cleanup gets another chance.
	err := r.resource.Close(ctx)
	// AutoRemove can race explicit removal: Docker returns conflict while the
	// daemon's removal is in progress. Confirm actual disappearance before
	// accepting that conflict; do not turn arbitrary removal errors into success.
	if errdefs.IsConflict(err) {
		if info := r.resource.Container(); info.HostConfig != nil && info.HostConfig.AutoRemove {
			if waitErr := waitRemoved(ctx, r.pool, r.ID()); waitErr == nil {
				err = r.resource.Close(ctx) // untrack the now-absent resource
			} else {
				err = errors.Join(err, waitErr)
			}
		}
	}
	if err := errors.Join(err, r.pool.Close(ctx)); err != nil {
		return fmt.Errorf("failed to purge container: %w", err)
	}
	return nil
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
	// An Ephemeral container asks the daemon to choose the host port. A fixed one
	// is a second shared resource beside the name: two concurrent callers collide
	// on it, and the loser either fails to start or is quietly given a different
	// port than it asked for.
	hostPort := cfg.Port
	if cfg.Ephemeral {
		hostPort = ""
	}
	bindings := network.PortMap{}
	if cfg.Port != "" {
		port, err := network.ParsePort(cfg.Port + "/tcp")
		if err != nil {
			return nil, fmt.Errorf("invalid port %q: %w", cfg.Port, err)
		}
		bindings[port] = []network.PortBinding{{
			HostIP:   netip.MustParseAddr("0.0.0.0"),
			HostPort: hostPort,
		}}
	}

	res, err := pool.Run(ctx, cfg.Repository,
		dockertest.WithTag(cfg.Tag),
		dockertest.WithName(cfg.Name),
		dockertest.WithCmd(cfg.Cmd),
		dockertest.WithEnv(cfg.Env),
		dockertest.WithUser(cfg.User),
		dockertest.WithContainerConfig(func(cc *container.Config) {
			if cfg.Entrypoint != nil {
				cc.Entrypoint = cfg.Entrypoint
			}
		}),
		dockertest.WithLabels(ownershipLabels(cfg.Labels)),
		dockertest.WithPortBindings(bindings),
		dockertest.WithHostConfig(func(hc *container.HostConfig) {
			cfg.Limits.apply(hc)
			applyMounts(hc, cfg.Mounts)
			hc.AutoRemove = cfg.AutoRemove
			if cfg.NetworkMode != "" {
				hc.NetworkMode = container.NetworkMode(cfg.NetworkMode)
			}
			if cfg.Port == "" {
				hc.PublishAllPorts = false
			}
		}),
		dockertest.WithoutReuse(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to run container %s: %w", cfg.Name, err)
	}
	return res, nil
}

// selectHost narrows the candidates to one, using a built-in TCP dial rather than
// the caller's probe. Racing lives here, on this side of the callback boundary:
// the caller's ReadyCheck may be stateful, and calling it once per candidate in
// parallel — then discarding all but one result — would be a silent contract change
// that breaks in the consumer's repository rather than in this one.
//
// A dial is a weak check on purpose. It answers "which of these addresses reaches
// the container" and nothing else; whether the thing behind it is the right service
// is the probe's question, asked once, afterwards.
//
// KNOWN TRADE, decided rather than overlooked. Selection and validation can
// disagree: an address that completes a handshake without being the container wins
// selection on every attempt, fails the probe every time, and starves a candidate
// that would have passed both. The obvious repair — drop an address once its probe
// fails — is WRONG here, and that is the interesting part: during startup the probe
// failing is the NORMAL case, so the first attempt would eliminate the correct
// candidate for not being ready yet. Probing every candidate serially instead would
// work, at the cost of calling the caller's probe more than once per attempt, which
// is the contract this design exists to keep simple. So the trade stands: the
// failure is loud (a timeout naming every candidate) rather than silent, and on any
// topology where two addresses reach the same daemon it cannot arise.
func selectHost(hosts []string, port string, timeout time.Duration) (string, error) {
	type result struct {
		addr string
		err  error
	}
	results := make(chan result, len(hosts))
	for _, host := range hosts {
		go func(host string) {
			addr := net.JoinHostPort(host, port)
			results <- result{addr, dialWithTimeout(addr, timeout)}
		}(host)
	}

	var lastErr error
	for range hosts {
		got := <-results
		if got.err == nil {
			return got.addr, nil
		}
		lastErr = got.err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no candidate addresses to try")
	}
	return "", lastErr
}

// waitForReady blocks until the container answers the caller's probe, and records
// the address it answered at.
//
// Each attempt: narrow the candidates with a cheap dial, then run the caller's
// probe ONCE against the winner. Racing the dials matters because an address that
// is unroutable-but-not-refused burns the whole timeout before the next is tried,
// and rootless Docker, Docker Desktop and a remote DOCKER_HOST all put the bridge
// gateway somewhere that blackholes.
func waitForReady(ctx context.Context, pool dockertest.ClosablePool, cfg ContainerConfig, r *Resource) error {
	const selectTimeout = time.Second

	probe := cfg.Ready
	if probe == nil {
		probe = tcpDialProbe
	}
	hosts := hostCandidates(ctx, pool)

	err := pool.Retry(ctx, cfg.Timeout, func() error {
		// Inside the retry, not above it. With Ephemeral the daemon assigns the host
		// port, so this depends on inspect data being populated; resolved once
		// outside, an empty value would make every subsequent attempt dial "host:"
		// and burn the whole timeout on a container that was healthy.
		port := r.resource.GetPort(r.port + "/tcp")
		if port == "" {
			return fmt.Errorf("container %s publishes no host port for %s yet", cfg.Name, r.port)
		}
		addr, err := selectHost(hosts, port, selectTimeout)
		if err != nil {
			return err
		}
		if err := probe(addr); err != nil {
			return err
		}
		r.hostPort = addr
		return nil
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

// hostCandidates returns the addresses worth trying for a published port. The
// order carries no preference — selection races them, so any address that answers
// is acceptable and on a workstation both do. Do not read this list as a ranking.
//
// The daemon is ASKED for its own bridge gateway rather than the environment being
// interrogated: a job pod on a separate CNI can route to that gateway even though
// loopback is refused. Loopback is always included: it is the right answer wherever
// the daemon and the caller share a namespace, and the only candidate available if
// the daemon reports no bridge gateway at all.
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
				//
				// Is4 because the port is published on 0.0.0.0. A dual-stack bridge
				// also reports a v6 gateway, and taking the first non-loopback entry
				// could pick an address nothing is listening on -- dead weight that
				// the race would hide rather than surface.
				if ipam.Gateway.IsValid() && !ipam.Gateway.IsLoopback() && ipam.Gateway.Is4() {
					hosts = append(hosts, ipam.Gateway.String())
					break
				}
			}
		}
	}
	return append(hosts, localhost)
}

// defaultDialTimeout bounds a single TCP connect attempt.
const defaultDialTimeout = time.Second

// dialWithTimeout is the one place a connect duration is decided, so a caller that
// passes a timeout gets the one it passed. The previous shape took a timeout and
// dialled with a hardcoded constant that happened to match — inert, harmless today,
// and precisely the trap that costs whoever later tunes the constant and observes
// nothing.
func dialWithTimeout(hostPort string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", hostPort, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// tcpDialProbe is the default readiness check: a successful TCP connect.
func tcpDialProbe(hostPort string) error {
	return dialWithTimeout(hostPort, defaultDialTimeout)
}
