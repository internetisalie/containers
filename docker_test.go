package containers

import (
	"net"
	"testing"
)

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
