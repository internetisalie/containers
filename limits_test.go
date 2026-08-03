package containers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
	"github.com/ory/dockertest/v4"
)

// The exponent is the only arithmetic in this package, and an integration test that
// asserts "a limit is set" cannot see it: 1.5 CPUs written as 1.5e6 is still a
// non-zero NanoCPUs and still inspects as a limit. Fractional on purpose — a whole
// number survives a truncating conversion that halves anything with a remainder.
func TestLimitsTranslateToDaemonUnits(t *testing.T) {
	var hc container.HostConfig
	Limits{MemoryBytes: 512 << 20, CPUs: 1.5}.apply(&hc)

	if want := int64(536870912); hc.Memory != want {
		t.Fatalf("Memory = %d, want %d", hc.Memory, want)
	}
	if want := int64(1_500_000_000); hc.NanoCPUs != want {
		t.Fatalf("NanoCPUs = %d, want %d (1.5 CPUs in nano-CPU units)", hc.NanoCPUs, want)
	}
}

// Zero must leave the field ALONE rather than write a zero. They are the same value
// to Docker today, and they are not the same statement: a future default set before
// apply runs would be silently overwritten by a caller who never asked for a limit.
func TestZeroLimitsDoNotTouchTheHostConfig(t *testing.T) {
	hc := container.HostConfig{Resources: container.Resources{Memory: 99, NanoCPUs: 98}}
	Limits{}.apply(&hc)

	if hc.Memory != 99 || hc.NanoCPUs != 98 {
		t.Fatalf("zero Limits overwrote an existing HostConfig: Memory=%d NanoCPUs=%d", hc.Memory, hc.NanoCPUs)
	}
}

// A negative limit is a caller's arithmetic gone wrong. The alternative to an error
// is a container running unbounded while its author believes it is capped, which is
// the failure mode this package exists to remove: present, wrong, and silent.
func TestNegativeLimitsAreRefused(t *testing.T) {
	if err := (Limits{MemoryBytes: -1}).validate(); err == nil {
		t.Fatal("a negative MemoryBytes must be refused, not treated as unbounded")
	}
	if err := (Limits{CPUs: -0.5}).validate(); err == nil {
		t.Fatal("a negative CPUs must be refused, not treated as unbounded")
	}
	if err := (Limits{MemoryBytes: 1 << 20, CPUs: 1}).validate(); err != nil {
		t.Fatalf("a valid limit must not be refused: %v", err)
	}
}

func TestStartContainerRefusesANegativeLimit(t *testing.T) {
	res, err := StartContainer(ContainerConfig{
		Name:       "containers-negative-limit",
		Repository: "nats",
		Tag:        "2.10-alpine",
		Port:       "4222",
		Ephemeral:  true,
		Limits:     Limits{MemoryBytes: -1},
		Ready:      natsGreeting,
	})
	if err == nil {
		_ = res.Purge()
		t.Fatal("a negative limit must fail before anything is started")
	}
	// Refused BEFORE the daemon is contacted, which is why this holds on a machine
	// with no Docker at all. Asserting the REASON matters: without it the test
	// passes on a workstation that simply has no daemon, and would keep passing if
	// validation were deleted entirely.
	if !strings.Contains(err.Error(), "negative") {
		t.Fatalf("want refusal by validation naming the negative limit, got: %v", err)
	}
}

// The WIRING, which is the failure worth a daemon: a correct HostConfig built and
// never passed to Run. Every assertion above stays green if the WithHostConfig
// option is deleted from runContainer; this one turns red.
func TestLimitsAndAutoRemoveReachTheDaemon(t *testing.T) {
	const memBytes = 512 << 20
	res, err := StartContainer(ContainerConfig{
		Name:       "containers-limits-probe",
		Repository: "nats",
		Tag:        "2.10-alpine",
		Port:       "4222",
		Ephemeral:  true,
		Limits:     Limits{MemoryBytes: memBytes, CPUs: 1.5},
		AutoRemove: true,
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

	if got := insp.Container.HostConfig.Memory; got != int64(memBytes) {
		t.Fatalf("daemon reports Memory = %d, want %d", got, int64(memBytes))
	}
	if got := insp.Container.HostConfig.NanoCPUs; got != 1_500_000_000 {
		t.Fatalf("daemon reports NanoCPUs = %d, want 1500000000", got)
	}
	if !insp.Container.HostConfig.AutoRemove {
		t.Fatal("daemon reports AutoRemove = false; the container will outlive its own exit")
	}
}
