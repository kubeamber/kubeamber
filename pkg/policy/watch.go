package policy

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Current holds the policy in force and swaps it atomically on reload.
type Current struct {
	p atomic.Pointer[Policy]
}

// NewCurrent returns a holder initialised with p.
func NewCurrent(p *Policy) *Current {
	c := &Current{}
	c.p.Store(p)
	return c
}

// Get returns the policy in force.
func (c *Current) Get() *Policy { return c.p.Load() }

// Watch re-reads path every interval and installs the new policy when its
// content changes. An invalid file is logged and ignored, so a bad edit never
// replaces a working policy. Polling (rather than inotify) follows the
// symlink swap the kubelet uses to update mounted ConfigMaps.
func (c *Current) Watch(ctx context.Context, path string, interval time.Duration, log *slog.Logger, onChange func(*Policy)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p, err := Load(path)
		if err != nil {
			log.Error("policy reload failed; keeping the previous policy", "error", err)
			continue
		}
		if p.Checksum == c.Get().Checksum {
			continue
		}
		c.p.Store(p)
		log.Info("policy reloaded", "name", p.Metadata.Name, "agents", len(p.Spec.Agents), "rules", len(p.Spec.Rules))
		if onChange != nil {
			onChange(p)
		}
	}
}
