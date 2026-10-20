// Package changerequest stores held changes as ChangeRequest custom resources
// (kubeamber.dev/v1alpha1). A ChangeRequest is created when an agent's change is
// held, approved or rejected by a human, and consumed when the agent retries
// the identical change.
package changerequest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var GVR = schema.GroupVersionResource{Group: "kubeamber.dev", Version: "v1alpha1", Resource: "changerequests"}

const (
	Kind       = "ChangeRequest"
	APIVersion = "kubeamber.dev/v1alpha1"

	// FingerprintLabel holds a prefix of the fingerprint so requests can be
	// found with a label selector (label values are limited to 63 chars).
	FingerprintLabel = "kubeamber.dev/fingerprint"
	AgentLabel       = "kubeamber.dev/agent"
)

type Phase string

const (
	Pending  Phase = "Pending"
	Approved Phase = "Approved"
	Rejected Phase = "Rejected"
	Consumed Phase = "Consumed"
	Expired  Phase = "Expired"
)

type Decision string

const (
	DecisionApproved Decision = "Approved"
	DecisionRejected Decision = "Rejected"
)

type ChangeRequest struct {
	Name              string      `json:"-"`
	ResourceVersion   string      `json:"-"`
	CreationTimestamp metav1.Time `json:"-"`
	Spec              Spec        `json:"spec"`
	Status            Status      `json:"status"`
}

type Spec struct {
	Agent       string      `json:"agent"`
	User        string      `json:"user"`
	Operation   string      `json:"operation"`
	Target      Target      `json:"target"`
	Score       int         `json:"score"`
	Reasons     []string    `json:"reasons,omitempty"`
	Summary     string      `json:"summary,omitempty"`
	Intent      string      `json:"intent,omitempty"`
	TraceParent string      `json:"traceparent,omitempty"`
	Fingerprint string      `json:"fingerprint"`
	ExpiresAt   metav1.Time `json:"expiresAt"`
	Approval    *Approval   `json:"approval,omitempty"`
}

type Target struct {
	Group       string `json:"group,omitempty"`
	Version     string `json:"version,omitempty"`
	Resource    string `json:"resource"`
	SubResource string `json:"subResource,omitempty"`
	Kind        string `json:"kind"`
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name,omitempty"`
}

type Approval struct {
	Decision Decision    `json:"decision"`
	By       string      `json:"by"`
	Reason   string      `json:"reason,omitempty"`
	At       metav1.Time `json:"at"`
}

type Status struct {
	Phase      Phase        `json:"phase"`
	ConsumedAt *metav1.Time `json:"consumedAt,omitempty"`
}

// Effective returns the phase taking time into account: a pending request
// past its expiry, or an approval older than approvalTTL, is Expired.
func (cr *ChangeRequest) Effective(now time.Time, approvalTTL time.Duration) Phase {
	switch cr.Status.Phase {
	case Pending, "":
		if now.After(cr.Spec.ExpiresAt.Time) {
			return Expired
		}
		return Pending
	case Approved:
		if cr.Spec.Approval == nil || now.After(cr.Spec.Approval.At.Add(approvalTTL)) {
			return Expired
		}
		return Approved
	}
	return cr.Status.Phase
}

// FromUnstructured decodes a ChangeRequest.
func FromUnstructured(u *unstructured.Unstructured) (*ChangeRequest, error) {
	data, err := json.Marshal(u.Object)
	if err != nil {
		return nil, err
	}
	var cr ChangeRequest
	if err := json.Unmarshal(data, &cr); err != nil {
		return nil, fmt.Errorf("decoding ChangeRequest %s: %w", u.GetName(), err)
	}
	cr.Name = u.GetName()
	cr.ResourceVersion = u.GetResourceVersion()
	cr.CreationTimestamp = u.GetCreationTimestamp()
	return &cr, nil
}

func (cr *ChangeRequest) toUnstructured() (*unstructured.Unstructured, error) {
	data, err := json.Marshal(cr)
	if err != nil {
		return nil, err
	}
	obj := map[string]any{}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion(APIVersion)
	u.SetKind(Kind)
	u.SetName(cr.Name)
	u.SetResourceVersion(cr.ResourceVersion)
	u.SetLabels(map[string]string{
		FingerprintLabel: labelValue(cr.Spec.Fingerprint),
		AgentLabel:       cr.Spec.Agent,
	})
	return u, nil
}

func labelValue(fp string) string { return fp[:min(40, len(fp))] }

// Store reads and writes ChangeRequests.
type Store struct {
	Client dynamic.Interface
}

func (s *Store) res() dynamic.ResourceInterface { return s.Client.Resource(GVR) }

// FindByFingerprint returns the change requests for a fingerprint, newest first.
func (s *Store) FindByFingerprint(ctx context.Context, fp string) ([]*ChangeRequest, error) {
	list, err := s.res().List(ctx, metav1.ListOptions{LabelSelector: FingerprintLabel + "=" + labelValue(fp)})
	if err != nil {
		return nil, err
	}
	var out []*ChangeRequest
	for i := range list.Items {
		cr, err := FromUnstructured(&list.Items[i])
		if err != nil {
			return nil, err
		}
		if cr.Spec.Fingerprint == fp {
			out = append(out, cr)
		}
	}
	sortNewestFirst(out)
	return out, nil
}

// List returns all change requests, newest first.
func (s *Store) List(ctx context.Context) ([]*ChangeRequest, error) {
	list, err := s.res().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]*ChangeRequest, 0, len(list.Items))
	for i := range list.Items {
		cr, err := FromUnstructured(&list.Items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, cr)
	}
	sortNewestFirst(out)
	return out, nil
}

func (s *Store) Get(ctx context.Context, name string) (*ChangeRequest, error) {
	u, err := s.res().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return FromUnstructured(u)
}

// Create stores a new pending change request. Its name is derived from the
// fingerprint plus a random suffix assigned by the API server.
func (s *Store) Create(ctx context.Context, cr *ChangeRequest) (*ChangeRequest, error) {
	cr.Status = Status{Phase: Pending}
	u, err := cr.toUnstructured()
	if err != nil {
		return nil, err
	}
	u.SetName("")
	u.SetGenerateName("cr-" + cr.Spec.Fingerprint[:min(8, len(cr.Spec.Fingerprint))] + "-")
	created, err := s.res().Create(ctx, u, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return FromUnstructured(created)
}

// ErrAlreadyUsed is returned when another request consumed the approval first.
var ErrAlreadyUsed = fmt.Errorf("approval was already used")

// SetPhase moves a change request to a new phase. It uses the resource
// version it was read at, so two concurrent retries cannot both consume one
// approval.
func (s *Store) SetPhase(ctx context.Context, cr *ChangeRequest, phase Phase, now time.Time) error {
	cr.Status.Phase = phase
	if phase == Consumed {
		t := metav1.NewTime(now)
		cr.Status.ConsumedAt = &t
	}
	u, err := cr.toUnstructured()
	if err != nil {
		return err
	}
	updated, err := s.res().Update(ctx, u, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return ErrAlreadyUsed
	}
	if err != nil {
		return err
	}
	cr.ResourceVersion = updated.GetResourceVersion()
	return nil
}

// Decide records a human decision. The admission webhook verifies that by
// is the identity making the request.
func (s *Store) Decide(ctx context.Context, name string, d Decision, by, reason string, now time.Time) error {
	phase := Approved
	if d == DecisionRejected {
		phase = Rejected
	}
	patch := map[string]any{
		"spec":   map[string]any{"approval": Approval{Decision: d, By: by, Reason: reason, At: metav1.NewTime(now)}},
		"status": map[string]any{"phase": phase},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = s.res().Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{})
	return err
}

func sortNewestFirst(crs []*ChangeRequest) {
	sort.SliceStable(crs, func(i, j int) bool {
		return crs[j].CreationTimestamp.Before(&crs[i].CreationTimestamp)
	})
}
