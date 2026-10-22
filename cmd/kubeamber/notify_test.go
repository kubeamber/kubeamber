package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadNotifyConfigExample(t *testing.T) {
	c, err := loadNotifyConfig("../../deploy/notify/notify.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.channels()) != 2 || len(c.Spec.Routes) != 2 || c.Spec.Interval.Duration != 15*time.Second ||
		c.Spec.RemindAfter.Duration != 15*time.Minute || c.Spec.Cluster == "" {
		t.Errorf("example config: %+v", c.Spec)
	}
}

func TestLoadNotifyConfigRejectsMistakes(t *testing.T) {
	head := "apiVersion: kubeamber.dev/v1alpha1\nkind: NotifyConfig\nspec:\n"
	for name, spec := range map[string]string{
		"no channels":       "  channels: []\n",
		"two kinds":         "  channels:\n    - {name: a, slack: {urlFile: /x}, teams: {urlFile: /y}}\n",
		"no kind":           "  channels:\n    - {name: a}\n",
		"no url":            "  channels:\n    - {name: a, slack: {}}\n",
		"duplicate names":   "  channels:\n    - {name: a, slack: {urlFile: /x}}\n    - {name: a, teams: {urlFile: /y}}\n",
		"unknown channel":   "  channels:\n    - {name: a, slack: {urlFile: /x}}\n  routes:\n    - {channels: [b]}\n",
		"unknown event":     "  channels:\n    - {name: a, slack: {urlFile: /x}}\n  routes:\n    - {channels: [a], events: [sent]}\n",
		"unknown field":     "  channels:\n    - {name: a, slack: {urlFile: /x, url: https://x}}\n",
		"wrong kind of doc": "",
	} {
		doc := head + spec
		if name == "wrong kind of doc" {
			doc = "apiVersion: kubeamber.dev/v1alpha1\nkind: BridgeConfig\nspec: {}\n"
		}
		p := filepath.Join(t.TempDir(), "notify.yaml")
		if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadNotifyConfig(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRemindAfterZeroTurnsRemindersOff(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notify.yaml")
	doc := "apiVersion: kubeamber.dev/v1alpha1\nkind: NotifyConfig\nspec:\n  remindAfter: 0s\n  channels:\n    - {name: a, webhook: {urlFile: /x}}\n"
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadNotifyConfig(p)
	if err != nil || c.Spec.RemindAfter.Duration != 0 || !strings.HasPrefix(c.channels()[0].Name(), "a") {
		t.Fatalf("config %+v, %v", c, err)
	}
}
