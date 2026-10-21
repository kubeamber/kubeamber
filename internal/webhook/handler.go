// Package webhook serves KubeAmber's validating admission webhook.
//
// Admission webhooks must answer within seconds, so a held change cannot wait
// for a human inside the request. Instead KubeAmber denies it, records a
// ChangeRequest, and allows the identical change once a human has approved
// it: the agent retries, the fingerprints match, the approval is consumed.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubeamber/kubeamber/internal/changerequest"
	"github.com/kubeamber/kubeamber/pkg/decide"
	"github.com/kubeamber/kubeamber/pkg/fingerprint"
	"github.com/kubeamber/kubeamber/pkg/policy"
	"github.com/kubeamber/kubeamber/pkg/risk"
)

// Annotations an agent can set on the objects it changes.
const (
	// IntentAnnotation explains, in the agent's words, why it is making the change.
	IntentAnnotation = "kubeamber.dev/intent"
	// TraceParentAnnotation links the change to the agent's OpenTelemetry trace.
	TraceParentAnnotation = "kubeamber.dev/traceparent"
)

// maxClockSkew bounds how far an approval's timestamp may differ from the
// webhook's clock, so an approval cannot be post-dated to outlive its TTL.
const maxClockSkew = 2 * time.Minute

// replayWindow is how long after an approval is used the identical change is
// still allowed. The API server re-runs admission when it retries a write
// after a conflict (for example while a controller updates the object's
// status), so one kubectl command can reach the webhook twice. Re-applying
// an identical change has no further effect, so allowing it is safe.
const replayWindow = 30 * time.Second

// Store is the subset of changerequest.Store the handler uses.
type Store interface {
	FindByFingerprint(ctx context.Context, fp string) ([]*changerequest.ChangeRequest, error)
	Create(ctx context.Context, cr *changerequest.ChangeRequest) (*changerequest.ChangeRequest, error)
	SetPhase(ctx context.Context, cr *changerequest.ChangeRequest, phase changerequest.Phase, now time.Time) error
}

// Handler answers AdmissionReview requests.
type Handler struct {
	Engine *decide.Engine
	Store  Store
	Policy func() *policy.Policy
	// SelfUser is KubeAmber's own service account username.
	SelfUser string
	// BridgeUser is the service account username of `kubeamber bridge`, the
	// only identity that may link tickets and record decisions made in a
	// change-management system. Empty disables external decisions.
	BridgeUser string
	// Owners finds the person who launched an agent; optional.
	Owners interface {
		Owner(ctx context.Context, username string) string
	}
	Now func() time.Time
	Log *slog.Logger

	tracer    trace.Tracer
	decisions metric.Int64Counter
	scores    metric.Int64Histogram
}

const instrumentation = "github.com/kubeamber/kubeamber/internal/webhook"

// NewHandler wires telemetry instruments into a handler.
func NewHandler(h Handler) *Handler {
	if h.Now == nil {
		h.Now = time.Now
	}
	if h.Log == nil {
		h.Log = slog.Default()
	}
	h.tracer = otel.Tracer(instrumentation)
	meter := otel.Meter(instrumentation)
	h.decisions, _ = meter.Int64Counter("kubeamber.decisions",
		metric.WithDescription("Admission decisions for agent changes, by outcome."),
		metric.WithUnit("{decision}"))
	h.scores, _ = meter.Int64Histogram("kubeamber.risk.score",
		metric.WithDescription("Risk score of agent changes."),
		metric.WithExplicitBucketBoundaries(10, 20, 30, 40, 50, 60, 70, 80, 90, 100))
	return &h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&review); err != nil {
		http.Error(w, "decoding AdmissionReview: "+err.Error(), http.StatusBadRequest)
		return
	}
	if review.Request == nil {
		http.Error(w, "AdmissionReview has no request", http.StatusBadRequest)
		return
	}
	resp := h.Review(r.Context(), review.Request)
	resp.UID = review.Request.UID
	out := admissionv1.AdmissionReview{TypeMeta: review.TypeMeta, Response: resp}
	if out.APIVersion == "" {
		out.APIVersion, out.Kind = "admission.k8s.io/v1", "AdmissionReview"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// Review decides one admission request.
func (h *Handler) Review(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if req.Resource.Group == changerequest.GVR.Group && req.Resource.Resource == changerequest.GVR.Resource &&
		req.UserInfo.Username != h.SelfUser {
		return h.reviewChangeRequest(req)
	}
	return h.reviewChange(ctx, req)
}

func (h *Handler) reviewChange(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	p := h.Policy()
	if p.MatchAgent(req.UserInfo.Username, req.UserInfo.Groups) == nil {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	// Access reviews (kubectl auth can-i, kubectl auth whoami) change nothing.
	if g := req.Resource.Group; g == "authorization.k8s.io" || g == "authentication.k8s.io" {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	change, err := toChange(req)
	if err != nil {
		return deny(fmt.Sprintf("kubeamber: DENY: could not decode the request (%v)", err))
	}
	intent, traceparent := agentAnnotations(change)

	var opts []trace.SpanStartOption
	if link, ok := linkTo(traceparent); ok {
		opts = append(opts, trace.WithLinks(link))
	}
	ctx, span := h.tracer.Start(ctx, "kubeamber.review", opts...)
	defer span.End()

	d := h.Engine.Evaluate(ctx, decide.Request{
		User: req.UserInfo.Username, Groups: req.UserInfo.Groups, Change: change, Now: h.Now(),
	})
	outcome := string(d.Verdict)
	var resp *admissionv1.AdmissionResponse
	var crName string

	switch d.Verdict {
	case policy.Allow:
		resp = &admissionv1.AdmissionResponse{Allowed: true, Warnings: []string{decide.Message(d)}}
	case policy.Deny:
		resp = deny(decide.Message(d) + ". Do not retry this change; propose a safer alternative or ask a human to make it.")
	case policy.Hold:
		if req.DryRun != nil && *req.DryRun {
			resp = deny(decide.Message(d) + ". This was a dry run, so no change request was created.")
			break
		}
		resp, crName, outcome = h.hold(ctx, req, change, d, intent, traceparent)
	}

	resp.AuditAnnotations = map[string]string{
		"verdict": outcome,
		"score":   strconv.Itoa(d.Score),
		"agent":   d.Agent,
	}
	if crName != "" {
		resp.AuditAnnotations["change-request"] = crName
	}

	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.agent.name", d.Agent),
		attribute.String("kubeamber.outcome", outcome),
	}
	h.decisions.Add(ctx, 1, metric.WithAttributes(attrs...))
	h.scores.Record(ctx, int64(d.Score), metric.WithAttributes(attrs[0]))
	span.SetAttributes(append(attrs,
		attribute.String("kubeamber.verdict", string(d.Verdict)),
		attribute.Int("kubeamber.risk.score", d.Score),
		attribute.String("kubeamber.rule", d.Rule),
		attribute.String("kubeamber.change_request", crName),
		attribute.String("kubeamber.user", req.UserInfo.Username),
		attribute.String("kubeamber.operation", change.Operation),
		attribute.String("kubeamber.target.kind", change.Kind),
		attribute.String("kubeamber.target.name", change.Name),
		attribute.String("k8s.namespace.name", change.Namespace),
	)...)
	if !resp.Allowed {
		span.SetStatus(codes.Error, outcome)
	}
	h.Log.Info("agent change reviewed",
		"outcome", outcome, "agent", d.Agent, "user", req.UserInfo.Username,
		"operation", change.Operation, "target", change.Target(), "score", d.Score,
		"rule", d.Rule, "changeRequest", crName, "intent", intent, "reasons", d.Reasons)
	return resp
}

// hold finds or creates the ChangeRequest for a held change.
func (h *Handler) hold(ctx context.Context, req *admissionv1.AdmissionRequest, change risk.Change, d decide.Decision,
	intent, traceparent string) (*admissionv1.AdmissionResponse, string, string) {
	p := h.Policy()
	now := h.Now()
	fp := fingerprint.Of(req.UserInfo.Username, change)
	msg := decide.Message(d)

	existing, err := h.Store.FindByFingerprint(ctx, fp)
	if err != nil {
		return deny(fmt.Sprintf("%s. KubeAmber could not look up approvals (%v), so the change is held; retry shortly.", msg, err)), "", "hold"
	}
	for _, cr := range existing {
		ext := cr.Status.External
		switch cr.Effective(now, p.Spec.Approvals.TTL.Duration) {
		case changerequest.Approved:
			a := cr.Spec.Approval
			if a.Source != "" && ext != nil && ext.PlannedStart != nil && now.Before(ext.PlannedStart.Time) {
				return deny(fmt.Sprintf("%s. Approved in %s as %s, but its change window opens at %s. Retry this exact change then.",
					msg, a.Source, a.ExternalRef, ext.PlannedStart.UTC().Format(time.RFC3339))), cr.Name, "hold"
			}
			err := h.Store.SetPhase(ctx, cr, changerequest.Consumed, now)
			if errors.Is(err, changerequest.ErrAlreadyUsed) {
				continue
			}
			if err != nil {
				return deny(fmt.Sprintf("%s. Change request %s is approved but could not be marked as used (%v); retry.", msg, cr.Name, err)), cr.Name, "hold"
			}
			where := "change request " + cr.Name
			if a.ExternalRef != "" {
				where = fmt.Sprintf("%s %s, change request %s", a.Source, a.ExternalRef, cr.Name)
			}
			warn := fmt.Sprintf("kubeamber: ALLOW (approved by %s in %s): %s", a.By, where, a.Reason)
			return &admissionv1.AdmissionResponse{Allowed: true, Warnings: []string{warn}}, cr.Name, "approved"
		case changerequest.Consumed:
			if c := cr.Status.ConsumedAt; c != nil && now.Sub(c.Time) >= 0 && now.Sub(c.Time) <= replayWindow {
				a := cr.Spec.Approval
				warn := fmt.Sprintf("kubeamber: ALLOW (approved by %s in change request %s; the API server retried the write)", a.By, cr.Name)
				return &admissionv1.AdmissionResponse{Allowed: true, Warnings: []string{warn}}, cr.Name, "approved"
			}
		case changerequest.Pending:
			switch {
			case ext != nil:
				link := ""
				if ext.URL != "" {
					link = " (" + ext.URL + ")"
				}
				return deny(fmt.Sprintf("%s. Change request %s is waiting for approval in %s as %s%s, currently %s. Retry this exact change once it is approved.",
					msg, cr.Name, ext.System, ext.Number, link, ext.State)), cr.Name, "hold"
			case p.Spec.Approvals.RequireExternal:
				return deny(fmt.Sprintf("%s. Change request %s is waiting for a ticket to be opened in your change-management system. Retry this exact change once it is approved there.",
					msg, cr.Name)), cr.Name, "hold"
			}
			return deny(fmt.Sprintf("%s. Change request %s is still waiting for a human. Ask an operator to run `kubeamber approve %s`, then retry this exact change.",
				msg, cr.Name, cr.Name)), cr.Name, "hold"
		case changerequest.Rejected:
			if a := cr.Spec.Approval; a != nil && now.Before(a.At.Add(p.Spec.Approvals.PendingTTL.Duration)) {
				where := cr.Name
				if a.ExternalRef != "" {
					where = fmt.Sprintf("%s %s (change request %s)", a.Source, a.ExternalRef, cr.Name)
				}
				return deny(fmt.Sprintf("%s. %s rejected this change in %s: %q. Do not retry it.",
					msg, a.By, where, a.Reason)), cr.Name, "rejected"
			}
		}
	}

	cr, err := h.Store.Create(ctx, &changerequest.ChangeRequest{Spec: changerequest.Spec{
		Agent:     d.Agent,
		User:      req.UserInfo.Username,
		Owner:     h.ownerOf(ctx, req.UserInfo.Username),
		Operation: change.Operation,
		Target: changerequest.Target{
			Group: req.Resource.Group, Version: req.Resource.Version, Resource: req.Resource.Resource,
			SubResource: req.SubResource, Kind: change.Kind, Namespace: change.Namespace, Name: change.Name,
		},
		Score:       d.Score,
		Reasons:     d.Reasons,
		Factors:     factorLines(d.Factors),
		Summary:     d.Summary,
		Intent:      intent,
		TraceParent: traceparent,
		Fingerprint: fp,
		ExpiresAt:   metav1.NewTime(now.Add(p.Spec.Approvals.PendingTTL.Duration)),
	}})
	if err != nil {
		return deny(fmt.Sprintf("%s. KubeAmber could not record a change request (%v); retry shortly.", msg, err)), "", "hold"
	}
	if p.Spec.Approvals.RequireExternal {
		return deny(fmt.Sprintf("%s. Held as change request %s; a ticket is being opened in your change-management system. Retry this exact change once it is approved there.",
			msg, cr.Name)), cr.Name, "hold"
	}
	return deny(fmt.Sprintf("%s. Held for human approval as change request %s (expires in %s). Ask an operator to run `kubeamber approve %s`, then retry this exact change.",
		msg, cr.Name, p.Spec.Approvals.PendingTTL.Duration, cr.Name)), cr.Name, "hold"
}

// reviewChangeRequest enforces who may change a ChangeRequest, and how.
// Agents may not touch them at all. Humans may record their own decision on
// a pending request they did not raise. The change-management bridge may
// link one ticket and record the decision made on it.
func (h *Handler) reviewChangeRequest(req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	user := req.UserInfo.Username
	p := h.Policy()
	if p.MatchAgent(user, req.UserInfo.Groups) != nil {
		return deny("kubeamber: DENY: agents cannot create, approve, modify or delete change requests")
	}
	bridge := h.BridgeUser != "" && user == h.BridgeUser
	switch req.Operation {
	case admissionv1.Create:
		return deny("kubeamber: DENY: change requests are created by KubeAmber itself")
	case admissionv1.Delete, admissionv1.Connect:
		if bridge {
			return deny("kubeamber: DENY: the change-management bridge cannot delete change requests")
		}
		return &admissionv1.AdmissionResponse{Allowed: true}
	}

	oldCR, err := decodeCR(req.OldObject.Raw)
	if err != nil {
		return deny("kubeamber: DENY: " + err.Error())
	}
	newCR, err := decodeCR(req.Object.Raw)
	if err != nil {
		return deny("kubeamber: DENY: " + err.Error())
	}
	oldSpec, newSpec := oldCR.Spec, newCR.Spec
	oldSpec.Approval, newSpec.Approval = nil, nil
	if !reflect.DeepEqual(oldSpec, newSpec) {
		return deny("kubeamber: DENY: only spec.approval of a change request may be changed")
	}
	if !reflect.DeepEqual(oldCR.Status.ConsumedAt, newCR.Status.ConsumedAt) {
		return deny("kubeamber: DENY: status.consumedAt is set by KubeAmber when the change is applied")
	}
	if bridge {
		return h.reviewBridgeUpdate(req.Name, oldCR, newCR)
	}

	if !reflect.DeepEqual(oldCR.Status.External, newCR.Status.External) {
		return deny("kubeamber: DENY: status.external is maintained by the change-management bridge")
	}
	a := newCR.Spec.Approval
	if reflect.DeepEqual(oldCR.Spec.Approval, a) {
		if oldCR.Status.Phase != newCR.Status.Phase {
			return deny("kubeamber: DENY: status.phase changes only when a decision is recorded")
		}
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	if a != nil {
		switch {
		case a.Source != "" || a.ExternalRef != "":
			return deny("kubeamber: DENY: only the change-management bridge can record decisions made in an external system")
		case a.By != user:
			return deny(fmt.Sprintf("kubeamber: DENY: spec.approval.by must be your own identity (%s)", user))
		case a.Decision == changerequest.DecisionApproved && oldCR.Spec.Owner == user && !p.Spec.Approvals.AllowOwnerApproval:
			return deny(fmt.Sprintf("kubeamber: DENY: you launched this agent (%s), so someone else must approve its changes; you can still reject it",
				oldCR.Spec.User))
		case a.Decision == changerequest.DecisionApproved && p.Spec.Approvals.RequireExternal:
			where := "your change-management system"
			if e := oldCR.Status.External; e != nil {
				where = fmt.Sprintf("%s as %s", e.System, e.Number)
			}
			return deny(fmt.Sprintf("kubeamber: DENY: this cluster requires approvals to be recorded in %s; you can still reject it here", where))
		}
	}
	if reason := checkDecision(req.Name, oldCR, newCR, h.Now()); reason != "" {
		return deny("kubeamber: DENY: " + reason)
	}
	h.Log.Info("change request decided", "changeRequest", req.Name, "decision", a.Decision, "by", user,
		"agent", oldCR.Spec.Agent, "reason", a.Reason)
	return &admissionv1.AdmissionResponse{Allowed: true}
}

// reviewBridgeUpdate enforces what the change-management bridge may write:
// one immutable ticket link, its mirrored state, and the decision made on
// that ticket.
func (h *Handler) reviewBridgeUpdate(name string, oldCR, newCR *changerequest.ChangeRequest) *admissionv1.AdmissionResponse {
	oe, ne := oldCR.Status.External, newCR.Status.External
	switch {
	case oe != nil && ne == nil:
		return deny("kubeamber: DENY: a linked ticket cannot be removed")
	case ne != nil && (ne.System == "" || ne.ID == "" || ne.Number == ""):
		return deny("kubeamber: DENY: status.external needs system, id and number")
	case oe != nil && !oe.SameTicket(ne):
		return deny(fmt.Sprintf("kubeamber: DENY: %s is linked to %s %s; the link cannot be changed", name, oe.System, oe.Number))
	}
	a := newCR.Spec.Approval
	if reflect.DeepEqual(oldCR.Spec.Approval, a) {
		if oldCR.Status.Phase != newCR.Status.Phase {
			return deny("kubeamber: DENY: status.phase changes only when a decision is recorded")
		}
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	if ne == nil {
		return deny("kubeamber: DENY: an external decision needs a linked ticket")
	}
	if a != nil && (a.Source != ne.System || a.ExternalRef != ne.Number || !strings.HasPrefix(a.By, ne.System+":")) {
		return deny(fmt.Sprintf("kubeamber: DENY: an external decision must come from the linked ticket (%s %s) and name its approver as %s:<user>",
			ne.System, ne.Number, ne.System))
	}
	if reason := checkDecision(name, oldCR, newCR, h.Now()); reason != "" {
		return deny("kubeamber: DENY: " + reason)
	}
	h.Log.Info("change request decided externally", "changeRequest", name, "decision", a.Decision, "by", a.By,
		"ticket", ne.Number, "system", ne.System, "agent", oldCR.Spec.Agent)
	return &admissionv1.AdmissionResponse{Allowed: true}
}

// checkDecision validates a newly recorded decision, whoever records it, and
// returns why it is invalid or "".
func checkDecision(name string, oldCR, newCR *changerequest.ChangeRequest, now time.Time) string {
	a := newCR.Spec.Approval
	wantPhase := map[changerequest.Decision]changerequest.Phase{
		changerequest.DecisionApproved: changerequest.Approved,
		changerequest.DecisionRejected: changerequest.Rejected,
	}
	switch {
	case oldCR.Spec.Approval != nil:
		return fmt.Sprintf("%s was already decided by %s", name, oldCR.Spec.Approval.By)
	case a == nil:
		return "a recorded decision cannot be removed"
	case oldCR.Status.Phase != changerequest.Pending && oldCR.Status.Phase != "":
		return fmt.Sprintf("%s is %s, not Pending", name, oldCR.Status.Phase)
	case wantPhase[a.Decision] == "":
		return "spec.approval.decision must be Approved or Rejected"
	case a.By == oldCR.Spec.User:
		return "the identity that requested a change cannot approve it"
	case a.At.Sub(now).Abs() > maxClockSkew:
		return "spec.approval.at must be the current time"
	case a.Decision == changerequest.DecisionApproved && now.After(newCR.Deadline()):
		return fmt.Sprintf("%s expired at %s; the agent must retry to raise a new one",
			name, newCR.Deadline().UTC().Format(time.RFC3339))
	case newCR.Status.Phase != wantPhase[a.Decision]:
		return fmt.Sprintf("status.phase must be %s", wantPhase[a.Decision])
	}
	return ""
}

// ownerOf looks up who launched an agent, without holding up admission for
// long if the lookup is slow.
func (h *Handler) ownerOf(ctx context.Context, username string) string {
	if h.Owners == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return h.Owners.Owner(ctx, username)
}

func factorLines(fs []risk.Factor) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = fmt.Sprintf("%+d %s", f.Points, f.Reason)
	}
	return out
}

func decodeCR(raw []byte) (*changerequest.ChangeRequest, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("missing change request body")
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	return changerequest.FromUnstructured(u)
}

// toChange converts an admission request into the scorer's input.
func toChange(req *admissionv1.AdmissionRequest) (risk.Change, error) {
	group, kind := risk.ParentKind(req.Resource.Resource, req.SubResource, req.Kind.Group, req.Kind.Kind)
	c := risk.Change{
		Operation:   string(req.Operation),
		Group:       group,
		Kind:        kind,
		Resource:    req.Resource.Resource,
		SubResource: req.SubResource,
		Namespace:   req.Namespace,
		Name:        req.Name,
	}
	var err error
	if c.Object, err = decodeObject(req.Object.Raw); err != nil {
		return c, fmt.Errorf("object: %w", err)
	}
	if c.OldObject, err = decodeObject(req.OldObject.Raw); err != nil {
		return c, fmt.Errorf("oldObject: %w", err)
	}
	return c, nil
}

func decodeObject(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]any
	return m, json.Unmarshal(raw, &m)
}

func agentAnnotations(c risk.Change) (intent, traceparent string) {
	for _, obj := range []map[string]any{c.Object, c.OldObject} {
		meta, _ := obj["metadata"].(map[string]any)
		ann, _ := meta["annotations"].(map[string]any)
		if intent == "" {
			intent, _ = ann[IntentAnnotation].(string)
		}
		if traceparent == "" {
			traceparent, _ = ann[TraceParentAnnotation].(string)
		}
	}
	return intent, traceparent
}

// linkTo parses a W3C traceparent into a span link.
func linkTo(traceparent string) (trace.Link, bool) {
	if traceparent == "" {
		return trace.Link{}, false
	}
	ctx := propagation.TraceContext{}.Extract(context.Background(),
		propagation.MapCarrier{"traceparent": traceparent})
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return trace.Link{}, false
	}
	return trace.Link{SpanContext: sc, Attributes: []attribute.KeyValue{
		attribute.String("kubeamber.link", "agent-trace"),
	}}, true
}

func deny(msg string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusForbidden,
			Reason:  metav1.StatusReasonForbidden,
			Message: msg,
		},
	}
}
