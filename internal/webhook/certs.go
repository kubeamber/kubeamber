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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Certs keeps the webhook's serving certificate in a Secret, shared by all
// replicas, and publishes its CA in the webhook configuration. It is the
// zero-dependency default; pass --tls-cert-file to use cert-manager instead.
//
// The CA outlives the serving certificate: a renewal issues a new serving
// certificate from the same CA, so the published bundle does not change and
// replicas still serving the old certificate stay trusted. When the CA itself
// is replaced, the bundle carries the old CA too until the next renewal.
type Certs struct {
	Client      kubernetes.Interface
	Namespace   string
	SecretName  string
	ServiceName string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 365 * 24 * time.Hour
	renewBefore  = 30 * 24 * time.Hour

	// caKeyKey holds the CA's private key. Secrets written by v0.2.0 lack
	// it, and their CA is replaced at the next renewal.
	caKeyKey = "ca.key"
)

// Ensure returns a serving certificate and the PEM CA bundle that verifies it,
// renewing the certificate if the Secret is missing, expiring or for another
// service. Every replica that calls it gets what the Secret holds, so they
// all publish the same bundle.
func (c *Certs) Ensure(ctx context.Context) (tls.Certificate, []byte, error) {
	secrets := c.Client.CoreV1().Secrets(c.Namespace)
	for attempt := 0; attempt < 3; attempt++ {
		s, err := secrets.Get(ctx, c.SecretName, metav1.GetOptions{})
		found := err == nil
		if err != nil && !apierrors.IsNotFound(err) {
			return tls.Certificate{}, nil, fmt.Errorf("reading secret %s/%s: %w", c.Namespace, c.SecretName, err)
		}
		var old map[string][]byte
		if found {
			if cert, bundle, ok := c.usable(s); ok {
				return cert, bundle, nil
			}
			old = s.Data
		}

		data, err := c.renew(old)
		if err != nil {
			return tls.Certificate{}, nil, err
		}
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
		cert, err := tls.X509KeyPair(data[corev1.TLSCertKey], data[corev1.TLSPrivateKeyKey])
		return cert, data["ca.crt"], err
	}
	return tls.Certificate{}, nil, fmt.Errorf("secret %s/%s kept changing while it was being written", c.Namespace, c.SecretName)
}

func (c *Certs) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Certs) dnsNames() []string {
	svc := c.ServiceName
	return []string{svc, svc + "." + c.Namespace, svc + "." + c.Namespace + ".svc", svc + "." + c.Namespace + ".svc.cluster.local"}
}

// usable reports whether the Secret's certificate can keep serving: it is for
// this service, has more than renewBefore left and chains to the bundle.
func (c *Certs) usable(s *corev1.Secret) (tls.Certificate, []byte, bool) {
	cert, err := tls.X509KeyPair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	bundle := s.Data["ca.crt"]
	if err != nil || len(bundle) == 0 {
		return tls.Certificate{}, nil, false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil || leaf.NotAfter.Sub(c.now()) < renewBefore || !slices.Contains(leaf.DNSNames, c.dnsNames()[2]) {
		return tls.Certificate{}, nil, false
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle) {
		return tls.Certificate{}, nil, false
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: c.dnsNames()[2], CurrentTime: c.now()}); err != nil {
		return tls.Certificate{}, nil, false
	}
	return cert, bundle, true
}

// renew returns new Secret data: a serving certificate from the stored CA
// when it can sign one for the full leaf validity, otherwise from a new CA,
// with the old CA kept in the bundle so certificates it signed stay trusted
// until every replica has reloaded.
func (c *Certs) renew(old map[string][]byte) (map[string][]byte, error) {
	now := c.now()
	ca, caKey := storedCA(old)
	var prev *x509.Certificate
	if ca == nil || ca.NotAfter.Before(now.Add(leafValidity+renewBefore)) {
		if p := firstCert(old["ca.crt"]); p != nil && now.Before(p.NotAfter) {
			prev = p
		}
		var err error
		if ca, caKey, err = newCA(now); err != nil {
			return nil, err
		}
	}
	// A renewal from the same CA publishes just that CA, dropping one kept
	// from an earlier rotation: every replica has reloaded since.
	bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})
	if prev != nil {
		bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: prev.Raw})...)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
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
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	caKeyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		"ca.crt":                bundle,
		caKeyKey:                pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyDER}),
	}, nil
}

// storedCA returns the CA that signs serving certificates, the first in the
// bundle, if the Secret also holds its key.
func storedCA(data map[string][]byte) (*x509.Certificate, *ecdsa.PrivateKey) {
	ca := firstCert(data["ca.crt"])
	block, _ := pem.Decode(data[caKeyKey])
	if ca == nil || block == nil {
		return nil, nil
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil || !key.PublicKey.Equal(ca.PublicKey) {
		return nil, nil
	}
	return ca, key
}

func firstCert(bundle []byte) *x509.Certificate {
	block, _ := pem.Decode(bundle)
	if block == nil {
		return nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return cert
}

func newCA(now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "kubeamber-webhook-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	ca, err := x509.ParseCertificate(der)
	return ca, key, err
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// Serving holds the webhook's serving certificate and the CA bundle that
// verifies it, and swaps both in place on Refresh, so a renewed certificate is
// served without a restart.
type Serving struct {
	// Load returns the certificate and the CA bundle to publish; a nil bundle
	// means something else, such as cert-manager, publishes the CA.
	Load func(context.Context) (tls.Certificate, []byte, error)

	mu   sync.RWMutex
	cert *tls.Certificate
	ca   []byte
}

// Refresh reloads the certificate and bundle. On error the previous ones stay.
func (s *Serving) Refresh(ctx context.Context) error {
	cert, ca, err := s.Load(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cert, s.ca = &cert, ca
	return nil
}

// GetCertificate is for tls.Config.
func (s *Serving) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cert == nil {
		return nil, fmt.Errorf("no serving certificate loaded")
	}
	return s.cert, nil
}

// CABundle returns the bundle to publish in the webhook configuration.
func (s *Serving) CABundle() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ca
}
