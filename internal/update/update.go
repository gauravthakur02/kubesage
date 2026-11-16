// Package update checks GitHub for a newer kubesage release.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// LatestURL is the GitHub API endpoint for the latest release.
var LatestURL = "https://api.github.com/repos/gauravthakur02/kubesage/releases/latest"

// Release is the newest published version.
type Release struct {
	Version string
	URL     string
}

// Latest fetches the newest release (no personal data is sent; only a generic user agent).
func Latest(ctx context.Context) (*Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LatestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kubesage-update-check")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: %s", resp.Status)
	}
	var body struct {
		Tag string `json:"tag_name"`
		URL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return &Release{Version: strings.TrimPrefix(body.Tag, "v"), URL: body.URL}, nil
}

// Newer reports whether version b is newer than a (semver major.minor.patch; pre-release suffixes ignored).
func Newer(a, b string) bool {
	pa, pb := parse(a), parse(b)
	for i := 0; i < 3; i++ {
		if pb[i] != pa[i] {
			return pb[i] > pa[i]
		}
	}
	return false
}

func parse(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// IsRelease reports whether a version string is a real release (not a dev or git-describe build).
func IsRelease(v string) bool {
	if v == "" || strings.Contains(v, "dev") || strings.Count(strings.TrimPrefix(v, "v"), ".") != 2 {
		return false
	}
	return !strings.Contains(v, "-")
}
