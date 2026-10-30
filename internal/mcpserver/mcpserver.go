// Package mcpserver exposes KubeAmber to AI agents over the Model Context
// Protocol, so an agent can ask "what would happen if I made this change?"
// before it acts, and follow up on changes waiting for a human.
package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/kubeamber/kubeamber/internal/changerequest"
	"github.com/kubeamber/kubeamber/internal/manifest"
	"github.com/kubeamber/kubeamber/internal/version"
	"github.com/kubeamber/kubeamber/pkg/decide"
	"github.com/kubeamber/kubeamber/pkg/policy"
)

// Server answers MCP tool calls for one agent profile.
type Server struct {
	Engine *decide.Engine
	Policy func() *policy.Policy
	// Agent is the policy profile the connected agent runs as.
	Agent string
	// Cluster access is optional. With it, previews compare against the live
	// object and change requests can be looked up.
	Dynamic dynamic.Interface
	Mapper  meta.RESTMapper
	Store   *changerequest.Store
}

const instructions = `KubeAmber reviews changes that AI agents make to Kubernetes.
Before you create, change or delete anything in the cluster, call kubeamber_preview with the manifest.
- allow: go ahead.
- hold: a human must approve. Explain why the change is needed, add the annotation
  kubeamber.dev/intent: "<your goal>" to the object, apply it, and tell the user the change request name
  from the error. Retry the identical change after it is approved (check with kubeamber_change_request).
- deny: do not attempt it. Propose a safer alternative or ask a human to do it.`

// MCP builds the MCP server.
func (s *Server) MCP() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "kubeamber", Title: "KubeAmber", Version: version.Version},
		&mcp.ServerOptions{Instructions: instructions})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "kubeamber_preview",
		Title:       "Preview a Kubernetes change",
		Description: "Score a proposed Kubernetes change and report whether KubeAmber will allow it, hold it for human approval, or deny it, with the reasons. Makes no change.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.preview)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "kubeamber_policy",
		Title:       "Show the KubeAmber policy",
		Description: "Summarise the rules KubeAmber applies to this agent: thresholds, protected namespaces, explicit rules and freeze windows.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.policySummary)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "kubeamber_change_request",
		Title:       "Check a held change",
		Description: "Look up a change request by name to see whether a human has approved or rejected it.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.changeRequest)
	return srv
}

// Run serves MCP over stdio until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	return s.MCP().Run(ctx, &mcp.StdioTransport{})
}

type PreviewInput struct {
	Manifest    string `json:"manifest" jsonschema:"Kubernetes manifest (YAML or JSON) of the object as it will be after the change. For a delete, the object being deleted (apiVersion, kind and metadata are enough). Several documents separated by --- are previewed one by one."`
	Operation   string `json:"operation,omitempty" jsonschema:"CREATE, UPDATE, DELETE or CONNECT (exec/attach/port-forward). Defaults to UPDATE if the object exists in the cluster, otherwise CREATE."`
	Namespace   string `json:"namespace,omitempty" jsonschema:"Namespace for objects whose manifest does not set one."`
	SubResource string `json:"subresource,omitempty" jsonschema:"Subresource, e.g. exec for CONNECT or scale."`
}

type PreviewResult struct {
	Target    string          `json:"target"`
	Operation string          `json:"operation"`
	Decision  decide.Decision `json:"decision"`
	// ComparedWithLive is true when the live object was fetched to compute the diff.
	ComparedWithLive bool   `json:"comparedWithLive"`
	Advice           string `json:"advice"`
}

type PreviewOutput struct {
	Results []PreviewResult `json:"results"`
}

func (s *Server) preview(ctx context.Context, _ *mcp.CallToolRequest, in PreviewInput) (*mcp.CallToolResult, PreviewOutput, error) {
	objs, err := manifest.Parse([]byte(in.Manifest))
	if err != nil {
		return nil, PreviewOutput{}, fmt.Errorf("parsing manifest: %w", err)
	}
	if len(objs) == 0 {
		return nil, PreviewOutput{}, fmt.Errorf("the manifest is empty")
	}
	var out PreviewOutput
	for _, obj := range objs {
		opts := manifest.Options{Operation: in.Operation, Namespace: in.Namespace, SubResource: in.SubResource}
		live := s.lookup(ctx, obj, &opts)
		c, err := manifest.ToChange(obj, opts)
		if err != nil {
			return nil, PreviewOutput{}, err
		}
		d := s.Engine.Evaluate(ctx, decide.Request{Agent: s.Agent, Change: c})
		out.Results = append(out.Results, PreviewResult{
			Target: c.Target(), Operation: c.Operation, Decision: d, ComparedWithLive: live, Advice: advice(d),
		})
	}
	return nil, out, nil
}

// lookup fetches the live object so the preview can see what changes. It
// fills in the resource name, scope and old object, and reports success.
func (s *Server) lookup(ctx context.Context, obj map[string]any, o *manifest.Options) bool {
	if s.Dynamic == nil || s.Mapper == nil {
		return false
	}
	apiVersion, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return false
	}
	m, err := s.Mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: kind}, gv.Version)
	if err != nil {
		return false
	}
	o.Resource = m.Resource.Resource
	namespaced := m.Scope.Name() == meta.RESTScopeNameNamespace
	o.Namespaced = &namespaced

	md, _ := obj["metadata"].(map[string]any)
	name, _ := md["name"].(string)
	if name == "" {
		return false
	}
	var ri dynamic.ResourceInterface = s.Dynamic.Resource(m.Resource)
	if namespaced {
		ns, _ := md["namespace"].(string)
		if ns == "" {
			ns = o.Namespace
		}
		if ns == "" {
			ns = "default"
		}
		ri = s.Dynamic.Resource(m.Resource).Namespace(ns)
	}
	current, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) && o.Operation == "" {
			o.Operation = "CREATE"
		}
		return false
	}
	o.Old = current.Object
	return true
}

func advice(d decide.Decision) string {
	switch d.Verdict {
	case policy.Allow:
		return "KubeAmber will allow this change."
	case policy.Hold:
		return "KubeAmber will hold this change for human approval. Tell the user why it is needed, set the kubeamber.dev/intent annotation, apply it, and share the change request name from the error. Retry the identical change once it is approved. A lower-risk alternative (fewer replicas affected, a non-protected namespace, no deletion) may be allowed without approval."
	default:
		return "KubeAmber will deny this change. Do not attempt it. Propose a safer alternative, or ask a human operator to make it."
	}
}

type PolicyInput struct{}

type PolicyOutput struct {
	Agent               string   `json:"agent"`
	HoldThreshold       int      `json:"holdThreshold"`
	DenyThreshold       int      `json:"denyThreshold"`
	ProtectedNamespaces []string `json:"protectedNamespaces"`
	Rules               []string `json:"rules"`
	FreezeWindows       []string `json:"freezeWindows,omitempty"`
	ApprovalTTL         string   `json:"approvalTTL"`
	// TrustedImages and RequireDigest tell the agent which images it can
	// deploy without a hold.
	TrustedImages []string `json:"trustedImages,omitempty"`
	RequireDigest bool     `json:"requireDigest,omitempty"`
}

func (s *Server) policySummary(_ context.Context, _ *mcp.CallToolRequest, _ PolicyInput) (*mcp.CallToolResult, PolicyOutput, error) {
	p := s.Policy()
	out := PolicyOutput{
		Agent:               s.Agent,
		HoldThreshold:       p.Spec.Thresholds.Hold,
		DenyThreshold:       p.Spec.Thresholds.Deny,
		ProtectedNamespaces: p.Spec.ProtectedNamespaces,
		ApprovalTTL:         p.Spec.Approvals.TTL.Duration.String(),
		TrustedImages:       p.Spec.Images.Trusted,
		RequireDigest:       p.Spec.Images.RequireDigest,
	}
	for _, w := range p.Spec.FreezeWindows {
		out.FreezeWindows = append(out.FreezeWindows, describeWindow(w))
	}
	for _, r := range p.Spec.Rules {
		if len(r.Match.Agents) > 0 && !policy.GlobAny(r.Match.Agents, s.Agent) {
			continue
		}
		desc := fmt.Sprintf("%s: %s", strings.ToUpper(string(r.Action)), r.Name)
		if r.Reason != "" {
			desc += " (" + r.Reason + ")"
		}
		out.Rules = append(out.Rules, desc)
	}
	return nil, out, nil
}

var timeNow = time.Now

func describeWindow(w policy.FreezeWindow) string {
	scope := "all namespaces"
	if len(w.Namespaces) > 0 {
		scope = strings.Join(w.Namespaces, ", ")
	}
	when := fmt.Sprintf("%s %s-%s %s", strings.Join(w.Days, ","), w.Start, w.End, w.TimeZone)
	if w.From != nil {
		when = fmt.Sprintf("%s to %s", w.From.UTC().Format(time.RFC3339), w.Until.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("%s: %s in %s during %s", w.Name, strings.ToUpper(string(w.Action)), scope, strings.TrimSpace(when))
}

type ChangeRequestInput struct {
	Name string `json:"name" jsonschema:"Change request name from KubeAmber's error message, e.g. cr-3fa9c2d1-x7k2q."`
}

type ChangeRequestOutput struct {
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Target    string `json:"target"`
	DecidedBy string `json:"decidedBy,omitempty"`
	// Ticket and TicketURL identify the change-management ticket, if any.
	Ticket    string `json:"ticket,omitempty"`
	TicketURL string `json:"ticketURL,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Next      string `json:"next"`
}

func (s *Server) changeRequest(ctx context.Context, _ *mcp.CallToolRequest, in ChangeRequestInput) (*mcp.CallToolResult, ChangeRequestOutput, error) {
	if s.Store == nil {
		return nil, ChangeRequestOutput{}, fmt.Errorf("kubeamber mcp was started without cluster access")
	}
	cr, err := s.Store.Get(ctx, in.Name)
	if err != nil {
		return nil, ChangeRequestOutput{}, err
	}
	p := s.Policy()
	phase := cr.Effective(timeNow(), p.Spec.Approvals.TTL.Duration)
	t := cr.Spec.Target
	out := ChangeRequestOutput{Name: cr.Name, Phase: string(phase),
		Target: strings.TrimSpace(fmt.Sprintf("%s %s %s/%s", cr.Spec.Operation, t.Kind, t.Namespace, t.Name))}
	if a := cr.Spec.Approval; a != nil {
		out.DecidedBy, out.Reason = a.By, a.Reason
	}
	if e := cr.Status.External; e != nil {
		out.Ticket, out.TicketURL = e.Number, e.URL
	}
	switch phase {
	case changerequest.Pending:
		out.Next = "Still waiting for a human. Do not retry yet."
		if out.Ticket != "" {
			out.Next = "Waiting for approval of " + out.Ticket + " in the change-management system. Tell the user the ticket number; do not retry yet."
		}
	case changerequest.Approved:
		out.Next = "Approved. Retry the identical change now; the approval can be used once."
	case changerequest.Rejected:
		out.Next = "Rejected. Do not retry; tell the user and propose an alternative."
	case changerequest.Consumed:
		out.Next = "The approved change has already been applied."
	case changerequest.Expired:
		out.Next = "Expired. Retrying the change will raise a new change request."
	}
	return nil, out, nil
}
