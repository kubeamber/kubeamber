// Package changes connects held agent changes to a change-management system
// such as ServiceNow. The bridge opens a ticket for every pending
// ChangeRequest, records the decision made on it, and closes the ticket once
// the change has been applied or abandoned. See
// docs/proposals/0001-change-management.md.
package changes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeamber/kubeamber/internal/changerequest"
)

// Ticket is a change-management ticket as the bridge sees it.
type Ticket struct {
	// State is the ticket's state, as the system names it.
	State        string
	PlannedStart *time.Time
	PlannedEnd   *time.Time
	// Decision is empty until the ticket is approved or rejected.
	Decision changerequest.Decision
	// DecidedBy are the user names of the approvers (or rejecters).
	DecidedBy []string
	Reason    string
}

// Outcome is how a ticket is closed.
type Outcome string

const (
	// Implemented: the agent applied the approved change.
	Implemented Outcome = "implemented"
	// Abandoned: the change was never applied (expired, or rejected in the cluster).
	Abandoned Outcome = "abandoned"
)

// Provider talks to one change-management system.
type Provider interface {
	// Name is the system's name, e.g. "servicenow". It prefixes approver
	// identities and is recorded as the decision's source.
	Name() string
	// Open creates the ticket for a change request, or returns the one
	// already created for it. It must be idempotent for the same name.
	Open(ctx context.Context, cr *changerequest.ChangeRequest) (*changerequest.External, error)
	// Sync reads the ticket's current state and decision.
	Sync(ctx context.Context, ext changerequest.External) (Ticket, error)
	// Close records the outcome on the ticket.
	Close(ctx context.Context, ext changerequest.External, outcome Outcome, notes string) error
}

// Store is the subset of changerequest.Store the bridge uses.
type Store interface {
	List(ctx context.Context) ([]*changerequest.ChangeRequest, error)
	Update(ctx context.Context, cr *changerequest.ChangeRequest) error
}

// Bridge reconciles change requests with tickets.
type Bridge struct {
	Provider Provider
	Store    Store
	// ApprovalTTL is the policy's approvals.ttl, used to tell live approvals
	// from expired ones.
	ApprovalTTL time.Duration
	Now         func() time.Time
	Log         *slog.Logger
}

// Run reconciles every interval until ctx ends.
func (b *Bridge) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := b.ReconcileAll(ctx); err != nil {
			b.Log.Error("listing change requests", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ReconcileAll reconciles every change request once. Errors for one request
// are logged and recorded on it; they do not stop the others.
func (b *Bridge) ReconcileAll(ctx context.Context) error {
	crs, err := b.Store.List(ctx)
	if err != nil {
		return err
	}
	for _, cr := range crs {
		err := b.Reconcile(ctx, cr)
		switch {
		case err == nil:
		case apierrors.IsConflict(err):
			// Changed under us (an agent consumed it, a human decided it);
			// the next pass sees the new version.
		default:
			b.Log.Error("reconciling change request", "changeRequest", cr.Name, "error", err)
			b.recordError(ctx, cr, err)
		}
	}
	return nil
}

// Reconcile moves one change request and its ticket one step forward.
func (b *Bridge) Reconcile(ctx context.Context, cr *changerequest.ChangeRequest) error {
	now := b.now()
	ext := cr.Status.External
	phase := cr.Effective(now, b.ApprovalTTL)

	switch {
	case ext == nil && phase == changerequest.Pending:
		e, err := b.Provider.Open(ctx, cr)
		if err != nil {
			return fmt.Errorf("opening ticket: %w", err)
		}
		e.SyncedAt = timePtr(now)
		cr.Status.External = e
		b.Log.Info("ticket opened", "changeRequest", cr.Name, "system", e.System, "ticket", e.Number)
		return b.Store.Update(ctx, cr)

	case ext == nil || ext.Closed:
		return nil

	case phase == changerequest.Pending:
		t, err := b.Provider.Sync(ctx, *ext)
		if err != nil {
			return fmt.Errorf("syncing %s: %w", ext.Number, err)
		}
		before := *ext
		ext.State, ext.Error = t.State, ""
		// Mirror a rescheduled window, but never erase the one set at open.
		if t.PlannedStart != nil {
			ext.PlannedStart = metaTime(t.PlannedStart)
		}
		if t.PlannedEnd != nil {
			ext.PlannedEnd = metaTime(t.PlannedEnd)
		}
		if t.Decision != "" {
			by := strings.Join(t.DecidedBy, ",")
			if by == "" {
				by = "unrecorded"
			}
			reason := t.Reason
			if reason == "" {
				reason = fmt.Sprintf("%s %s in %s", strings.ToLower(string(t.Decision)), ext.Number, b.Provider.Name())
			}
			cr.Spec.Approval = &changerequest.Approval{
				Decision: t.Decision, By: b.Provider.Name() + ":" + by, Reason: reason, At: metav1.NewTime(now),
				Source: b.Provider.Name(), ExternalRef: ext.Number,
			}
			cr.Status.Phase = changerequest.Approved
			if t.Decision == changerequest.DecisionRejected {
				cr.Status.Phase = changerequest.Rejected
			}
			b.Log.Info("ticket decided", "changeRequest", cr.Name, "ticket", ext.Number, "decision", t.Decision, "by", by)
		} else if sameSync(before, *ext) {
			return nil // nothing changed; avoid a write every interval
		}
		ext.SyncedAt = timePtr(now)
		return b.Store.Update(ctx, cr)

	case phase == changerequest.Consumed:
		if err := b.Provider.Close(ctx, *ext, Implemented, implementedNotes(cr)); err != nil {
			return fmt.Errorf("closing %s: %w", ext.Number, err)
		}
		return b.markClosed(ctx, cr, now)

	case phase == changerequest.Expired:
		notes := fmt.Sprintf("KubeAmber change request %s expired before the agent applied the change. Nothing was changed.", cr.Name)
		if err := b.Provider.Close(ctx, *ext, Abandoned, notes); err != nil {
			return fmt.Errorf("cancelling %s: %w", ext.Number, err)
		}
		return b.markClosed(ctx, cr, now)

	case phase == changerequest.Rejected:
		// Rejected in the ticket: nothing to do there. Rejected in the
		// cluster: cancel the ticket so it does not wait for a CAB forever.
		if a := cr.Spec.Approval; a != nil && a.Source == "" {
			notes := fmt.Sprintf("Rejected in the cluster by %s: %s. Nothing was changed.", a.By, a.Reason)
			if err := b.Provider.Close(ctx, *ext, Abandoned, notes); err != nil {
				return fmt.Errorf("cancelling %s: %w", ext.Number, err)
			}
		}
		return b.markClosed(ctx, cr, now)
	}
	return nil
}

func (b *Bridge) markClosed(ctx context.Context, cr *changerequest.ChangeRequest, now time.Time) error {
	cr.Status.External.Closed = true
	cr.Status.External.Error = ""
	cr.Status.External.SyncedAt = timePtr(now)
	b.Log.Info("ticket closed", "changeRequest", cr.Name, "ticket", cr.Status.External.Number, "phase", cr.Status.Phase)
	return b.Store.Update(ctx, cr)
}

// recordError stores the last error on a linked change request so that
// `kubectl get changerequest -o yaml` shows why a ticket is stuck.
func (b *Bridge) recordError(ctx context.Context, cr *changerequest.ChangeRequest, err error) {
	if cr.Status.External == nil {
		return
	}
	msg := err.Error()
	if cr.Status.External.Error == msg {
		return
	}
	cr.Status.External.Error = msg
	if uerr := b.Store.Update(ctx, cr); uerr != nil && !apierrors.IsConflict(uerr) {
		b.Log.Error("recording bridge error", "changeRequest", cr.Name, "error", errors.Join(err, uerr))
	}
}

func implementedNotes(cr *changerequest.ChangeRequest) string {
	t := cr.Spec.Target
	target := t.Kind + " " + t.Name
	if t.Namespace != "" {
		target = t.Kind + " " + t.Namespace + "/" + t.Name
	}
	at := "unknown time"
	if cr.Status.ConsumedAt != nil {
		at = cr.Status.ConsumedAt.UTC().Format(time.RFC3339)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Applied by AI agent %s (%s) at %s: %s %s.", cr.Spec.Agent, cr.Spec.User, at, cr.Spec.Operation, target)
	if cr.Spec.Summary != "" {
		fmt.Fprintf(&b, " Change: %s.", cr.Spec.Summary)
	}
	fmt.Fprintf(&b, " Risk %d/100.", cr.Spec.Score)
	if a := cr.Spec.Approval; a != nil {
		where := "in the cluster"
		if a.Source != "" {
			where = "in " + a.Source + " " + a.ExternalRef
		}
		fmt.Fprintf(&b, " Approved by %s %s.", a.By, where)
	}
	if cr.Spec.Intent != "" {
		fmt.Fprintf(&b, " Agent's stated intent: %q.", cr.Spec.Intent)
	}
	fmt.Fprintf(&b, " KubeAmber change request %s, fingerprint %s.", cr.Name, cr.Spec.Fingerprint)
	if cr.Spec.TraceParent != "" {
		fmt.Fprintf(&b, " Trace: %s.", cr.Spec.TraceParent)
	}
	return b.String()
}

func (b *Bridge) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func sameSync(a, b changerequest.External) bool {
	return a.State == b.State && a.Error == b.Error && equalTime(a.PlannedStart, b.PlannedStart) && equalTime(a.PlannedEnd, b.PlannedEnd)
}

func equalTime(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(b)
}

func metaTime(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	m := metav1.NewTime(t.UTC().Truncate(time.Second))
	return &m
}

func timePtr(t time.Time) *metav1.Time {
	m := metav1.NewTime(t.UTC().Truncate(time.Second))
	return &m
}
