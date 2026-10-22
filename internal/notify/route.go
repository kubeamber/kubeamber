package notify

import (
	"fmt"
	"slices"

	"github.com/kubeamber/kubeamber/pkg/policy"
)

// Route sends matching messages to channels. Empty fields match everything.
type Route struct {
	// Channels are the names of the channels to send to.
	Channels []string `json:"channels"`
	// Events limits the route to these events.
	Events []Event `json:"events,omitempty"`
	// MinRisk limits the route to changes scored at least this high.
	MinRisk int `json:"minRisk,omitempty"`
	// Agents and Namespaces limit the route to these agent profiles and
	// target namespaces (globs such as "prod-*").
	Agents     []string `json:"agents,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`
}

// Matches reports whether the route takes m.
func (r Route) Matches(m Message) bool {
	if len(r.Events) > 0 && !slices.Contains(r.Events, m.Event) {
		return false
	}
	if m.Risk < r.MinRisk {
		return false
	}
	if len(r.Agents) > 0 && !policy.GlobAny(r.Agents, m.Agent) {
		return false
	}
	if len(r.Namespaces) > 0 && !policy.GlobAny(r.Namespaces, m.Namespace) {
		return false
	}
	return true
}

// Targets returns the names of the channels m goes to, each once, in the
// order the routes name them. With no routes, every channel gets everything.
func Targets(routes []Route, channels []string, m Message) []string {
	if len(routes) == 0 {
		return channels
	}
	var out []string
	for _, r := range routes {
		if !r.Matches(m) {
			continue
		}
		for _, c := range r.Channels {
			if !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
	}
	return out
}

// ValidateRoutes checks routes against the configured channel names.
func ValidateRoutes(routes []Route, channels []string) error {
	for i, r := range routes {
		if len(r.Channels) == 0 {
			return fmt.Errorf("routes[%d]: channels is required", i)
		}
		for _, c := range r.Channels {
			if !slices.Contains(channels, c) {
				return fmt.Errorf("routes[%d]: unknown channel %q", i, c)
			}
		}
		for _, e := range r.Events {
			if !slices.Contains(Events, e) {
				return fmt.Errorf("routes[%d]: unknown event %q (want one of %v)", i, e, Events)
			}
		}
		if r.MinRisk < 0 || r.MinRisk > 100 {
			return fmt.Errorf("routes[%d]: minRisk must be between 0 and 100", i)
		}
	}
	return nil
}
