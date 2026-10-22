package webhook

import (
	"fmt"
	"strings"
	"unicode/utf8"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kubeamber/kubeamber/internal/changerequest"
	"github.com/kubeamber/kubeamber/pkg/decide"
	"github.com/kubeamber/kubeamber/pkg/risk"
)

// EventOutcomes are the outcomes recorded as Kubernetes Events unless
// configured otherwise. Allows are left out because routine changes would
// flood the event stream.
var EventOutcomes = []string{"hold", "deny", "approved", "rejected"}

// eventReasons maps each outcome to its Event reason.
var eventReasons = map[string]string{
	"allow":    "AgentChangeAllowed",
	"hold":     "AgentChangeHeld",
	"deny":     "AgentChangeDenied",
	"approved": "AgentChangeApproved",
	"rejected": "AgentChangeRejected",
}

// ParseEventOutcomes reads a comma-separated list of outcomes to record as
// Events. "all" means every outcome, and "none" or an empty value turns
// Events off.
func ParseEventOutcomes(s string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, o := range strings.Split(s, ",") {
		switch o = strings.TrimSpace(o); o {
		case "", "none":
		case "all":
			for k := range eventReasons {
				out[k] = true
			}
		default:
			if _, ok := eventReasons[o]; !ok {
				return nil, fmt.Errorf("unknown outcome %q (want allow, hold, deny, approved, rejected, all or none)", o)
			}
			out[o] = true
		}
	}
	return out, nil
}

// maxEventNote is below the 1 kB that events.k8s.io accepts for a note.
const maxEventNote = 1000

// recordEvent records a decision as a Kubernetes Event on the changed
// object, so people see it in `kubectl get events` and, for objects that
// already exist, in `kubectl describe`.
func (h *Handler) recordEvent(req *admissionv1.AdmissionRequest, change risk.Change, d decide.Decision, outcome, crName string, allowed bool) {
	if h.Events == nil || !h.EventOutcomes[outcome] {
		return
	}
	// The webhook declares no side effects on dry runs.
	if req.DryRun != nil && *req.DryRun {
		return
	}
	regarding := &corev1.ObjectReference{
		APIVersion: schema.GroupVersion{Group: req.Resource.Group, Version: req.Resource.Version}.String(),
		Kind:       change.Kind,
		Namespace:  change.Namespace,
		Name:       change.Name,
		UID:        objectUID(change),
	}
	var related runtime.Object
	if crName != "" {
		related = &corev1.ObjectReference{APIVersion: changerequest.APIVersion, Kind: changerequest.Kind, Name: crName}
	}
	eventType := corev1.EventTypeWarning
	if allowed {
		eventType = corev1.EventTypeNormal
	}
	h.Events.Eventf(regarding, related, eventType, eventReasons[outcome], change.Operation, "%s", eventNote(d, outcome, crName))
}

// eventNote says what happened, for the person reading the Event.
func eventNote(d decide.Decision, outcome, crName string) string {
	var note string
	switch outcome {
	case "hold":
		note = decide.Message(d)
		if crName != "" {
			note = fmt.Sprintf("Agent %s: change held as change request %s; approve with `kubeamber approve %s` or reject with `kubeamber reject %s`. %s",
				d.Agent, crName, crName, crName, note)
		}
	case "approved":
		note = fmt.Sprintf("Agent %s: change applied once, because change request %s was approved (risk %d/100).", d.Agent, crName, d.Score)
	case "rejected":
		note = fmt.Sprintf("Agent %s: change refused, because a person rejected change request %s (risk %d/100).", d.Agent, crName, d.Score)
	default:
		note = fmt.Sprintf("Agent %s: %s", d.Agent, decide.Message(d))
	}
	return truncate(note, maxEventNote)
}

// objectUID is the UID of the object being changed, when the request carries
// it. Events with a UID show up in `kubectl describe` for that object.
func objectUID(c risk.Change) types.UID {
	for _, obj := range []map[string]any{c.OldObject, c.Object} {
		if meta, ok := obj["metadata"].(map[string]any); ok {
			if uid, ok := meta["uid"].(string); ok && uid != "" {
				return types.UID(uid)
			}
		}
	}
	return ""
}

// truncate shortens s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n-len("…")]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}
