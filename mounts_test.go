package containers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	mobyclient "github.com/moby/moby/client"
	"github.com/ory/dockertest/v4"
)

// Translation of Mount to moby's mount.Mount must preserve all fields and
// specify TypeBind. Separated from the Docker daemon so translation correctness
// can be asserted in unit tests.
func TestMountsTranslateToHostConfig(t *testing.T) {
	var hc container.HostConfig
	mounts := []Mount{
		{
			Source:   "/host/data",
			Target:   "/container/data",
			ReadOnly: true,
		},
		{
			Source:   "/host/logs",
			Target:   "/container/logs",
			ReadOnly: false,
		},
	}
	applyMounts(&hc, mounts)

	if len(hc.Mounts) != 2 {
		t.Fatalf("hc.Mounts length = %d, want 2", len(hc.Mounts))
	}

	m0 := hc.Mounts[0]
	if m0.Type != mount.TypeBind {
		t.Errorf("Mounts[0].Type = %q, want %q", m0.Type, mount.TypeBind)
	}
	if m0.Source != "/host/data" {
		t.Errorf("Mounts[0].Source = %q, want /host/data", m0.Source)
	}
	if m0.Target != "/container/data" {
		t.Errorf("Mounts[0].Target = %q, want /container/data", m0.Target)
	}
	if !m0.ReadOnly {
		t.Errorf("Mounts[0].ReadOnly = false, want true")
	}

	m1 := hc.Mounts[1]
	if m1.Type != mount.TypeBind {
		t.Errorf("Mounts[1].Type = %q, want %q", m1.Type, mount.TypeBind)
	}
	if m1.Source != "/host/logs" {
		t.Errorf("Mounts[1].Source = %q, want /host/logs", m1.Source)
	}
	if m1.Target != "/container/logs" {
		t.Errorf("Mounts[1].Target = %q, want /container/logs", m1.Target)
	}
	if m1.ReadOnly {
		t.Errorf("Mounts[1].ReadOnly = true, want false")
	}
}

// An empty or nil Mounts slice must leave HostConfig.Mounts untouched rather
// than replacing an existing slice with an empty one.
func TestEmptyMountsDoNotTouchHostConfig(t *testing.T) {
	existing := []mount.Mount{{Type: mount.TypeBind, Source: "/a", Target: "/b"}}
	hc := container.HostConfig{Mounts: existing}

	applyMounts(&hc, nil)
	if len(hc.Mounts) != 1 || hc.Mounts[0].Source != "/a" {
		t.Fatalf("nil mounts modified HostConfig.Mounts: %v", hc.Mounts)
	}

	applyMounts(&hc, []Mount{})
	if len(hc.Mounts) != 1 || hc.Mounts[0].Source != "/a" {
		t.Fatalf("empty mounts modified HostConfig.Mounts: %v", hc.Mounts)
	}
}

// Malformed mounts must be rejected before reaching Docker.
func TestInvalidMountsAreRefused(t *testing.T) {
	cases := []struct {
		name    string
		mount   Mount
		wantErr string
	}{
		{
			name:    "empty source",
			mount:   Mount{Source: "", Target: "/container/path"},
			wantErr: "Source must not be empty",
		},
		{
			name:    "relative source",
			mount:   Mount{Source: "relative/path", Target: "/container/path"},
			wantErr: "must be an absolute path",
		},
		{
			name:    "empty target",
			mount:   Mount{Source: "/host/path", Target: ""},
			wantErr: "Target must not be empty",
		},
		{
			name:    "relative target",
			mount:   Mount{Source: "/host/path", Target: "relative/path"},
			wantErr: "must begin with /",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mount.validate()
			if err == nil {
				t.Fatalf("want error naming %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got error %v, want substring %q", err, tc.wantErr)
			}
		})
	}

	valid := Mount{Source: "/host/path", Target: "/container/path", ReadOnly: true}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid mount returned error: %v", err)
	}
}

// StartContainer must refuse an invalid mount before contacting the daemon.
func TestStartContainerRefusesInvalidMount(t *testing.T) {
	res, err := StartContainer(ContainerConfig{
		Name:       "containers-invalid-mount",
		Repository: "nats",
		Tag:        "2.10-alpine",
		Port:       "4222",
		Ephemeral:  true,
		Mounts: []Mount{
			{Source: "relative/path", Target: "/data"},
		},
		Ready: natsGreeting,
	})
	if err == nil {
		_ = res.Purge()
		t.Fatal("an invalid mount must fail before the daemon is contacted")
	}
	if !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("want refusal naming absolute path requirement, got: %v", err)
	}
}

// Mounts must reach the DAEMON through WithHostConfig and be reported in
// inspect. Every unit assertion above stays green if applyMounts is dropped
// from runContainer; this integration test turns red.
func TestMountsReachTheDaemon(t *testing.T) {
	tempDir := t.TempDir()
	testFile := filepath.Join(tempDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello from host"), 0o600); err != nil {
		t.Fatal(err)
	}

	const containerTarget = "/mnt/test-data"
	res, err := StartContainer(ContainerConfig{
		Name:       "containers-mounts-probe",
		Repository: "nats",
		Tag:        "2.10-alpine",
		Port:       "4222",
		Ephemeral:  true,
		Mounts: []Mount{
			{
				Source:   tempDir,
				Target:   containerTarget,
				ReadOnly: true,
			},
		},
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

	// 1. Verify HostConfig.Mounts recorded the configured mount.
	if len(insp.Container.HostConfig.Mounts) != 1 {
		t.Fatalf("daemon reports %d HostConfig.Mounts, want 1", len(insp.Container.HostConfig.Mounts))
	}
	hm := insp.Container.HostConfig.Mounts[0]
	if hm.Type != mount.TypeBind {
		t.Errorf("HostConfig.Mounts[0].Type = %q, want %q", hm.Type, mount.TypeBind)
	}
	if hm.Source != tempDir {
		t.Errorf("HostConfig.Mounts[0].Source = %q, want %q", hm.Source, tempDir)
	}
	if hm.Target != containerTarget {
		t.Errorf("HostConfig.Mounts[0].Target = %q, want %q", hm.Target, containerTarget)
	}
	if !hm.ReadOnly {
		t.Errorf("HostConfig.Mounts[0].ReadOnly = false, want true")
	}

	// 2. Verify Container.Mounts reflects the live mount point inside the container.
	var found bool
	for _, m := range insp.Container.Mounts {
		if m.Destination == containerTarget {
			found = true
			if m.Type != mount.TypeBind {
				t.Errorf("Mounts.Type = %q, want %q", m.Type, mount.TypeBind)
			}
			if m.RW {
				t.Errorf("Mounts.RW = true, want false for read-only mount")
			}
			break
		}
	}
	if !found {
		t.Fatalf("daemon reported no mount point at %s; mounts: %+v", containerTarget, insp.Container.Mounts)
	}
}
