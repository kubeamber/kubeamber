package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/kubeamber/kubeamber/internal/changerequest"
)

// Store is the part of changerequest.Store the notifier reads.
type Store interface {
	List(ctx context.Context) ([]*changerequest.ChangeRequest, error)
}

// Notifier sends a message for each thing that happens to a held change.
// Delivery is at least once per channel: a message is marked sent only after
// the channel accepts it, and a failed channel is retried on the next pass
// without repeating the others.
type Notifier struct {
	Store    Store
	State    State
	Channels []Channel
	Routes   []Route
	// Cluster names this cluster in every message, for shared channels.
	Cluster string
	// RemindAfter sends one reminder for a request still pending this long
	// after it was raised. Zero sends none.
	RemindAfter time.Duration
	// ApprovalTTL returns the policy's approvals.ttl, to tell a live
	// approval from one that expired unused.
	ApprovalTTL func() time.Duration
	Now         func() time.Time
	Log         *slog.Logger
}

// Run notifies every interval until ctx ends.
func (n *Notifier) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := n.Notify(ctx); err != nil {
			n.Log.Error("notifying", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Notify sends what is due once.
func (n *Notifier) Notify(ctx context.Context) error {
	crs, err := n.Store.List(ctx)
	if err != nil {
		return err
	}
	sent, fresh, err := n.State.Load(ctx)
	if err != nil {
		return err
	}
	now, ttl := n.now(), n.ApprovalTTL()
	names := make([]string, len(n.Channels))
	byName := make(map[string]Channel, len(n.Channels))
	for i, c := range n.Channels {
		names[i], byName[c.Name()] = c.Name(), c
	}

	changed := fresh
	live := make(map[string]bool, len(crs))
	for _, cr := range crs {
		live[cr.Name] = true
		done := sent[cr.Name]
		if done == nil {
			done = map[string]bool{}
			sent[cr.Name] = done
		}
		// On the first start, don't replay history: requests already decided
		// are marked as told. Pending ones still need a person, so they go out.
		seed := fresh && cr.Effective(now, ttl) != changerequest.Pending
		justHeld := map[string]bool{}
		for _, e := range Due(cr, now, ttl, n.RemindAfter) {
			m := NewMessage(cr, e, n.Cluster)
			for _, name := range Targets(n.Routes, names, m) {
				key := string(e) + "/" + name
				if done[key] {
					continue
				}
				// A reminder right after the first message would only repeat it.
				if e == Reminder && justHeld[name] {
					done[key], changed = true, true
					continue
				}
				if !seed {
					if err := byName[name].Send(ctx, m); err != nil {
						n.Log.Error("sending notification", "changeRequest", cr.Name, "event", e, "channel", name, "error", err)
						continue
					}
					n.Log.Info("notification sent", "changeRequest", cr.Name, "event", e, "channel", name)
					if e == Held {
						justHeld[name] = true
					}
				}
				done[key], changed = true, true
			}
		}
	}
	for name := range sent {
		if !live[name] {
			delete(sent, name)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return n.State.Save(ctx, sent)
}

// Due lists the events a request has reached, as of now.
func Due(cr *changerequest.ChangeRequest, now time.Time, approvalTTL, remindAfter time.Duration) []Event {
	switch cr.Effective(now, approvalTTL) {
	case changerequest.Pending:
		if remindAfter > 0 && now.Sub(cr.CreationTimestamp.Time) >= remindAfter {
			return []Event{Held, Reminder}
		}
		return []Event{Held}
	case changerequest.Approved:
		return []Event{Approved}
	case changerequest.Rejected:
		return []Event{Rejected}
	case changerequest.Consumed:
		return []Event{Applied}
	case changerequest.Expired:
		return []Event{Expired}
	}
	return nil
}

func (n *Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}
