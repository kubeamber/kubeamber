// Package notify tells people about held agent changes and what became of
// them, in Slack, Microsoft Teams or any HTTP endpoint. It watches
// ChangeRequests rather than sitting in the admission path, and keeps what it
// has sent in a ConfigMap, so a restart does not repeat itself.
package notify

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/kubeamber/kubeamber/internal/changerequest"
)

// Event is what happened to a held change.
type Event string

const (
	// Held: an agent's change is waiting for a person.
	Held Event = "held"
	// Reminder: it is still waiting after spec.remindAfter.
	Reminder Event = "reminder"
	// Approved: a person approved it; the agent may retry it once.
	Approved Event = "approved"
	// Rejected: a person rejected it.
	Rejected Event = "rejected"
	// Applied: the agent retried and the approved change was made.
	Applied Event = "applied"
	// Expired: nobody decided in time, or the approval was never used.
	Expired Event = "expired"
)

// Events lists every event, in the order they can happen.
var Events = []Event{Held, Reminder, Approved, Rejected, Applied, Expired}

// Message is one notification about one ChangeRequest. Channels render it.
type Message struct {
	Event         Event     `json:"event"`
	Title         string    `json:"title"`
	Cluster       string    `json:"cluster,omitempty"`
	ChangeRequest string    `json:"changeRequest"`
	Agent         string    `json:"agent"`
	User          string    `json:"user"`
	Owner         string    `json:"owner,omitempty"`
	Operation     string    `json:"operation"`
	Kind          string    `json:"kind"`
	Namespace     string    `json:"namespace,omitempty"`
	Name          string    `json:"name,omitempty"`
	Risk          int       `json:"risk"`
	Summary       string    `json:"summary,omitempty"`
	Factors       []string  `json:"factors,omitempty"`
	Intent        string    `json:"intent,omitempty"`
	ExpiresAt     time.Time `json:"expiresAt"`
	DecidedBy     string    `json:"decidedBy,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Source        string    `json:"source,omitempty"`
	Ticket        string    `json:"ticket,omitempty"`
	TicketURL     string    `json:"ticketURL,omitempty"`
}

// maxIntent caps the agent's own text; the agent controls it, so it is also
// stripped of control characters before anyone sees it.
const maxIntent = 300

// NewMessage describes event for cr.
func NewMessage(cr *changerequest.ChangeRequest, event Event, cluster string) Message {
	s := cr.Spec
	m := Message{
		Event:         event,
		Cluster:       cluster,
		ChangeRequest: cr.Name,
		Agent:         s.Agent,
		User:          s.User,
		Owner:         s.Owner,
		Operation:     s.Operation,
		Kind:          s.Target.Kind,
		Namespace:     s.Target.Namespace,
		Name:          s.Target.Name,
		Risk:          s.Score,
		Summary:       s.Summary,
		Factors:       s.Factors,
		Intent:        clean(s.Intent, maxIntent),
		ExpiresAt:     cr.Deadline(),
	}
	if a := s.Approval; a != nil && event != Held && event != Reminder {
		m.DecidedBy, m.Reason, m.Source = a.By, clean(a.Reason, maxIntent), a.Source
	}
	if e := cr.Status.External; e != nil {
		m.Ticket, m.TicketURL = e.Number, e.URL
	}
	m.Title = m.title()
	return m
}

// Target is the changed object, e.g. "Deployment prod/checkout".
func (m Message) Target() string {
	name := m.Name
	if name == "" {
		name = "(new)"
	}
	if m.Namespace != "" {
		return m.Kind + " " + m.Namespace + "/" + name
	}
	return m.Kind + " " + name
}

func (m Message) title() string {
	change := fmt.Sprintf("%s %s", m.Operation, m.Target())
	switch m.Event {
	case Held:
		return fmt.Sprintf("Approval needed: %s by agent %s (risk %d/100)", change, m.Agent, m.Risk)
	case Reminder:
		return fmt.Sprintf("Still waiting for approval: %s by agent %s (risk %d/100)", change, m.Agent, m.Risk)
	case Approved:
		return fmt.Sprintf("Approved: %s by agent %s", change, m.Agent)
	case Rejected:
		return fmt.Sprintf("Rejected: %s by agent %s", change, m.Agent)
	case Applied:
		return fmt.Sprintf("Applied: %s by agent %s, after approval", change, m.Agent)
	case Expired:
		return fmt.Sprintf("Expired without being applied: %s by agent %s", change, m.Agent)
	}
	return change
}

// Lines is the body of the message, one fact per line, in plain text.
func (m Message) Lines() []string {
	var l []string
	if m.Summary != "" {
		l = append(l, "Change: "+m.Summary)
	}
	if len(m.Factors) > 0 {
		l = append(l, "Risk factors: "+strings.Join(m.Factors, ", "))
	}
	who := "Identity: " + m.User
	if m.Owner != "" {
		who += " (launched by " + m.Owner + ")"
	}
	l = append(l, who)
	if m.Intent != "" {
		l = append(l, "Agent's stated intent: "+m.Intent)
	}
	switch m.Event {
	case Held, Reminder:
		l = append(l, "Approve: kubeamber approve "+m.ChangeRequest+` --reason "..."`,
			"Reject: kubeamber reject "+m.ChangeRequest+` --reason "..."`,
			"Expires: "+m.ExpiresAt.UTC().Format(time.RFC3339))
		if m.Ticket != "" {
			l = append(l, "Or decide in "+m.Ticket+" "+m.TicketURL)
		}
	case Approved, Rejected:
		by := "By: " + m.DecidedBy
		if m.Source != "" {
			by += " in " + m.Source
			if m.Ticket != "" {
				by += " " + m.Ticket
			}
		}
		l = append(l, by)
		if m.Reason != "" {
			l = append(l, "Reason: "+m.Reason)
		}
	case Applied:
		if m.DecidedBy != "" {
			l = append(l, "Approved by: "+m.DecidedBy)
		}
	case Expired:
		l = append(l, "Nothing was changed.")
	}
	l = append(l, "Change request: "+m.ChangeRequest)
	return l
}

// clean strips control characters (which could rewrite what a reader sees)
// and caps the length.
func clean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}
