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
// Deliberately NOT PodUIDLabel, though it was when this test was written. That key
// is now stamped by the package itself and the environment's value wins, so a suite
// run inside a CI job pod — the one environment this all exists for — would see the
// caller's value replaced and go red for the right behaviour.
func TestLabelsReachTheDaemon(t *testing.T) {
	const uidKey = "code.internetisalie.net/containers.test-probe"
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

// The ownership label must land on the daemon for a caller that never asked for it
// and does not know it exists. That is the whole design: every consumer needs the
// label, none has a reason to think about it, and a label the caller must remember
// is the one that will be missing in whichever repository later leaks a container.
//
// Daemon-level rather than a unit test on ownershipLabels, because the failure this
// guards against is the wiring — the helper returning a correct map that nothing
// passes to Run. Reverting runContainer to WithLabels(cfg.Labels) turns this red and
// leaves every unit assertion below green.
func TestPodUIDLabelIsStampedWithoutTheCallerAskingForIt(t *testing.T) {
	const uid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	t.Setenv(podUIDEnv, uid)

	res, err := StartContainer(ContainerConfig{
		Name:       "containers-ownership-probe",
		Repository: "nats",
		Tag:        "2.10-alpine",
		Port:       "4222",
		Ephemeral:  true,
		// No Labels at all. That is the point.
		Timeout: 30 * time.Second,
		Ready:   natsGreeting,
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
	if got := insp.Container.Config.Labels[PodUIDLabel]; got != uid {
		t.Fatalf("label %s on the daemon = %q, want %q — this container is unreapable",
			PodUIDLabel, got, uid)
	}
}

// Off-cluster there is no pod, so there is nothing to own the container and nothing
// that would ever reap it. Stamping an empty value would be worse than stamping
// nothing: a reaper listing by label presence would match every workstation
// container it could see.
func TestNoPodUIDLabelOutsideAPod(t *testing.T) {
	t.Setenv(podUIDEnv, "")

	got := ownershipLabels(map[string]string{"keep": "me"})
	if _, ok := got[PodUIDLabel]; ok {
		t.Fatalf("stamped %s with no pod UID in the environment: %v", PodUIDLabel, got)
	}
	if got["keep"] != "me" {
		t.Fatalf("caller labels did not survive: %v", got)
	}
}

// The environment wins, which inverts the usual politeness on purpose. The value is
// an assertion about which pod is running this process: the environment's copy comes
// from the downward API and cannot be forged, the caller's is a guess. A caller who
// overrode it could only ever make its own container unreapable.
func TestCallerCannotOverrideThePodUID(t *testing.T) {
	const real = "real-pod-uid"
	t.Setenv(podUIDEnv, real)

	got := ownershipLabels(map[string]string{PodUIDLabel: "a-uid-the-caller-made-up"})
	if got[PodUIDLabel] != real {
		t.Fatalf("%s = %q, want the environment's %q — a caller can make its container "+
			"unreapable", PodUIDLabel, got[PodUIDLabel], real)
	}
}

// The caller's map belongs to the caller and may be shared across several
// StartContainer calls. Writing into it would leak one container's identity onto the
// next — which, for an ownership label, means reaping a live container.
func TestOwnershipLabelsDoNotMutateTheCallersMap(t *testing.T) {
	t.Setenv(podUIDEnv, "some-pod-uid")

	callers := map[string]string{"team": "sim"}
	_ = ownershipLabels(callers)

	if _, ok := callers[PodUIDLabel]; ok {
		t.Fatalf("the caller's map was written to: %v", callers)
	}
	if len(callers) != 1 {
		t.Fatalf("the caller's map changed size: %v", callers)
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
// happens to work here. This is the only assertion in the file that covers the
// reachability change at all: on a workstation loopback works, so every other test
// passes just as well against a hardcoded 127.0.0.1.
//
// The load-bearing check is `hostPort == ""` — revert the measurement and that goes
// red whichever candidate wins. The equality check below is tautological in healthy
// code and earns its place only against a mutation that stops HostPort reading what
// was measured. Do NOT "improve" it into an assertion about WHICH address is
// chosen: selection races the candidates, both answer here, and a test asserting a
// winner would be genuinely flaky.
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

// selectHost must dial for the duration it was GIVEN. The parameter was previously
// accepted and ignored — the dial used a hardcoded constant that happened to match,
// so nothing misbehaved and nothing could observe the promise was empty. This test
// is what makes it observable: 192.0.2.1 is RFC 5737 TEST-NET-1, which blackholes,
// so the call can only return when a timeout fires, and which timeout fired is
// visible in the wall clock.
func TestSelectHostHonoursItsTimeout(t *testing.T) {
	const short = 150 * time.Millisecond
	const blackhole = "192.0.2.1" // RFC 5737: reserved, routed nowhere

	start := time.Now()
	_, err := selectHost([]string{blackhole}, "4222", short)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a blackholed address must not be selected")
	}
	// LOWER bound first, and it is the more important of the two. The upper bound
	// only means anything if a timeout was actually exercised, and whether
	// 192.0.2.1 blackholes is a property of the HOST, not of RFC 5737: with no
	// default route, or behind a firewall that answers ICMP unreachable, the dial
	// fails in microseconds. Then elapsed sits far under the upper bound and this
	// test passes WITH THE PARAMETER IGNORED — reporting success it did not earn,
	// which is the exact shape of the skips that started this work.
	//
	// Skip rather than fail: a fast-failing host is not a defect in this code, it
	// simply cannot run this assertion. A CI job pod with restricted egress is
	// precisely such a host.
	if elapsed < short/2 {
		t.Skipf("%s failed fast (%s); no timeout was exercised, so this proves nothing", blackhole, elapsed)
	}
	// Generous bound: the assertion is "the short timeout was used", not "timing is
	// precise". The default is 1s, so an ignored parameter overshoots this by 2x.
	if elapsed > 3*short {
		t.Fatalf("selectHost took %s for a %s timeout — the parameter is not honoured", elapsed, short)
	}
}
