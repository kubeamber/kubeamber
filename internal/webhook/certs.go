package webhook

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Certs keeps the webhook's serving certificate in a Secret, shared by all
// replicas, and publishes its CA in the webhook configuration. It is the
// zero-dependency default; pass --tls-cert-file to use cert-manager instead.
type Certs struct {
	Client      kubernetes.Interface
	Namespace   string
	SecretName  string
	ServiceName string
}

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 365 * 24 * time.Hour
	renewBefore  = 30 * 24 * time.Hour
)

// Ensure returns a serving certificate and the PEM CA that signed it,
// generating both if the Secret is missing, expiring or for another service.
func (c *Certs) Ensure(ctx context.Context) (tls.Certificate, []byte, error) {
	secrets := c.Client.CoreV1().Secrets(c.Namespace)
	for attempt := 0; attempt < 3; attempt++ {
		s, err := secrets.Get(ctx, c.SecretName, metav1.GetOptions{})
		found := err == nil
		if err != nil && !apierrors.IsNotFound(err) {
			return tls.Certificate{}, nil, fmt.Errorf("reading secret %s/%s: %w", c.Namespace, c.SecretName, err)
		}
		if found {
			if cert, ca, ok := c.usable(s); ok {
				return cert, ca, nil
			}
		}

		certPEM, keyPEM, caPEM, err := c.generate()
		if err != nil {
			return tls.Certificate{}, nil, err
		}
		data := map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM, "ca.crt": caPEM}
		if found {
			s.Data = data
			_, err = secrets.Update(ctx, s, metav1.UpdateOptions{})
		} else {
			_, err = secrets.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: c.SecretName, Namespace: c.Namespace},
				Type:       corev1.SecretTypeTLS,
				Data:       data,
			}, metav1.CreateOptions{})
		}
		if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
			continue // another replica won the race; use its certificate
		}
		if err != nil {
			return tls.Certificate{}, nil, fmt.Errorf("writing secret %s/%s: %w", c.Namespace, c.SecretName, err)
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		return cert, caPEM, err
	}
	return tls.Certificate{}, nil, fmt.Errorf("secret %s/%s kept changing while it was being written", c.Namespace, c.SecretName)
}

func (c *Certs) dnsNames() []string {
	svc := c.ServiceName
	return []string{svc, svc + "." + c.Namespace, svc + "." + c.Namespace + ".svc", svc + "." + c.Namespace + ".svc.cluster.local"}
}

func (c *Certs) usable(s *corev1.Secret) (tls.Certificate, []byte, bool) {
	cert, err := tls.X509KeyPair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if err != nil || len(s.Data["ca.crt"]) == 0 {
		return tls.Certificate{}, nil, false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil || time.Until(leaf.NotAfter) < renewBefore || !slices.Contains(leaf.DNSNames, c.dnsNames()[2]) {
		return tls.Certificate{}, nil, false
	}
	return cert, s.Data["ca.crt"], true
}

func (c *Certs) generate() (certPEM, keyPEM, caPEM []byte, err error) {
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "kubeamber-webhook-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	names := c.dnsNames()
	leafTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: names[2]},
		DNSNames:     names,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caTmpl, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}
