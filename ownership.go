package containers

import (
	"maps"
	"os"
)

// PodUIDLabel carries the UID of the Kubernetes pod that started a container. It
// is what makes a leftover REAPABLE: the container is a host Docker object with no
// relationship to the pod that created it, so once that pod is gone the only thing
// tying the two together is this string.
//
// The key is namespaced by this package's import path rather than by any consumer,
// because the reaper is written once and the consumers are many. A reaper matching
// one spelling while a consumer stamps another reaps nothing — and reaping nothing
// is indistinguishable from a clean fleet, so the mistake would never surface.
const PodUIDLabel = "code.internetisalie.net/containers.pod-uid"

// podUIDEnv is set on CI job pods from the downward API (metadata.uid), which is
// what makes the value trustworthy: it is not copied from anywhere the workflow
// controls, and the runner injects it AFTER task env so a task variable of the same
// name cannot shadow it.
//
// It is empty on a workstation, which is the common case and not an error — there is
// no pod, so there is nothing to own the container and nothing to reap it.
const podUIDEnv = "RUNNER_POD_UID"

// ownershipLabels returns the labels to stamp: the caller's, plus the pod UID when
// this process is running inside a CI job pod.
//
// The stamping is AUTOMATIC and deliberately not a caller-supplied label. Every
// consumer of this package needs it and none has a reason to think about it, so a
// label the caller must remember is a label that will be missing in precisely the
// repository that later leaks a container — and the omission is invisible until an
// unrelated job is starved by it weeks later.
//
// The environment WINS over a caller-set value for the same key. That inverts the
// usual politeness on purpose: the value is an assertion about which pod is running
// this process, the environment's copy is unforgeable and the caller's is a guess.
// A caller who overrode it could only ever make the container unreapable.
//
// The caller's map is never mutated — it belongs to them and may be shared across
// several StartContainer calls.
func ownershipLabels(callerLabels map[string]string) map[string]string {
	uid := os.Getenv(podUIDEnv)
	if uid == "" {
		return callerLabels
	}
	out := make(map[string]string, len(callerLabels)+1)
	maps.Copy(out, callerLabels)
	out[PodUIDLabel] = uid
	return out
}
