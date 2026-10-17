package containers

import (
	"net"
	"testing"
	"time"
)

// NATS exposes 4222 in the image, but the caller may choose another TCP port.
// Inspect metadata as well as reaching it: newer daemons tolerate a missing
// ExposedPorts entry, so a successful local connection alone masks the CI defect.
func TestTCPPortOutsideImageMetadata(t *testing.T) {
	observer(t)
	r := startWorker(t, ContainerConfig{
		Name: "containers-nondefault-port", Repository: "nats", Tag: "2.10-alpine",
		Port: "4423", Cmd: []string{"-p", "4423"}, Ephemeral: true,
		Ready: natsGreeting, Timeout: 10 * time.Second,
		Limits: Limits{MemoryBytes: 64 << 20, CPUs: 0.5},
	})
	info, err := r.Inspect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for port := range info.Config.ExposedPorts {
		if port.String() == "4423/tcp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("configured port absent from create metadata: %v", info.Config.ExposedPorts)
	}
	if r.HostPort() == "" {
		t.Fatal("configured TCP port was not reachable")
	}
}

// TestTCPDialProbe verifies the default readiness probe succeeds against a live
// listener and fails against a closed port — no Docker daemon required.
func TestTCPDialProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	if err := tcpDialProbe(ln.Addr().String()); err != nil {
		t.Errorf("probe against open listener: got %v, want nil", err)
	}

	_ = ln.Close()
	if err := tcpDialProbe(ln.Addr().String()); err == nil {
		t.Error("probe against closed listener: got nil, want error")
	}
}

// TestConfigDefaultTimeout documents that a zero Timeout falls back to DefaultTimeout.
func TestConfigDefaultTimeout(t *testing.T) {
	if DefaultTimeout <= 0 {
		t.Fatalf("DefaultTimeout must be positive, got %v", DefaultTimeout)
	}
}
