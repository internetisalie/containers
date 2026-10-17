package containers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Exercise deterministic Docker responses that cannot be reliably induced by a
// real daemon. Worker end-to-end tests separately prove actual process behavior.
func fakeDaemon(t *testing.T, autoRemove bool, handle func(http.ResponseWriter, *http.Request, string) bool) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.55")
		w.Header().Set("Content-Type", "application/json")
		if handle(w, req, path) {
			return
		}
		switch {
		case path == "/_ping":
			w.Header().Set("API-Version", "1.55")
		case strings.HasPrefix(path, "/images/"):
			fmt.Fprint(w, `{"Id":"fixture-image"}`)
		case path == "/containers/create":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"Id":"fixture-container"}`)
		case path == "/networks/bridge":
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(path, "/json"):
			fmt.Fprintf(w, `{"Id":"fixture-container","State":{"Running":true},"HostConfig":{"AutoRemove":%t}}`, autoRemove)
		case strings.HasSuffix(path, "/start"), strings.HasSuffix(path, "/stop"), req.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected daemon request: %s %s", req.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("DOCKER_HOST", server.URL)
	t.Setenv("DOCKER_API_VERSION", "1.55")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	return server.URL
}

func fakeWorker(t *testing.T, autoRemove bool, handle func(http.ResponseWriter, *http.Request, string) bool) *Resource {
	t.Helper()
	fakeDaemon(t, autoRemove, handle)
	cfg := workerConfig()
	cfg.AutoRemove = autoRemove
	cfg.ReadyResource = func(context.Context, *Resource) error { return nil }
	r, err := StartContainerContext(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Purge() })
	return r
}

func TestStopGraceReachesDockerInWholeSeconds(t *testing.T) {
	var mu sync.Mutex
	var timeouts []string
	r := fakeWorker(t, false, func(w http.ResponseWriter, req *http.Request, path string) bool {
		if !strings.HasSuffix(path, "/stop") {
			return false
		}
		mu.Lock()
		timeouts = append(timeouts, req.URL.Query().Get("t"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return true
	})
	for _, grace := range []time.Duration{0, time.Nanosecond, 1500 * time.Millisecond, 2 * time.Second} {
		if err := r.Stop(t.Context(), grace); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Stop(t.Context(), -time.Second); err == nil {
		t.Fatal("negative grace was accepted")
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(timeouts, []string{"0", "1", "2", "2"}) {
		t.Fatalf("Docker stop timeouts = %v, want [0 1 2 2]", timeouts)
	}
}

func TestPurgeWaitsForDaemonAutoRemoval(t *testing.T) {
	var mu sync.Mutex
	var deletes, inspections int
	r := fakeWorker(t, true, func(w http.ResponseWriter, req *http.Request, path string) bool {
		mu.Lock()
		defer mu.Unlock()
		if req.Method == http.MethodDelete {
			deletes++
			if inspections < 2 {
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, `{"message":"removal is already in progress"}`)
			} else {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"already removed"}`)
			}
			return true
		}
		if strings.HasSuffix(path, "/json") && deletes > 0 {
			inspections++
			if inspections < 2 {
				fmt.Fprint(w, `{"Id":"fixture-container","State":{"Status":"removing"}}`)
			} else {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"already removed"}`)
			}
			return true
		}
		return false
	})
	if err := r.Purge(); err != nil {
		t.Fatalf("AutoRemove conflict should wait for observed deletion: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if inspections != 2 || deletes != 2 {
		t.Fatalf("removal was not observed and untracked: inspections=%d deletes=%d", inspections, deletes)
	}
}

func TestPurgeDoesNotHideRemovalFailures(t *testing.T) {
	r := fakeWorker(t, true, func(w http.ResponseWriter, req *http.Request, path string) bool {
		if req.Method != http.MethodDelete {
			return false
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"fixture removal denied"}`)
		return true
	})
	if err := r.Purge(); err == nil || !strings.Contains(err.Error(), "fixture removal denied") {
		t.Fatalf("container removal failure was hidden: %v", err)
	}
	if r.pool.Client() != nil {
		t.Fatal("failed removal leaked the owned client")
	}
}

func TestWaitDoesNotTurnDaemonFailureIntoExitSuccess(t *testing.T) {
	r := fakeWorker(t, false, func(w http.ResponseWriter, req *http.Request, path string) bool {
		if !strings.HasSuffix(path, "/wait") {
			return false
		}
		fmt.Fprint(w, `{"StatusCode":0,"Error":{"Message":"fixture wait failed"}}`)
		return true
	})
	if _, err := r.Wait(t.Context()); err == nil || !strings.Contains(err.Error(), "fixture wait failed") {
		t.Fatalf("daemon wait failure was reported as success: %v", err)
	}
}
