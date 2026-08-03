package containers

import (
	"fmt"

	"github.com/moby/moby/api/types/container"
)

// Limits bound what a container may consume on the host it shares.
//
// A test container is a HOST Docker object. Where dockerd runs beside a
// Kubernetes node rather than inside it — which is how this fleet's CI is built —
// the scheduler cannot see these containers at all: they are uncounted against pod
// limits and node allocatable, so the kubelet schedules into memory Docker is
// already holding and OOM-kills something innocent. An unbounded test container is
// therefore not merely untidy; it is a fault injected into unrelated workloads.
//
// The failure that motivated this is worth stating because it is not the obvious
// one. CockroachDB sizes its cache as a fraction of HOST RAM and reads the host
// rather than its own cgroup, so inside a 1 GiB container it sized itself for a
// 20-core machine, came up, served a query, and was killed. Bounding the process
// from the inside (--cache, --max-sql-memory) fixes that ONE image; bounding the
// container from the outside fixes every image, including the ones whose sizing
// heuristics nobody has read yet.
//
// Zero means unbounded, which is Docker's default and this package's — a limit
// nobody asked for is worse than none, because the container that dies at a
// threshold you did not choose looks like a flake.
type Limits struct {
	// MemoryBytes caps resident memory. The container is OOM-killed at this
	// figure rather than the host running out.
	MemoryBytes int64
	// CPUs caps CPU time, in whole and fractional cores: 1.5 means one and a half
	// cores' worth. Translated to the daemon's nano-CPU units.
	CPUs float64
}

// nanoCPUs converts to the daemon's unit. Kept here rather than inline so the
// magic constant has one home and a name.
const nanoCPUsPerCPU = 1e9

// apply writes the limits onto a HostConfig.
//
// Separated from the RunOption that carries it so the translation can be asserted
// without a daemon: CPUs is the one field with arithmetic in it, and a wrong
// exponent is invisible in an integration test that only checks "a limit is set".
func (l Limits) apply(hc *container.HostConfig) {
	if l.MemoryBytes > 0 {
		hc.Memory = l.MemoryBytes
	}
	if l.CPUs > 0 {
		hc.NanoCPUs = int64(l.CPUs * nanoCPUsPerCPU)
	}
}

// validate rejects negatives rather than silently ignoring them.
//
// A negative limit is always a caller's arithmetic gone wrong — a subtraction that
// underflowed, a config parsed as the wrong unit — and the alternative to an error
// is a container that runs unbounded while its author believes it is capped. That
// is the shape of every defect this package exists to prevent: a protection that
// is present, wrong, and silent.
func (l Limits) validate() error {
	if l.MemoryBytes < 0 {
		return fmt.Errorf("MemoryBytes is negative (%d); a limit that cannot be met is a caller error, not an unbounded container", l.MemoryBytes)
	}
	if l.CPUs < 0 {
		return fmt.Errorf("CPUs is negative (%v); a limit that cannot be met is a caller error, not an unbounded container", l.CPUs)
	}
	return nil
}
