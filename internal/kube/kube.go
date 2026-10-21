// Package kube builds Kubernetes clients from a kubeconfig or, inside a pod,
// from the in-cluster service account.
package kube

import (
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Flags select the kubeconfig and context; both may be empty.
type Flags struct {
	Kubeconfig string
	Context    string
	// As impersonates a user, e.g. a service account, like kubectl --as.
	As string
}

// Config returns a REST config. Without an explicit kubeconfig it follows
// KUBECONFIG, then ~/.kube/config, then the in-cluster service account.
func (f Flags) Config() (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if f.Kubeconfig != "" {
		rules.ExplicitPath = f.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: f.Context}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.UserAgent = "kubeamber"
	if f.As != "" {
		cfg.Impersonate.UserName = f.As
	}
	return cfg, nil
}
