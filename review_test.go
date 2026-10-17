package containers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

func TestCancelledCreateIsFoundAfterInitialNotFound(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	var labels map[string]string
	var lists int
	var removals []string
	releaseCreate, created := make(chan struct{}), make(chan struct{})
	var release sync.Once
	fakeDaemon(t, true, func(w http.ResponseWriter, req *http.Request, path string) bool {
		switch {
		case path == "/containers/create":
			var cfg container.Config
			if err := json.NewDecoder(req.Body).Decode(&cfg); err != nil {
				t.Error(err)
			}
			mu.Lock()
			labels = cfg.Labels
			mu.Unlock()
			cancel()
			// Finish creation only AFTER cleanup has seen NotFound and an
			// initial list without our container. No scheduling sleep needed.
			<-releaseCreate
			close(created)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"Id":"late-created-id"}`)
			return true
		case path == "/containers/json":
			mu.Lock()
			lists++
			n := lists
			token := labels[startupLabel]
			mu.Unlock()
			var filters mobyclient.Filters
			if err := json.Unmarshal([]byte(req.URL.Query().Get("filters")), &filters); err != nil {
				t.Error(err)
			}
			if req.URL.Query().Get("all") != "1" || len(filters["label"]) != 1 || !filters["label"][startupLabel+"="+token] || token == "" {
				t.Errorf("late cleanup did not filter all states by this start's label: %s", req.URL.RawQuery)
			}
			items := []map[string]any{{"Id": "foreign-id", "Labels": map[string]string{startupLabel: "foreign-attempt"}}}
			if n == 1 {
				release.Do(func() { close(releaseCreate) })
			} else {
				<-created
				items = append(items, map[string]any{"Id": "late-created-id", "Labels": map[string]string{startupLabel: token}, "State": "created"})
			}
			_ = json.NewEncoder(w).Encode(items)
			return true
		case strings.HasSuffix(path, "/json") && strings.HasPrefix(path, "/containers/"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"create has not completed"}`)
			return true
		case req.Method == http.MethodDelete:
			mu.Lock()
			removals = append(removals, path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		return false
	})
	// Runs before server.Close even if a mutation returns without polling.
	t.Cleanup(func() { release.Do(func() { close(releaseCreate) }) })
	cfg := workerConfig()
	cfg.AutoRemove = true // it cannot remove a container that never started
	r, err := StartContainerContext(ctx, cfg)
	if r != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled create = %v, %v", r, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if lists < 2 || len(removals) != 1 || removals[0] != "/containers/late-created-id" {
		t.Fatalf("late create leaked or foreign ID was removed: lists=%d removals=%v", lists, removals)
	}
}

func TestUncancelledCreateFailureDoesNotPollOrRemoveForeign(t *testing.T) {
	var mu sync.Mutex
	var lists, deletes int
	fakeDaemon(t, false, func(w http.ResponseWriter, req *http.Request, path string) bool {
		switch {
		case path == "/containers/create":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"fixture create denied"}`)
			return true
		case path == "/containers/json":
			mu.Lock()
			lists++
			mu.Unlock()
			fmt.Fprint(w, `[]`)
			return true
		case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
			fmt.Fprint(w, `{"Id":"foreign-id","Config":{"Labels":{"code.internetisalie.net/containers.startup-id":"foreign-attempt"}}}`)
			return true
		case req.Method == http.MethodDelete:
			mu.Lock()
			deletes++
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		return false
	})
	r, err := StartContainerContext(t.Context(), workerConfig())
	if r != nil || err == nil || !strings.Contains(err.Error(), "fixture create denied") {
		t.Fatalf("create failure was lost: %v, %v", r, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if lists != 0 || deletes != 0 {
		t.Fatalf("certain failure polled or deleted a foreign object: lists=%d deletes=%d", lists, deletes)
	}
}

func TestLateCreateLookupFailureIsReported(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fakeDaemon(t, false, func(w http.ResponseWriter, req *http.Request, path string) bool {
		switch {
		case path == "/containers/create":
			cancel()
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"create cancelled"}`)
			return true
		case path == "/containers/json":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"fixture lookup denied"}`)
			return true
		case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"not created yet"}`)
			return true
		}
		return false
	})
	r, err := StartContainerContext(ctx, workerConfig())
	if r != nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "fixture lookup denied") {
		t.Fatalf("uncertain-create lookup failure was hidden: %v, %v", r, err)
	}
}

// A real listening endpoint is enough for the library's initial TCP selection;
// the explicit Ready callback below is the application-level readiness check.
func fakeTCPPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

func fakeTCPInspect(w http.ResponseWriter, port string) {
	fmt.Fprintf(w, `{"Id":"fixture-container","State":{"Running":true},"NetworkSettings":{"Ports":{"4222/tcp":[{"HostIp":"127.0.0.1","HostPort":%q}]}}}`, port)
}

func TestReadinessTimeoutDoesNotIncludePullOrCreate(t *testing.T) {
	for _, mode := range []string{"tcp", "worker"} {
		for _, phase := range []string{"pull", "create"} {
			t.Run(mode+"/"+phase, func(t *testing.T) {
				port := fakeTCPPort(t)
				fakeDaemon(t, false, func(w http.ResponseWriter, req *http.Request, path string) bool {
					switch {
					case strings.HasPrefix(path, "/images/") && req.Method == http.MethodGet && phase == "pull":
						w.WriteHeader(http.StatusNotFound)
						fmt.Fprint(w, `{"message":"image not cached"}`)
						return true
					case path == "/images/create":
						time.Sleep(400 * time.Millisecond)
						fmt.Fprint(w, `{"status":"downloaded"}`)
						return true
					case path == "/containers/create" && phase == "create":
						time.Sleep(400 * time.Millisecond)
						w.WriteHeader(http.StatusCreated)
						fmt.Fprint(w, `{"Id":"fixture-container"}`)
						return true
					case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
						fakeTCPInspect(w, port)
						return true
					}
					return false
				})
				cfg := workerConfig()
				cfg.Timeout = 300 * time.Millisecond
				calls := 0
				if mode == "tcp" {
					cfg.Port = "4222"
					cfg.ReadyResource = nil
					cfg.Ready = func(string) error { calls++; return nil }
				} else {
					cfg.ReadyResource = func(ctx context.Context, _ *Resource) error {
						calls++
						deadline, ok := ctx.Deadline()
						if !ok || time.Until(deadline) < 200*time.Millisecond {
							t.Error("readiness did not receive its own timeout budget")
						}
						return nil
					}
				}
				r, err := StartContainerContext(t.Context(), cfg)
				if r != nil {
					t.Cleanup(func() {
						if err := r.Purge(); err != nil {
							t.Error(err)
						}
					})
				}
				if err != nil || r == nil || calls != 1 {
					t.Fatalf("slow %s consumed %s readiness timeout: resource=%v calls=%d err=%v", phase, mode, r, calls, err)
				}
			})
		}
	}
}

func TestFailedReadinessWaitsForAutoRemoval(t *testing.T) {
	var mu sync.Mutex
	var deletes, inspections int
	fakeDaemon(t, true, func(w http.ResponseWriter, req *http.Request, path string) bool {
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
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	probeErr := errors.New("fixture not ready")
	cfg := workerConfig()
	cfg.AutoRemove = true
	var resource *Resource
	cfg.ReadyResource = func(_ context.Context, r *Resource) error {
		resource = r
		cancel()
		return probeErr
	}
	r, err := StartContainerContext(ctx, cfg)
	if r != nil || !errors.Is(err, probeErr) || !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "failed startup cleanup") {
		t.Fatalf("failed readiness lost its cause or invented an AutoRemove failure: %v, %v", r, err)
	}
	if resource == nil || resource.pool.Client() != nil {
		t.Fatal("failed readiness did not close the owned client")
	}
	mu.Lock()
	defer mu.Unlock()
	if inspections != 2 || deletes != 2 {
		t.Fatalf("failed readiness did not observe/untrack removal: inspections=%d deletes=%d", inspections, deletes)
	}
}

func TestReadinessPanicCleansUpAndPropagates(t *testing.T) {
	for _, mode := range []string{"worker", "tcp"} {
		t.Run(mode, func(t *testing.T) {
			port := fakeTCPPort(t)
			var mu sync.Mutex
			var removals []string
			fakeDaemon(t, false, func(w http.ResponseWriter, req *http.Request, path string) bool {
				if req.Method == http.MethodDelete {
					mu.Lock()
					removals = append(removals, path)
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
					return true
				}
				if strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json") {
					fakeTCPInspect(w, port)
					return true
				}
				return false
			})
			cfg := workerConfig()
			panicValue := &struct{ message string }{"probe panic"}
			var observed *Resource
			cfg.ReadyResource = func(_ context.Context, r *Resource) error { observed = r; panic(panicValue) }
			if mode == "tcp" {
				cfg.Port, cfg.ReadyResource = "4222", nil
				cfg.Ready = func(string) error { panic(panicValue) }
			}
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				_, _ = StartContainerContext(t.Context(), cfg)
			}()
			if recovered != panicValue {
				t.Fatalf("readiness panic was swallowed/replaced: %v", recovered)
			}
			if mode == "worker" && (observed == nil || observed.pool.Client() != nil) {
				t.Fatal("panicking readiness leaked the owned client")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(removals) != 1 || removals[0] != "/containers/fixture-container" {
				t.Fatalf("panicking readiness leaked its container: %v", removals)
			}
		})
	}
}
