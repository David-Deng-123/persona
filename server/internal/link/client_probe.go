package link

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	probeOver  = 3 * time.Second
	probeEvery = 2 * time.Second
	healthPath = "/api/v1/health"
)

// probeRecovery signals only when the hub was unreachable at the start of this wait and answers a later probe.
// A hub that already answers keeps the original wait even if its WebSocket handshake failed.
func probeRecovery(ctx context.Context, reach func(context.Context) bool) <-chan struct{} {
	recovered := make(chan struct{})
	go func() {
		tick := time.NewTicker(probeEvery)
		defer tick.Stop()
		if ctx.Err() != nil || reach(ctx) {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if ctx.Err() != nil {
					return
				}
				if reach(ctx) {
					close(recovered)
					return
				}
			}
		}
	}()
	return recovered
}

// hubReachable races all eligible routes within one probe interval. Reachability is shared across routes: a hub
// answering through any of them is already reachable, and any of them can bring a hub back into reach.
func hubReachable(ctx context.Context, c *Config, sshBin string) bool {
	ctx, cancel := context.WithTimeout(ctx, probeEvery)
	defer cancel()
	p, proxiedEndpoints, _ := proxied(c)
	var probes []func() bool
	addEndpoints := func(eps []Endpoint, proxy *url.URL) {
		for _, ep := range eps {
			probes = append(probes, func() bool {
				return hubHealthy(ctx, hubClient(c, proxy, nil), ep.URL)
			})
		}
	}
	for _, kind := range routeOrder(c, p != nil, "") {
		switch kind {
		case KindSSH:
			probes = append(probes, func() bool { return sshHubHealthy(ctx, c, sshBin) })
		case KindProxy:
			addEndpoints(proxiedEndpoints, p)
		default:
			addEndpoints(ranked(c.Endpoints), nil)
		}
	}
	results := make(chan bool, len(probes))
	for _, probe := range probes {
		go func() { results <- probe() }()
	}
	for range probes {
		select {
		case <-ctx.Done():
			return false
		case healthy := <-results:
			if healthy {
				return true
			}
		}
	}
	return false
}

func sshHubHealthy(ctx context.Context, c *Config, sshBin string) bool {
	cfg, err := NormalizeSSH(c.SSH.Server, c.SSH.Target, c.SSH.Always, c.Endpoints)
	if err != nil || ctx.Err() != nil {
		return false
	}
	t, err := startSSHTunnel(sshBin, *cfg)
	if err != nil {
		return false
	}
	defer t.close()
	return hubHealthy(ctx, tunnelClient(c, t), tunnelBaseURL(c, *cfg))
}

// hubHealthy asks for health through the same TLS and routing checks used by the WebSocket. The health request
// carries no link credential. The client belongs to this probe, so closing idle connections also cancels any dial
// the transport kept alive after the request was canceled.
func hubHealthy(ctx context.Context, client *http.Client, base string) bool {
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+healthPath, nil)
	if err != nil || req.URL.Scheme != "https" || req.URL.Host == "" {
		return false
	}
	// net/http can derive Basic authentication from URL user info when it sends the request.
	req.URL.User = nil
	req.Header.Del("Authorization")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return false
	}
	var health struct {
		OK bool `json:"ok"`
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&health) == nil && health.OK
}
