package kube

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HelmRelease is one revision of a Helm release, decoded from Helm's storage
// (Secrets of type helm.sh/release.v1, or ConfigMaps with the configmap driver).
type HelmRelease struct {
	Name, Namespace string
	Revision        int
	Status          string
	Chart           string
	ChartVersion    string
	AppVersion      string
	Updated         time.Time
	Description     string
	Values          map[string]any // user-supplied values (helm get values)
	Manifest        string
	Notes           string
}

type helmRecord struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Version   int    `json:"version"`
	Info      struct {
		Status       string `json:"status"`
		LastDeployed string `json:"last_deployed"`
		Description  string `json:"description"`
		Notes        string `json:"notes"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Name       string `json:"name"`
			Version    string `json:"version"`
			AppVersion string `json:"appVersion"`
		} `json:"metadata"`
	} `json:"chart"`
	Config   map[string]any `json:"config"`
	Manifest string         `json:"manifest"`
}

// DecodeHelmRelease decodes Helm's storage format: base64(gzip(JSON)).
func DecodeHelmRelease(data []byte) (*HelmRelease, error) {
	raw, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, fmt.Errorf("not a Helm release: %w", err)
	}
	if len(raw) > 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		if raw, err = io.ReadAll(io.LimitReader(zr, 64<<20)); err != nil {
			return nil, err
		}
	}
	var r helmRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	t, _ := time.Parse(time.RFC3339Nano, r.Info.LastDeployed)
	return &HelmRelease{Name: r.Name, Namespace: r.Namespace, Revision: r.Version, Status: r.Info.Status,
		Chart: r.Chart.Metadata.Name, ChartVersion: r.Chart.Metadata.Version, AppVersion: r.Chart.Metadata.AppVersion,
		Updated: t, Description: r.Info.Description, Values: r.Config, Manifest: r.Manifest, Notes: r.Info.Notes}, nil
}

type helmStored struct {
	ns, name string
	rev      int
	status   string
	data     []byte
}

// helmStorage lists Helm storage records (Secrets first, ConfigMaps as a fallback), optionally for one release.
func (c *Client) helmStorage(ctx context.Context, ns, release string) ([]helmStored, error) {
	sel := "owner=helm"
	if release != "" {
		sel += ",name=" + release
	}
	var out []helmStored
	secrets, serr := listAcross(ctx, c, ns, func(n string) ([]corev1.Secret, error) {
		l, err := c.Clientset.CoreV1().Secrets(n).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
	for _, s := range secrets {
		rev, _ := strconv.Atoi(s.Labels["version"])
		out = append(out, helmStored{s.Namespace, s.Labels["name"], rev, s.Labels["status"], s.Data["release"]})
	}
	cms, cerr := listAcross(ctx, c, ns, func(n string) ([]corev1.ConfigMap, error) {
		l, err := c.Clientset.CoreV1().ConfigMaps(n).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
	for _, cm := range cms {
		rev, _ := strconv.Atoi(cm.Labels["version"])
		out = append(out, helmStored{cm.Namespace, cm.Labels["name"], rev, cm.Labels["status"], []byte(cm.Data["release"])})
	}
	if len(out) == 0 && serr != nil {
		return nil, serr // Helm's default driver is Secrets; report why we cannot read them
	}
	_ = cerr
	return out, nil
}

// HelmReleases returns the latest revision of every release in ns (or all namespaces).
func (c *Client) HelmReleases(ctx context.Context, ns string) ([]HelmRelease, error) {
	stored, err := c.helmStorage(ctx, ns, "")
	if err != nil {
		return nil, err
	}
	latest := map[string]helmStored{}
	for _, s := range stored {
		k := s.ns + "/" + s.name
		if cur, ok := latest[k]; !ok || s.rev > cur.rev {
			latest[k] = s
		}
	}
	var out []HelmRelease
	for _, s := range latest {
		r, err := DecodeHelmRelease(s.data)
		if err != nil {
			r = &HelmRelease{Name: s.name, Namespace: s.ns, Revision: s.rev, Status: s.status, Description: "undecodable: " + err.Error()}
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// HelmHistory returns every stored revision of a release, newest first.
func (c *Client) HelmHistory(ctx context.Context, ns, name string) ([]HelmRelease, error) {
	stored, err := c.helmStorage(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	var out []HelmRelease
	for _, s := range stored {
		if s.ns != ns {
			continue
		}
		if r, err := DecodeHelmRelease(s.data); err == nil {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Revision > out[j].Revision })
	return out, nil
}
