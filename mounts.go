package containers

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
)

// Mount defines a host-to-container filesystem bind mount.
//
// A test container is a host Docker object. Integration tests frequently need to
// project fixtures, configuration files, or simulated secret volumes into the
// container filesystem before it boots.
//
// Source is the absolute path on the host.
// Target is the absolute path inside the container.
// ReadOnly should be set to true whenever the container does not need to mutate
// the host path, bounding the test container's blast radius against the host.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// toMoby converts a Mount to moby's mount.Mount representation.
func (m Mount) toMoby() mount.Mount {
	return mount.Mount{
		Type:     mount.TypeBind,
		Source:   m.Source,
		Target:   m.Target,
		ReadOnly: m.ReadOnly,
	}
}

// validate rejects malformed mounts before contacting the Docker daemon.
//
// An empty or relative source or target is always a caller defect. Failing
// early with an explicit error prevents starting an unconfigured container or
// relying on daemon-specific error messages that vary across Docker versions.
func (m Mount) validate() error {
	if m.Source == "" {
		return fmt.Errorf("Source must not be empty")
	}
	if !filepath.IsAbs(m.Source) {
		return fmt.Errorf("Source %q must be an absolute path; bind mounts require absolute host paths", m.Source)
	}
	if m.Target == "" {
		return fmt.Errorf("Target must not be empty")
	}
	if !strings.HasPrefix(m.Target, "/") {
		return fmt.Errorf("Target %q must be an absolute path; container paths must begin with /", m.Target)
	}
	return nil
}

// validateMounts validates each mount in the slice, reporting index on error.
func validateMounts(mounts []Mount) error {
	for i, m := range mounts {
		if err := m.validate(); err != nil {
			return fmt.Errorf("mount[%d]: %w", i, err)
		}
	}
	return nil
}

// applyMounts writes mounts onto a HostConfig.
//
// Separated from the RunOption that carries it so translation can be asserted
// in unit tests without requiring a Docker daemon.
func applyMounts(hc *container.HostConfig, mounts []Mount) {
	if hc == nil || len(mounts) == 0 {
		return
	}
	if hc.Mounts == nil {
		hc.Mounts = make([]mount.Mount, 0, len(mounts))
	}
	for _, m := range mounts {
		hc.Mounts = append(hc.Mounts, m.toMoby())
	}
}
