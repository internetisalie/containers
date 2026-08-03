package containers

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	mobyclient "github.com/moby/moby/client"
	"github.com/ory/dockertest/v4"
)

// The label has to reach the DAEMON, not just the config struct. A reaper reads
// containers from the daemon's view and nothing else, so a label that never lands
// there makes it a silent no-op — reaping nothing, indistinguishable from a clean
// fleet. This is the assertion that distinguishes those.
func TestLabelsReachTheDaemon(t *testing.T) {
	const uidKey = "code.internetisalie.net/containers.pod-uid"
	const uidVal = "11111111-2222-3333-4444-555555555555"

	res, err := StartContainer(ContainerConfig{
		Name:       "containers-label-probe",
		Repository: "nats",
		Tag:        "2.10-alpine",
		Port:       "4222",
		Ephemeral:  true,
		Labels:     map[string]string{uidKey: uidVal},
		Timeout:    30 * time.Second,
		Ready:      natsGreeting,
	})
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	defer func() { _ = res.Purge() }()

	pool, err := dockertest.NewPool(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	insp, err := pool.Client().ContainerInspect(context.Background(), res.Name(), mobyclient.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := insp.Container.Config.Labels[uidKey]; got != uidVal {
		t.Fatalf("label %s on the daemon = %q, want %q", uidKey, got, uidVal)
	}
}

// Ephemeral exists so two callers of the same fixture do not destroy each other.
// Without it the name is the identity and StartContainer force-removes the prior
// holder; this is the test that would go red if Ephemeral stopped working.
func TestEphemeralContainersCoexist(t *testing.T) {
	cfg := ContainerConfig{
		Name:       "containers-coexist-probe",
		Repository: "nats",
		Tag:        "2.10-alpine",
		Port:       "4222",
		Ephemeral:  true,
		Timeout:    30 * time.Second,
		Ready:      natsGreeting,
	}

	first, err := StartContainer(cfg)
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	defer func() { _ = first.Purge() }()

	second, err := StartContainer(cfg)
	if err != nil {
		t.Fatalf("a second Ephemeral container must coexist with the first: %v", err)
	}
	defer func() { _ = second.Purge() }()

	if first.Name() == second.Name() {
		t.Fatalf("both containers took the name %q; Ephemeral did not randomise it", first.Name())
	}
	if first.HostPort() == second.HostPort() {
		t.Fatalf("both containers took host address %q; the port was not daemon-assigned", first.HostPort())
	}
	// The first must still be serving: the point is that the second did not evict it.
	if err := natsGreeting(first.HostPort()); err != nil {
		t.Fatalf("the first container stopped serving once the second started: %v", err)
	}
}

// natsGreeting is a real identity probe: NATS announces itself with INFO. A bare
// TCP connect would accept anything listening on that address.
func natsGreeting(hostPort string) error {
	conn, err := net.DialTimeout("tcp", hostPort, time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 4)
	if _, err := conn.Read(buf); err != nil {
		return err
	}
	if !strings.HasPrefix(string(buf), "INFO") {
		return &net.AddrError{Err: "not a NATS server: " + string(buf), Addr: hostPort}
	}
	return nil
}

// HostPort must be the address the probe ACTUALLY answered at, not a default that
// happens to work here. This is the assertion that covers the reachability fix at
// all: on a workstation loopback works, so every other test in this file passes
// just as well against a hardcoded 127.0.0.1. Comparing the returned address to
// the one resolution recorded is what makes the difference observable without a
// second topology — the race prefers the bridge gateway, so a hardcoded loopback
// disagrees with it here even though both are reachable.
func TestHostPortIsTheAddressThatAnswered(t *testing.T) {
	res, err := StartContainer(ContainerConfig{
		Name:       "containers-hostport-probe",
		Repository: "nats",
		Tag:        "2.10-alpine",
		Port:       "4222",
		Ephemeral:  true,
		Timeout:    30 * time.Second,
		Ready:      natsGreeting,
	})
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	defer func() { _ = res.Purge() }()

	if res.hostPort == "" {
		t.Fatal("readiness resolved no address; HostPort would be falling back to a guess")
	}
	if got := res.HostPort(); got != res.hostPort {
		t.Fatalf("HostPort() = %q but readiness answered at %q — the address is being "+
			"derived rather than reported", got, res.hostPort)
	}
	if err := natsGreeting(res.HostPort()); err != nil {
		t.Fatalf("HostPort() %q does not serve NATS: %v", res.HostPort(), err)
	}
	t.Logf("resolved to %s", res.HostPort())
}
