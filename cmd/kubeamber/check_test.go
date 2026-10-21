package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const groupPolicy = `
apiVersion: kubeamber.dev/v1alpha1
kind: Policy
metadata: {name: test}
spec:
  agents:
    - name: azure-sre-agent
      groups: ["6f1c2a9e-0b7d-4c55-9a3e-2d8f1e4b7c10"]
    - name: bedrock-ops-agent
      users: ["arn:aws:sts::123456789012:assumed-role/eks-ops-agent/*"]
`

const prodScale = `
apiVersion: apps/v1
kind: Deployment
metadata: {name: checkout, namespace: kube-system}
spec: {replicas: 2}
`

func runCheck(t *testing.T, args ...string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	pol, man := filepath.Join(dir, "policy.yaml"), filepath.Join(dir, "deploy.yaml")
	if err := os.WriteFile(pol, []byte(groupPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(man, []byte(prodScale), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := checkCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"-f", man, "--policy", pol}, args...))
	err := cmd.Execute()
	var ee exitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &ee):
		return out.String(), ee.code
	}
	return out.String() + err.Error(), 1
}

func TestCheckMatchesAgentsByGroup(t *testing.T) {
	out, code := runCheck(t, "--as", "0b2e7d41-aa10-4c1e-9f0d-5c3b8e2a6f77", "--group", "6f1c2a9e-0b7d-4c55-9a3e-2d8f1e4b7c10")
	if code != 0 || !strings.Contains(out, "ALLOW") {
		t.Errorf("group member: exit %d\n%s", code, out)
	}
	out, code = runCheck(t, "--as", "0b2e7d41-aa10-4c1e-9f0d-5c3b8e2a6f77")
	if code != 1 || !strings.Contains(out, "is not an agent") {
		t.Errorf("same user without the group should not be an agent: exit %d\n%s", code, out)
	}
}

func TestCheckMatchesIAMRoleSessions(t *testing.T) {
	out, code := runCheck(t, "--as", "arn:aws:sts::123456789012:assumed-role/eks-ops-agent/bedrock-session-42", "-o", "json")
	if code != 0 || !strings.Contains(out, `"agent": "bedrock-ops-agent"`) {
		t.Errorf("exit %d\n%s", code, out)
	}
}
