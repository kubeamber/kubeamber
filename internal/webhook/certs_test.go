package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

const svcName = "kubeamber.kubeamber-system.svc"

func TestCertsEnsure(t *testing.T) {
	cs := fake.NewClientset()
	c := &Certs{Client: cs, Namespace: "kubeamber-system", SecretName: "kubeamber-webhook-tls", ServiceName: "kubeamber"}
	ctx := context.Background()

	cert, ca, err := c.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname(svcName); err != nil {
		t.Error(err)
	}
	if err := verify(cert, ca, time.Now()); err != nil {
		t.Errorf("leaf does not chain to the CA: %v", err)
	}

	// A second replica reuses the stored certificate.
	_, ca2, err := c.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ca, ca2) {
		t.Error("a second Ensure generated a new CA")
	}

	// A certificate for another service is replaced, from the same CA.
	other := &Certs{Client: cs, Namespace: "kubeamber-system", SecretName: "kubeamber-webhook-tls", ServiceName: "renamed"}
	cert3, ca3, err := other.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if leaf3, _ := x509.ParseCertificate(cert3.Certificate[0]); leaf3.VerifyHostname("renamed.kubeamber-system.svc") != nil {
		t.Error("certificate for the old service name was reused")
	}
	if !bytes.Equal(ca, ca3) {
		t.Error("a new service name replaced the CA")
	}
	s, _ := cs.CoreV1().Secrets("kubeamber-system").Get(ctx, "kubeamber-webhook-tls", metav1.GetOptions{})
	if s.Type != "kubernetes.io/tls" {
		t.Errorf("secret type %s", s.Type)
	}
}

// clock is a settable time for Certs.Now.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newCerts(cs kubernetes.Interface, clk *clock) *Certs {
	return &Certs{Client: cs, Namespace: "kubeamber-system", SecretName: "kubeamber-webhook-tls", ServiceName: "kubeamber", Now: clk.now}
}

// intoRenewal is how far to move the clock so a fresh leaf is due for renewal.
const intoRenewal = leafValidity - renewBefore + time.Hour

func TestCertsRenewalKeepsCA(t *testing.T) {
	ctx := context.Background()
	clk := &clock{time.Now()}
	c := newCerts(fake.NewClientset(), clk)
	cert1, ca1, err := c.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}

	clk.add(intoRenewal)
	cert2, ca2, err := c.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(cert1.Certificate[0], cert2.Certificate[0]) {
		t.Fatal("certificate was not renewed")
	}
	if !bytes.Equal(ca1, ca2) {
		t.Error("renewing the serving certificate changed the CA bundle")
	}
	if err := verify(cert2, ca2, clk.t); err != nil {
		t.Error(err)
	}
	if err := verify(cert1, ca2, clk.t); err != nil {
		t.Errorf("a replica still serving the old certificate is no longer trusted: %v", err)
	}
}

// TestReplicasAgreeAfterRenewal is issue #16: one replica renews, and the
// other must serve and publish the same thing at its next refresh rather than
// write its startup CA back.
func TestReplicasAgreeAfterRenewal(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	clk := &clock{time.Now()}
	a := &Serving{Load: newCerts(cs, clk).Ensure}
	b := &Serving{Load: newCerts(cs, clk).Ensure}
	for _, s := range []*Serving{a, b} {
		if err := s.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}

	clk.add(intoRenewal)
	// Simulate a v0.2.0 Secret, the case where the CA has to change.
	s, _ := cs.CoreV1().Secrets("kubeamber-system").Get(ctx, "kubeamber-webhook-tls", metav1.GetOptions{})
	delete(s.Data, caKeyKey)
	if _, err := cs.CoreV1().Secrets("kubeamber-system").Update(ctx, s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	certA, _ := a.GetCertificate(nil)
	certB, _ := b.GetCertificate(nil)
	if err := verify(*certB, a.CABundle(), clk.t); err != nil {
		t.Errorf("A's bundle does not trust B's old certificate: %v", err)
	}

	if err := b.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.CABundle(), b.CABundle()) {
		t.Error("replicas publish different CA bundles after a renewal")
	}
	certB, _ = b.GetCertificate(nil)
	if !bytes.Equal(certA.Certificate[0], certB.Certificate[0]) {
		t.Error("B did not pick up the renewed certificate")
	}
}

func TestCertsReplaceCA(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, cs kubernetes.Interface, clk *clock)
	}{
		{"secret written by v0.2.0, without the CA key", func(t *testing.T, cs kubernetes.Interface, clk *clock) {
			s, _ := cs.CoreV1().Secrets("kubeamber-system").Get(ctx, "kubeamber-webhook-tls", metav1.GetOptions{})
			delete(s.Data, caKeyKey)
			if _, err := cs.CoreV1().Secrets("kubeamber-system").Update(ctx, s, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			clk.add(intoRenewal)
		}},
		{"CA too close to expiry to sign a full certificate", func(t *testing.T, cs kubernetes.Interface, clk *clock) {
			clk.add(caValidity - leafValidity)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset()
			clk := &clock{time.Now()}
			c := newCerts(cs, clk)
			cert1, ca1, err := c.Ensure(ctx)
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(t, cs, clk)

			cert2, ca2, err := c.Ensure(ctx)
			if err != nil {
				t.Fatal(err)
			}
			cas := certs(t, ca2)
			if len(cas) != 2 || !cas[1].Equal(certs(t, ca1)[0]) || cas[0].Equal(cas[1]) {
				t.Fatalf("bundle should be the new CA then the old one, got %d certificates", len(cas))
			}
			if err := verify(cert2, ca2, clk.t); err != nil {
				t.Error(err)
			}
			if leaf1, _ := x509.ParseCertificate(cert1.Certificate[0]); clk.t.Before(leaf1.NotAfter) {
				if err := verify(cert1, ca2, clk.t); err != nil {
					t.Errorf("the old certificate is not trusted during the rotation: %v", err)
				}
			}

			// The next renewal, from the new CA, drops the old one.
			clk.add(intoRenewal)
			cert3, ca3, err := c.Ensure(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := certs(t, ca3); len(got) != 1 || !got[0].Equal(cas[0]) {
				t.Errorf("after the next renewal the bundle should be just the new CA, got %d certificates", len(got))
			}
			if err := verify(cert3, ca3, clk.t); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestCertsRenewWhenBundleDoesNotMatch(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	clk := &clock{time.Now()}
	c := newCerts(cs, clk)
	if _, _, err := c.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	// Someone replaced ca.crt with an unrelated CA.
	other := newCerts(fake.NewClientset(), clk)
	_, foreign, err := other.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := cs.CoreV1().Secrets("kubeamber-system").Get(ctx, "kubeamber-webhook-tls", metav1.GetOptions{})
	s.Data["ca.crt"] = foreign
	if _, err := cs.CoreV1().Secrets("kubeamber-system").Update(ctx, s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	cert, ca, err := c.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(cert, ca, clk.t); err != nil {
		t.Errorf("published bundle does not verify the served certificate: %v", err)
	}
}

func TestServingRefresh(t *testing.T) {
	ctx := context.Background()
	var next tls.Certificate
	var fail error
	s := &Serving{Load: func(context.Context) (tls.Certificate, []byte, error) { return next, []byte("CA"), fail }}
	if _, err := s.GetCertificate(nil); err == nil {
		t.Error("GetCertificate before the first Refresh should fail")
	}

	next = tls.Certificate{Certificate: [][]byte{[]byte("one")}}
	if err := s.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	next = tls.Certificate{Certificate: [][]byte{[]byte("two")}}
	if err := s.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetCertificate(nil); string(got.Certificate[0]) != "two" || string(s.CABundle()) != "CA" {
		t.Errorf("Refresh did not swap the certificate: %q", got.Certificate[0])
	}

	fail = errors.New("secret unreadable")
	next = tls.Certificate{}
	if err := s.Refresh(ctx); err == nil {
		t.Error("Refresh should report the load error")
	}
	if got, _ := s.GetCertificate(nil); string(got.Certificate[0]) != "two" {
		t.Error("a failed Refresh dropped the working certificate")
	}
}

// verify checks that cert's leaf chains to a CA in bundle at time at.
func verify(cert tls.Certificate, bundle []byte, at time.Time) error {
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle) {
		return errors.New("bundle holds no certificates")
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: svcName, CurrentTime: at})
	return err
}

func certs(t *testing.T, bundle []byte) []*x509.Certificate {
	t.Helper()
	var out []*x509.Certificate
	for {
		var block *pem.Block
		block, bundle = pem.Decode(bundle)
		if block == nil {
			return out
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
}
