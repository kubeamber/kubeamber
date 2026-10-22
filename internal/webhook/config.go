package webhook

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/kubeamber/kubeamber/pkg/policy"
)

// AgentsWebhook is the name of the webhook entry that reviews agent changes.
const AgentsWebhook = "agents.kubeamber.dev"

// AgentMatchCondition is the name of the CEL condition KubeAmber maintains on
// the agents webhook.
const AgentMatchCondition = "kubeamber-agents"

// AgentExpression builds a CEL expression that is true only for identities
// the policy treats as agents, so the API server never sends other traffic
// to KubeAmber. Requests from KubeAmber itself are always excluded.
func AgentExpression(p *policy.Policy, selfUser string) string {
	var terms []string
	user := "request.userInfo.username"
	for _, a := range p.Spec.Agents {
		for _, u := range a.Users {
			terms = append(terms, globTerm(user, u))
		}
		for _, sa := range a.ServiceAccounts {
			terms = append(terms, globTerm(user, policy.ServiceAccountUsername(sa)))
		}
		for _, g := range a.Groups {
			terms = append(terms, "request.userInfo.groups.exists(g, "+globTerm("g", g)+")")
		}
	}
	return fmt.Sprintf("%s != %s && (%s)", user, celString(selfUser), strings.Join(dedupe(terms), " || "))
}

func globTerm(field, pattern string) string {
	if !strings.Contains(pattern, "*") {
		return field + " == " + celString(pattern)
	}
	return field + ".matches(" + celString(policy.GlobRegexp(pattern)) + ")"
}

// celString quotes s as a CEL string literal.
func celString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	out := s[:0]
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// Configurator keeps the admission configuration in step with the running
// webhook: the CA bundle it serves with, and which identities are agents.
type Configurator struct {
	Client kubernetes.Interface
	// Name is the ValidatingWebhookConfiguration.
	Name string
	// AdmissionPolicy is the ValidatingAdmissionPolicy that blocks agents
	// from changing admission webhooks; empty to skip it.
	AdmissionPolicy string
	SelfUser        string
}

// Sync updates the CA bundle of every webhook in the configuration and the
// agent match condition of the agents webhook and the admission policy. It
// writes only on change and reports whether anything was written.
func (c *Configurator) Sync(ctx context.Context, caPEM []byte, p *policy.Policy) (bool, error) {
	cond := admissionregv1.MatchCondition{Name: AgentMatchCondition, Expression: AgentExpression(p, c.SelfUser)}

	// Every replica syncs, so two may write the same content at once: on a
	// conflict, read again, and the second attempt usually finds nothing to do.
	changed := false
	api := c.Client.AdmissionregistrationV1().ValidatingWebhookConfigurations()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cfg, err := api.Get(ctx, c.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading ValidatingWebhookConfiguration %s: %w", c.Name, err)
		}
		changed = false
		for i := range cfg.Webhooks {
			w := &cfg.Webhooks[i]
			if caPEM != nil && !bytes.Equal(w.ClientConfig.CABundle, caPEM) {
				w.ClientConfig.CABundle = caPEM
				changed = true
			}
			if w.Name == AgentsWebhook {
				changed = setCondition(&w.MatchConditions, cond) || changed
			}
		}
		if !changed {
			return nil
		}
		_, err = api.Update(ctx, cfg, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return false, fmt.Errorf("updating ValidatingWebhookConfiguration %s: %w", c.Name, err)
	}

	if c.AdmissionPolicy == "" {
		return changed, nil
	}
	vapChanged := false
	vapAPI := c.Client.AdmissionregistrationV1().ValidatingAdmissionPolicies()
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vap, err := vapAPI.Get(ctx, c.AdmissionPolicy, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading ValidatingAdmissionPolicy %s: %w", c.AdmissionPolicy, err)
		}
		vapChanged = setCondition(&vap.Spec.MatchConditions, cond)
		if !vapChanged {
			return nil
		}
		_, err = vapAPI.Update(ctx, vap, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return changed, fmt.Errorf("updating ValidatingAdmissionPolicy %s: %w", c.AdmissionPolicy, err)
	}
	return changed || vapChanged, nil
}

// setCondition puts cond first in conds, keeping any other conditions, and
// reports whether that changed anything.
func setCondition(conds *[]admissionregv1.MatchCondition, cond admissionregv1.MatchCondition) bool {
	out := []admissionregv1.MatchCondition{cond}
	for _, mc := range *conds {
		if mc.Name != cond.Name {
			out = append(out, mc)
		}
	}
	if reflect.DeepEqual(*conds, out) {
		return false
	}
	*conds = out
	return true
}
