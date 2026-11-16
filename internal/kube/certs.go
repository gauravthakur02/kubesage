package kube

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CertInfo is one TLS certificate found in a Secret, optionally managed by cert-manager.
type CertInfo struct {
	Namespace, Secret string
	Subject, Issuer   string
	DNSNames          []string
	NotAfter          time.Time
	CertManager       string // Certificate name, if managed
	Ready             string // cert-manager Ready condition
	Message           string
	Err               string
}

// DaysLeft is the number of days until expiry (negative when expired).
func (c CertInfo) DaysLeft() int { return int(time.Until(c.NotAfter).Hours() / 24) }

var certGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}

// ParseCertPEM returns the first certificate of a PEM bundle.
func ParseCertPEM(data []byte) (*x509.Certificate, error) {
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			return nil, fmt.Errorf("no PEM certificate")
		}
		if b.Type == "CERTIFICATE" {
			return x509.ParseCertificate(b.Bytes)
		}
	}
}

// Certificates lists TLS secrets (public certificate only; private keys are never read into the UI)
// and cert-manager Certificates, soonest expiry first.
func (c *Client) Certificates(ctx context.Context) ([]CertInfo, error) {
	ns := c.Namespace
	byKey := map[string]*CertInfo{}
	var order []string
	secrets, serr := listAcross(ctx, c, ns, func(n string) ([]corev1.Secret, error) {
		l, err := c.Clientset.CoreV1().Secrets(n).List(ctx, metav1.ListOptions{FieldSelector: "type=kubernetes.io/tls"})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
	for _, s := range secrets {
		ci := &CertInfo{Namespace: s.Namespace, Secret: s.Name}
		if cert, err := ParseCertPEM(s.Data["tls.crt"]); err != nil {
			ci.Err = err.Error()
		} else {
			ci.Subject, ci.Issuer, ci.DNSNames, ci.NotAfter = cert.Subject.CommonName, cert.Issuer.CommonName, cert.DNSNames, cert.NotAfter
			if ci.Issuer == "" && len(cert.Issuer.Organization) > 0 {
				ci.Issuer = cert.Issuer.Organization[0]
			}
		}
		if n := s.Annotations["cert-manager.io/certificate-name"]; n != "" {
			ci.CertManager = n
		}
		k := s.Namespace + "/" + s.Name
		byKey[k] = ci
		order = append(order, k)
	}
	if c.Dynamic != nil {
		certs, cerr := listAcross(ctx, c, ns, func(n string) ([]unstructured.Unstructured, error) {
			l, err := c.Dynamic.Resource(certGVR).Namespace(n).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, err
			}
			return l.Items, nil
		})
		if cerr == nil || len(certs) > 0 {
			for _, u := range certs {
				secret, _, _ := unstructured.NestedString(u.Object, "spec", "secretName")
				k := u.GetNamespace() + "/" + secret
				ci := byKey[k]
				if ci == nil {
					ci = &CertInfo{Namespace: u.GetNamespace(), Secret: secret}
					if na, _, _ := unstructured.NestedString(u.Object, "status", "notAfter"); na != "" {
						ci.NotAfter, _ = time.Parse(time.RFC3339, na)
					}
					names, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "dnsNames")
					ci.DNSNames = names
					ci.Issuer, _, _ = unstructured.NestedString(u.Object, "spec", "issuerRef", "name")
					byKey[k] = ci
					order = append(order, k)
				}
				ci.CertManager = u.GetName()
				conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
				for _, x := range conds {
					m, _ := x.(map[string]any)
					if m["type"] == "Ready" {
						ci.Ready, ci.Message = fmt.Sprint(m["status"]), fmt.Sprint(m["message"])
					}
				}
			}
		}
	}
	if len(order) == 0 && serr != nil {
		if apierrors.IsForbidden(serr) {
			return nil, fmt.Errorf("not allowed to list secrets here: %v", serr)
		}
		return nil, serr
	}
	out := make([]CertInfo, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].NotAfter.IsZero() != out[j].NotAfter.IsZero() {
			return !out[i].NotAfter.IsZero()
		}
		return out[i].NotAfter.Before(out[j].NotAfter)
	})
	return out, nil
}

// CertHealth grades a certificate: expired/failed → Err, < 30 days → Warn.
func CertHealth(ci CertInfo) Health {
	switch {
	case ci.Err != "" || (ci.Ready != "" && ci.Ready != "True"):
		return HealthErr
	case ci.NotAfter.IsZero():
		return HealthNeutral
	case ci.DaysLeft() < 0:
		return HealthErr
	case ci.DaysLeft() < 30:
		return HealthWarn
	}
	return HealthOK
}

// ShortList joins up to n items.
func ShortList(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" +%d", len(items)-n)
}
