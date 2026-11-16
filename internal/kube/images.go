package kube

import (
	"context"
	"sort"
	"strings"
)

// ImageInfo is one container image in use.
type ImageInfo struct {
	Image      string
	Registry   string
	Tag        string
	Digest     string // resolved digest from pod status (what actually runs)
	Pinned     bool   // referenced by @sha256 digest
	Latest     bool   // :latest or no tag
	Pods       int
	Namespaces []string
	Workloads  []string
}

// SplitImage returns registry, repository and tag of an image reference.
func SplitImage(img string) (registry, repo, tag string) {
	ref := img
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref, tag = ref[:i], ref[i+1:]
	}
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 2 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		return parts[0], parts[1], tag
	}
	return "docker.io", ref, tag
}

// Images lists every image used by pods in scope.
func (c *Client) Images(ctx context.Context) ([]ImageInfo, error) {
	pods, err := c.listPodsAcross(ctx, c.Namespace)
	if err != nil {
		return nil, err
	}
	by := map[string]*ImageInfo{}
	nsSet, wlSet := map[string]map[string]bool{}, map[string]map[string]bool{}
	for _, p := range pods {
		wl := p.Name
		if len(p.OwnerReferences) > 0 {
			wl = p.OwnerReferences[0].Kind + "/" + p.OwnerReferences[0].Name
		}
		digests := map[string]string{}
		for _, cs := range append(append(p.Status.InitContainerStatuses[:0:0], p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
			if i := strings.Index(cs.ImageID, "@"); i >= 0 {
				digests[cs.Name] = cs.ImageID[i+1:]
			}
		}
		for _, ct := range append(append(p.Spec.InitContainers[:0:0], p.Spec.InitContainers...), p.Spec.Containers...) {
			ii := by[ct.Image]
			if ii == nil {
				reg, _, tag := SplitImage(ct.Image)
				ii = &ImageInfo{Image: ct.Image, Registry: reg, Tag: tag, Pinned: strings.Contains(ct.Image, "@sha256:"),
					Latest: (tag == "" && !strings.Contains(ct.Image, "@")) || tag == "latest"}
				by[ct.Image] = ii
				nsSet[ct.Image], wlSet[ct.Image] = map[string]bool{}, map[string]bool{}
			}
			ii.Pods++
			if d := digests[ct.Name]; d != "" {
				ii.Digest = d
			}
			nsSet[ct.Image][p.Namespace] = true
			wlSet[ct.Image][p.Namespace+"/"+wl] = true
		}
	}
	out := make([]ImageInfo, 0, len(by))
	for img, ii := range by {
		ii.Namespaces, ii.Workloads = keysSorted(nsSet[img]), keysSorted(wlSet[img])
		out = append(out, *ii)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Image < out[j].Image })
	return out, nil
}

func keysSorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
