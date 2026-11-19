package link

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
)

func runAtBackoffCap(t *testing.T, c Config, o Options) chan Status {
	t.Helper()
	o.Path = filepath.Join(t.TempDir(), "link.json")
	if err := Update(o.Path, func(saved *Config) error { *saved = c; return nil }); err != nil {
		t.Fatal(err)
	}
	statuses := make(chan Status, 32)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	o.PingEvery, o.PingTimeout = PingEvery, PingTimeout
	o.Logf = t.Logf
	go func() {
		defer close(done)
		runActive(ctx, o, &reporter{out: func(s Status) {
			select {
			case statuses <- s:
			case <-ctx.Done():
			}
		}}, backoff{n: 6})
	}()
	t.Cleanup(func() { cancel(); <-done })
	return statuses
}

func TestActiveLinkReconnectsWhenTheDirectHubComesBack(t *testing.T) {
	hub := newSSHTestHub(t, "")
	direct := newGatedDirectListener(t, hub.target, false)
	c := Config{Machine: "laptop", FP: hub.pin, Token: "jd_ssh", Endpoints: []Endpoint{{URL: direct.url, Kind: "lan"}}}
	statuses := runAtBackoffCap(t, c, Options{})
	waiting := waitFor(t, statuses, func(s Status) bool { return s.State == "waiting" })
	if left := time.Until(*waiting.Retry); left < 29*time.Second || left > time.Minute {
		t.Fatalf("first wait has %s left, want the jittered cap", left)
	}
	waitCondition(t, 5*time.Second, "a failed link dial, the baseline probe, and the first periodic probe", func() bool {
		return direct.attempts.Load() >= 3
	})
	back := time.Now()
	direct.allowed.Store(true)
	select {
	case <-hub.links:
	case <-time.After(5 * time.Second):
		t.Fatal("link did not reconnect within 5 seconds of direct recovery")
	}
	s := waitFor(t, statuses, func(s Status) bool { return s.State == "connected" })
	if s.Address != direct.url || !s.Since.Before(waiting.Retry.Add(-20*time.Second)) {
		t.Fatalf("connected at %s after %s over %s, initial retry was %s", s.Since, time.Since(back), s.Address, waiting.Retry)
	}
}

func TestActiveLinkOnlyProbesWhileTheHubStaysDown(t *testing.T) {
	var tries, probes atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath && strings.HasPrefix(r.UserAgent(), "Go-http-client/"):
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
				t.Error("health probe must be an unauthenticated GET")
			}
			probes.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.URL.Path == Path && r.Header.Get("Authorization") == "Bearer jd_test":
			tries.Add(1)
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	check := make(chan struct{}, 1)
	c := Config{Machine: "laptop", FP: pinOf(srv), Token: "jd_test", Endpoints: []Endpoint{{URL: srv.URL}}}
	statuses := runAtBackoffCap(t, c, Options{Check: check})
	waitFor(t, statuses, func(s Status) bool { return s.State == "waiting" })
	waitCondition(t, 5*time.Second, "baseline and periodic probes", func() bool { return probes.Load() >= 2 })
	if n := tries.Load(); n != 1 {
		t.Fatalf("got %d full attempts during one wait, want 1", n)
	}
	check <- struct{}{}
	waitFor(t, statuses, func(s Status) bool { return s.State == "waiting" })
	if n := tries.Load(); n != 2 {
		t.Fatalf("got %d full attempts after Check, want 2", n)
	}
}

func TestWaitKeepsItsDeadlineUnlessTheHubRecovers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		answer     func(n int) bool
		wait       time.Duration
		want       time.Duration
		wantProbes int
	}{
		{"down", func(int) bool { return false }, 31 * time.Second, 31 * time.Second, 16},
		{"healthy from the start", func(int) bool { return true }, 31 * time.Second, 31 * time.Second, 1},
		{"recovers on third probe", func(n int) bool { return n == 3 }, 31 * time.Second, 4 * time.Second, 3},
		{"short wait", func(int) bool { return true }, 3 * time.Second, 3 * time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var probes int
				o := Options{Logf: t.Logf}
				start := time.Now()
				o.waitWithProbe(t.Context(), tc.wait, time.Time{}, func(context.Context) bool {
					probes++
					return tc.answer(probes)
				})
				synctest.Wait()
				if took := time.Since(start); took != tc.want || probes != tc.wantProbes {
					t.Fatalf("waited %s with %d probes, want %s with %d probes", took, probes, tc.want, tc.wantProbes)
				}
			})
		})
	}
}

func TestActiveLinkKeepsSpecialWaits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		http  int
		close websocket.StatusCode
		state string
		wait  time.Duration
	}{
		{"401", http.StatusUnauthorized, 0, "refused", refusedWait},
		{"403", http.StatusForbidden, 0, "refused", refusedWait},
		{"4002", 0, CloseRevoked, "refused", refusedWait},
		{"4001", 0, CloseReplaced, "waiting", replaceWait},
		{"429", http.StatusTooManyRequests, 0, "waiting", 90 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := make(chan struct{}, 1)
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == healthPath && strings.HasPrefix(r.UserAgent(), "Go-http-client/") {
					select {
					case probe <- struct{}{}:
					default:
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if r.URL.Path != Path || r.Header.Get("Authorization") != "Bearer jd_test" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if tc.http != 0 {
					w.Header().Set("Retry-After", "90")
					w.WriteHeader(tc.http)
					return
				}
				if ws, err := websocket.Accept(w, r, nil); err == nil {
					defer ws.CloseNow()
					if _, _, err := ws.Read(r.Context()); err == nil {
						_ = ws.Close(tc.close, "closed")
					}
				}
			}))
			t.Cleanup(srv.Close)
			c := Config{Machine: "laptop", FP: pinOf(srv), Token: "jd_test", Endpoints: []Endpoint{{URL: srv.URL}}}
			statuses := runAtBackoffCap(t, c, Options{})
			s := waitFor(t, statuses, func(s Status) bool { return s.Retry != nil })
			left := time.Until(*s.Retry)
			if s.State != tc.state || left <= tc.wait-time.Second || left > tc.wait {
				t.Fatalf("state %s, wait %s, want %s and %s", s.State, left, tc.state, tc.wait)
			}
			select {
			case <-probe:
				t.Fatal("fixed wait started a health probe")
			case s := <-statuses:
				t.Fatalf("fixed wait ended early: %s", s.State)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

func TestCredentialRefusalInsideJoinedRouteErrors(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		err := errors.Join(&httpError{status: http.StatusBadGateway}, &httpError{status: code})
		if !errors.Is(err, errCredentialRefused) {
			t.Errorf("HTTP %d was hidden by another route's error", code)
		}
	}
}

func TestWaitCanStillWakeWhileAProbeIsRunning(t *testing.T) {
	for _, trigger := range []string{"context", "Check", "Redial", "link.json"} {
		t.Run(trigger, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "link.json")
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				check, redial := make(chan struct{}, 1), make(chan struct{}, 1)
				o := Options{Path: path, Check: check, Redial: redial}
				started, canceled, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
				start := time.Now()
				go func() {
					defer close(done)
					o.waitWithProbe(ctx, time.Minute, stampOf(path), func(ctx context.Context) bool {
						close(started)
						<-ctx.Done()
						close(canceled)
						return false
					})
				}()
				<-started
				var want time.Duration
				switch trigger {
				case "context":
					cancel()
				case "Check":
					check <- struct{}{}
				case "Redial":
					redial <- struct{}{}
				case "link.json":
					if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
						t.Fatal(err)
					}
					want = pairCheck
				}
				<-done
				<-canceled
				if took := time.Since(start); took != want {
					t.Fatalf("wait and probe ended after %s, want %s", took, want)
				}
			})
		})
	}
}

func TestHubReachableStripsEndpointCredentials(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != healthPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if len(r.Header.Values("Authorization")) != 0 {
			t.Error("health probe sent an Authorization header")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	endpoint := strings.Replace(srv.URL, "https://", "https://user:pass@", 1)
	c := &Config{FP: pinOf(srv), Proxy: ProxyOff, Endpoints: []Endpoint{{URL: endpoint}}}
	if !hubReachable(t.Context(), c, "") {
		t.Fatal("probe did not receive a successful unauthenticated health response")
	}
}

func TestHubReachableThroughProxy(t *testing.T) {
	hub := newSSHTestHub(t, "/hub")
	proxy := newConnectProxy(t, hub.target, nil)
	c := &Config{FP: hub.pin, Proxy: proxy.url, Endpoints: []Endpoint{{URL: unroutable + "/hub"}}}
	proxy.reset.Store(true) // CONNECT succeeds, but the hub's TLS handshake fails.
	if hubReachable(t.Context(), c, "") {
		t.Fatal("a proxy that resets the tunnel counted as reachable")
	}
	proxy.reset.Store(false)
	if !hubReachable(t.Context(), c, "") {
		t.Fatal("hub health did not answer through the proxy")
	}
	if len(proxy.asked()) < 2 {
		t.Fatal("health checks did not use the configured proxy")
	}
}

func TestHubReachableThroughSSH(t *testing.T) {
	ssh := newSSHHarness(t)
	hub := newSSHTestHub(t, "/hub")
	c := &Config{FP: hub.pin, Endpoints: []Endpoint{{URL: hub.srv.URL + "/hub"}},
		SSH: &SSH{Server: "hub.example.com", Target: hub.target, Always: true}}
	ssh.setMode("target_unreachable")
	if hubReachable(t.Context(), c, ssh.bin) {
		t.Fatal("an SSH listener without a working forward counted as reachable, or Always allowed a direct probe")
	}
	ssh.setMode("forward")
	if !hubReachable(t.Context(), c, ssh.bin) {
		t.Fatal("hub health did not answer through SSH")
	}
	c.FP = "different-pin"
	if hubReachable(t.Context(), c, ssh.bin) {
		t.Fatal("probe accepted a different hub key through SSH")
	}
}
