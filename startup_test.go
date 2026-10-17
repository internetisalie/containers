package containers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

// Deterministically exercise the gap between Docker creation and dockertest
// registering the resource. Real-daemon readiness tests only exercise failures
// AFTER registration, and cannot prove this cancellation/cleanup boundary.
func TestStartupCancellationBeforeResourceRegistration(t *testing.T) {
	for _, mode := range []string{"start", "inspect", "foreign", "cleanup-error"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var mu sync.Mutex
			var labels map[string]string
			var removals []string
			var lookups int
			const id = "created-container-id"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				path := strings.TrimPrefix(req.URL.Path, "/v1.55")
				w.Header().Set("Content-Type", "application/json")
				switch {
				case path == "/_ping":
					w.Header().Set("API-Version", "1.55")
					w.WriteHeader(http.StatusOK)
				case strings.HasPrefix(path, "/images/"):
					fmt.Fprint(w, `{"Id":"fixture-image"}`)
				case path == "/containers/create":
					var cfg container.Config
					if err := json.NewDecoder(req.Body).Decode(&cfg); err != nil {
						t.Error(err)
					}
					labels = cfg.Labels
					w.WriteHeader(http.StatusCreated)
					fmt.Fprintf(w, `{"Id":%q}`, id)
				case path == "/containers/"+id+"/start":
					if mode == "inspect" {
						w.WriteHeader(http.StatusNoContent)
					} else {
						cancel()
						w.WriteHeader(http.StatusInternalServerError)
						fmt.Fprint(w, `{"message":"startup cancelled"}`)
					}
				case path == "/containers/"+id+"/json":
					cancel()
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, `{"message":"inspection cancelled"}`)
				case req.Method == http.MethodGet && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
					lookups++
					if mode == "foreign" {
						fmt.Fprint(w, `{"Id":"foreign-container-id","Config":{"Labels":{"code.internetisalie.net/containers.startup-id":"foreign-attempt"}}}`)
					} else {
						_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Config": map[string]any{"Labels": labels}})
					}
				case req.Method == http.MethodDelete:
					removals = append(removals, path)
					if mode == "cleanup-error" {
						w.WriteHeader(http.StatusInternalServerError)
						fmt.Fprint(w, `{"message":"fixture cleanup denied"}`)
					} else {
						w.WriteHeader(http.StatusNoContent)
					}
				default:
					t.Errorf("unexpected daemon request: %s %s", req.Method, path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			t.Setenv("DOCKER_HOST", server.URL)
			t.Setenv("DOCKER_API_VERSION", "1.55")
			t.Setenv("DOCKER_TLS_VERIFY", "")
			t.Setenv("DOCKER_CERT_PATH", "")
			cfg := workerConfig()
			cfg.Labels = map[string]string{startupLabel: "caller-cannot-reuse-token"}
			r, err := StartContainerContext(ctx, cfg)
			if r != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled startup = %v, %v", r, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if lookups != 1 || labels[startupLabel] == "" || labels[startupLabel] == "caller-cannot-reuse-token" {
				t.Fatalf("unregistered startup cleanup not exercised: lookups=%d labels=%v", lookups, labels)
			}
			if mode == "foreign" {
				if len(removals) != 0 {
					t.Fatalf("cleanup removed another start's container: %v", removals)
				}
			} else if len(removals) != 1 || removals[0] != "/containers/"+id {
				t.Fatalf("failed startup was not removed by immutable ID using a fresh context: %v", removals)
			}
			if mode == "cleanup-error" && !strings.Contains(err.Error(), "fixture cleanup denied") {
				t.Fatalf("cleanup failure was hidden: %v", err)
			}
			if cfg.Labels[startupLabel] != "caller-cannot-reuse-token" {
				t.Fatal("caller's label map was modified")
			}
		})
	}
}

func TestAlreadyCancelledStartupDoesNotPruneSingleton(t *testing.T) {
	// No actual daemon access is needed: an already cancelled caller must not
	// prune the singleton name, inspect images or create a pool.
	t.Setenv("DOCKER_HOST", "unsupported://must-not-connect")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cfg := workerConfig()
	cfg.Ephemeral = false
	cfg.Timeout = time.Second
	r, err := StartContainerContext(ctx, cfg)
	if r != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("already cancelled startup did not fail before daemon access: %v, %v", r, err)
	}
}
