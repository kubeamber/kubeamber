// Package guards evaluates live signals that can escalate an agent's change,
// such as an SLO that is burning its error budget.
package guards

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kubeamber/kubeamber/pkg/decide"
	"github.com/kubeamber/kubeamber/pkg/policy"
)

// Prometheus runs PromQL checks. A check fires while its query returns at
// least one series. Results are cached briefly so the webhook stays fast.
type Prometheus struct {
	Client   *http.Client
	CacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	fired bool
	err   error
	at    time.Time
}

// NewPrometheus returns a guard with a 15s cache.
func NewPrometheus() *Prometheus {
	return &Prometheus{Client: &http.Client{}, CacheTTL: 15 * time.Second}
}

func (p *Prometheus) Evaluate(ctx context.Context, cfg *policy.PrometheusGuard, namespace string) []decide.GuardResult {
	var out []decide.GuardResult
	for _, c := range cfg.Checks {
		if len(c.Namespaces) > 0 && !policy.GlobAny(c.Namespaces, namespace) {
			continue
		}
		fired, err := p.query(ctx, cfg, c.Query)
		switch {
		case err != nil && cfg.OnError != policy.Allow:
			out = append(out, decide.GuardResult{Name: c.Name, Verdict: cfg.OnError,
				Reason: fmt.Sprintf("guard %q could not be evaluated (%v)", c.Name, err)})
		case fired:
			reason := c.Reason
			if reason == "" {
				reason = "its query returned results"
			}
			out = append(out, decide.GuardResult{Name: c.Name, Verdict: c.Action,
				Reason: fmt.Sprintf("guard %q fired: %s", c.Name, reason)})
		}
	}
	return out
}

func (p *Prometheus) query(ctx context.Context, cfg *policy.PrometheusGuard, q string) (bool, error) {
	key := cfg.URL + "\x00" + q
	p.mu.Lock()
	if c, ok := p.cache[key]; ok && time.Since(c.at) < p.CacheTTL {
		p.mu.Unlock()
		return c.fired, c.err
	}
	p.mu.Unlock()

	fired, err := p.fetch(ctx, cfg, q)

	p.mu.Lock()
	if p.cache == nil {
		p.cache = map[string]cached{}
	}
	p.cache[key] = cached{fired: fired, err: err, at: time.Now()}
	p.mu.Unlock()
	return fired, err
}

func (p *Prometheus) fetch(ctx context.Context, cfg *policy.PrometheusGuard, q string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout.Duration)
	defer cancel()
	u := strings.TrimRight(cfg.URL, "/") + "/api/v1/query?query=" + url.QueryEscape(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, fmt.Errorf("prometheus: decoding response: %w", err)
	}
	if body.Status != "success" {
		return false, fmt.Errorf("prometheus: %s", body.Error)
	}
	switch body.Data.ResultType {
	case "vector", "matrix":
		return len(body.Data.Result) > 0, nil
	}
	return false, fmt.Errorf("prometheus: query must return a vector, got %s", body.Data.ResultType)
}
