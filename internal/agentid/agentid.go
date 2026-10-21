// Package agentid gives each AI agent its own short-lived Kubernetes
// identity, so a person never has to hand their own credentials to an agent.
//
// An agent identity is a service account named after the agent and the
// person who launched it (claude-code-alice), annotated with that person as
// its owner. Its permissions come from role bindings the person creates; the
// API server refuses to bind a role they do not hold themselves, so an agent
// can never get more access than its owner. Credentials are bound tokens
// from the TokenRequest API: they expire on their own, and deleting the
// service account revokes every one of them at once.
package agentid

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/kubeamber/kubeamber/pkg/decide"
)

const (
	// OwnerAnnotation names the person who launched the agent. The webhook
	// stops agents from changing it.
	OwnerAnnotation = decide.OwnerAnnotation
	// AgentLabel names the agent, e.g. claude-code.
	AgentLabel = "kubeamber.dev/agent"
	// ServiceAccountLabel marks role bindings made for an agent identity.
	ServiceAccountLabel = "kubeamber.dev/service-account"
	managedBy           = "app.kubernetes.io/managed-by"
)

var agentName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// Name returns the service account name for an agent launched by owner,
// e.g. ("claude-code", "alice@contoso.com") -> "claude-code-alice-contoso-com".
// Long names are shortened with a hash so they stay unique.
func Name(agent, owner string) (string, error) {
	if !agentName.MatchString(agent) {
		return "", fmt.Errorf("agent name %q must be lower-case letters, digits and dashes (at most 32)", agent)
	}
	slug := strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(owner), "-"), "-")
	if slug == "" {
		return "", fmt.Errorf("cannot derive a name from owner %q", owner)
	}
	name := agent + "-" + slug
	if len(name) > 63 {
		sum := fmt.Sprintf("%x", sha256.Sum256([]byte(owner)))[:8]
		name = strings.TrimRight(name[:63-9], "-") + "-" + sum
	}
	return name, nil
}

// Identity describes an agent identity to create or refresh.
type Identity struct {
	Namespace string // where agent service accounts live, e.g. kubeamber-agents
	Agent     string
	Owner     string
	// Role is a ClusterRole to bind, e.g. edit; empty binds nothing (the
	// identity then has whatever an administrator granted its namespace).
	Role string
	// Namespaces to bind Role in; empty with ClusterWide false binds nothing.
	Namespaces  []string
	ClusterWide bool
}

// ServiceAccountName is the identity's service account name.
func (id Identity) ServiceAccountName() (string, error) { return Name(id.Agent, id.Owner) }

// Username is the Kubernetes username the identity authenticates as.
func (id Identity) Username() (string, error) {
	n, err := id.ServiceAccountName()
	return "system:serviceaccount:" + id.Namespace + ":" + n, err
}

// Ensure creates the service account and role bindings, or checks that the
// existing ones belong to the same owner.
func Ensure(ctx context.Context, cs kubernetes.Interface, id Identity) (*corev1.ServiceAccount, error) {
	name, err := id.ServiceAccountName()
	if err != nil {
		return nil, err
	}
	sas := cs.CoreV1().ServiceAccounts(id.Namespace)
	sa, err := sas.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		sa, err = sas.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: id.Namespace,
			Labels:      map[string]string{AgentLabel: id.Agent, managedBy: "kubeamber"},
			Annotations: map[string]string{OwnerAnnotation: id.Owner},
		}}, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("creating service account %s/%s: %w", id.Namespace, name, err)
		}
	case err != nil:
		return nil, fmt.Errorf("reading service account %s/%s: %w", id.Namespace, name, err)
	case sa.Annotations[OwnerAnnotation] != id.Owner:
		return nil, fmt.Errorf("service account %s/%s belongs to %q, not %q", id.Namespace, name, sa.Annotations[OwnerAnnotation], id.Owner)
	}
	if id.Role == "" {
		return sa, nil
	}
	subject := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: id.Namespace}
	meta := func(ns string) metav1.ObjectMeta {
		return metav1.ObjectMeta{
			Name: name + "-" + id.Role, Namespace: ns,
			Labels:      map[string]string{ServiceAccountLabel: name, AgentLabel: id.Agent, managedBy: "kubeamber"},
			Annotations: map[string]string{OwnerAnnotation: id.Owner},
		}
	}
	ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: id.Role}
	if id.ClusterWide {
		_, err := cs.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: meta(""), RoleRef: ref, Subjects: []rbacv1.Subject{subject}}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("binding %s cluster-wide: %w", id.Role, err)
		}
	}
	for _, ns := range id.Namespaces {
		_, err := cs.RbacV1().RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: meta(ns), RoleRef: ref, Subjects: []rbacv1.Subject{subject}}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("binding %s in %s: %w", id.Role, ns, err)
		}
	}
	return sa, nil
}

// Token mints a bound token for the service account, valid for d.
func Token(ctx context.Context, cs kubernetes.Interface, namespace, name string, d time.Duration) (string, time.Time, error) {
	secs := int64(d.Seconds())
	tr, err := cs.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, name, &authnv1.TokenRequest{
		Spec: authnv1.TokenRequestSpec{ExpirationSeconds: &secs},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("minting a token for %s/%s: %w", namespace, name, err)
	}
	return tr.Status.Token, tr.Status.ExpirationTimestamp.Time, nil
}

// Revoke deletes the identity's role bindings and service account. Deleting
// the service account invalidates every token minted for it.
func Revoke(ctx context.Context, cs kubernetes.Interface, namespace, name, owner string, force bool) error {
	sa, err := cs.CoreV1().ServiceAccounts(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !force && sa.Annotations[OwnerAnnotation] != owner {
		return fmt.Errorf("service account %s/%s belongs to %q; pass --force to revoke someone else's agent", namespace, name, sa.Annotations[OwnerAnnotation])
	}
	sel := metav1.ListOptions{LabelSelector: ServiceAccountLabel + "=" + name}
	rbs, err := cs.RbacV1().RoleBindings("").List(ctx, sel)
	if err != nil {
		return err
	}
	for _, rb := range rbs.Items {
		if err := cs.RbacV1().RoleBindings(rb.Namespace).Delete(ctx, rb.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	crbs, err := cs.RbacV1().ClusterRoleBindings().List(ctx, sel)
	if err != nil {
		return err
	}
	for _, crb := range crbs.Items {
		if err := cs.RbacV1().ClusterRoleBindings().Delete(ctx, crb.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return cs.CoreV1().ServiceAccounts(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// Kubeconfig renders a kubeconfig that talks to the same API server as cfg
// with the agent's token.
func Kubeconfig(cfg *rest.Config, contextName, namespace, token string) ([]byte, error) {
	ca := cfg.CAData
	if len(ca) == 0 && cfg.CAFile != "" {
		var err error
		if ca, err = os.ReadFile(cfg.CAFile); err != nil {
			return nil, fmt.Errorf("reading the cluster CA: %w", err)
		}
	}
	kc := clientcmdapi.NewConfig()
	kc.Clusters[contextName] = &clientcmdapi.Cluster{
		Server: cfg.Host, CertificateAuthorityData: ca, InsecureSkipTLSVerify: cfg.Insecure, TLSServerName: cfg.ServerName,
	}
	kc.AuthInfos[contextName] = &clientcmdapi.AuthInfo{Token: token}
	kc.Contexts[contextName] = &clientcmdapi.Context{Cluster: contextName, AuthInfo: contextName, Namespace: namespace}
	kc.CurrentContext = contextName
	return clientcmd.Write(*kc)
}

// Owners looks up who launched an agent, for the webhook.
type Owners struct {
	Client kubernetes.Interface
}

// Owner returns the owner annotation of the service account behind a
// username, or "" for other identities and unannotated accounts.
func (o *Owners) Owner(ctx context.Context, username string) string {
	rest, ok := strings.CutPrefix(username, "system:serviceaccount:")
	if !ok {
		return ""
	}
	ns, name, ok := strings.Cut(rest, ":")
	if !ok {
		return ""
	}
	sa, err := o.Client.CoreV1().ServiceAccounts(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return sa.Annotations[OwnerAnnotation]
}
