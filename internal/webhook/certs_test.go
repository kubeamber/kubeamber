package webhook

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

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
	if err := leaf.VerifyHostname("kubeamber.kubeamber-system.svc"); err != nil {
		t.Error(err)
	}
	block, _ := pem.Decode(ca)
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "kubeamber.kubeamber-system.svc"}); err != nil {
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

	// A certificate for another service is replaced.
	other := &Certs{Client: cs, Namespace: "kubeamber-system", SecretName: "kubeamber-webhook-tls", ServiceName: "renamed"}
	if _, ca3, err := other.Ensure(ctx); err != nil || bytes.Equal(ca, ca3) {
		t.Errorf("certificate for the old service name was reused (err %v)", err)
	}
	s, _ := cs.CoreV1().Secrets("kubeamber-system").Get(ctx, "kubeamber-webhook-tls", metav1.GetOptions{})
	if s.Type != "kubernetes.io/tls" {
		t.Errorf("secret type %s", s.Type)
	}
}
