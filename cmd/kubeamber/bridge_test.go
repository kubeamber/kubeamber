package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubeamber/kubeamber/internal/changes/servicenow"
)

func TestShippedBridgeConfigLoads(t *testing.T) {
	c, err := loadBridgeConfig("../../deploy/servicenow-bridge/bridge.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.Interval.Duration != 15*time.Second || c.Spec.ApprovalTTL.Duration != 15*time.Minute {
		t.Errorf("spec = %+v", c.Spec)
	}
	if _, err := servicenow.New(*c.Spec.ServiceNow); err != nil {
		t.Errorf("shipped servicenow config is invalid: %v", err)
	}
}

func TestBridgeConfigRejectsTypos(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bridge.yaml")
	doc := "apiVersion: kubeamber.dev/v1alpha1\nkind: BridgeConfig\nspec:\n  intervall: 5s\n  servicenow: {instanceURL: https://x.service-now.com}\n"
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBridgeConfig(p); err == nil {
		t.Error("a misspelt field was accepted")
	}
}
