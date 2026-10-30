package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kubeamber/kubeamber/pkg/decide"
	"github.com/kubeamber/kubeamber/pkg/policy"
)

func TestPreviewOverMCP(t *testing.T) {
	p, err := policy.Load("../../examples/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	get := func() *policy.Policy { return p }
	s := &Server{Engine: &decide.Engine{Policy: get}, Policy: get, Agent: "sre-agent"}

	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := s.MCP().Connect(ctx, serverT, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "v0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if init := session.InitializeResult(); init.Instructions == "" {
		t.Error("server sent no instructions")
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 3 {
		t.Errorf("got %d tools", len(tools.Tools))
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "kubeamber_preview", Arguments: map[string]any{
		"manifest":  "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: prod\n",
		"operation": "DELETE",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %+v", res.Content)
	}
	var out PreviewOutput
	data, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].Decision.Verdict != policy.Deny || out.Results[0].Advice == "" {
		t.Fatalf("preview: %+v", out)
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "kubeamber_policy", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("policy tool: %v %+v", err, res)
	}
	var pol PolicyOutput
	data, _ = json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(data, &pol)
	if pol.HoldThreshold != 30 || len(pol.Rules) != 2 || len(pol.FreezeWindows) != 1 {
		t.Errorf("policy summary: %+v", pol)
	}

	// The summary tells the agent which images it can deploy without a hold.
	p, err = policy.Parse([]byte("apiVersion: kubeamber.dev/v1alpha1\nkind: Policy\nspec:\n" +
		"  agents: [{name: sre-agent, groups: [agents]}]\n" +
		"  images: {trusted: ['cgr.dev/chainguard/*'], requireDigest: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "kubeamber_policy", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("policy tool: %v %+v", err, res)
	}
	pol = PolicyOutput{}
	data, _ = json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(data, &pol)
	if len(pol.TrustedImages) != 1 || pol.TrustedImages[0] != "cgr.dev/chainguard/*" || !pol.RequireDigest {
		t.Errorf("policy summary images: %+v", pol)
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "kubeamber_change_request", Arguments: map[string]any{"name": "cr-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("change request lookup without cluster access should be a tool error")
	}
}
