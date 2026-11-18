package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DownloadBase is where release files live; {version} is replaced (tests point it at a local server).
var DownloadBase = "https://github.com/gauravthakur02/kubeweave/releases/download/v{version}"

// ArchiveName is the release archive for this platform, e.g. kubeweave_0.2.0_darwin_arm64.tar.gz.
func ArchiveName(version, goos, goarch string) string {
	arch := goarch
	if goarch == "arm" {
		arch = "armv7"
	}
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("kubeweave_%s_%s_%s%s", version, goos, arch, ext)
}

// ManagedBy says which package manager owns the binary at path ("" = installed by hand or the install script).
func ManagedBy(path string) string {
	p := filepath.ToSlash(path)
	switch {
	case strings.Contains(p, "/Cellar/") || strings.Contains(p, "/Caskroom/") || strings.Contains(p, "/homebrew/"):
		return "Homebrew (brew upgrade --cask kubeweave)"
	case strings.Contains(strings.ToLower(p), "/scoop/"):
		return "Scoop (scoop update kubeweave)"
	case p == "/usr/bin/kubeweave":
		return "your Linux package manager (apt/dnf/apk)"
	}
	return ""
}

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kubeweave-update")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

// Apply downloads version for this platform, verifies it against checksums.txt and replaces the binary at exe.
// The old binary is kept as exe+".old" until the next update (Windows cannot delete a running program).
func Apply(ctx context.Context, version, exe string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	version = strings.TrimPrefix(version, "v")
	base := strings.ReplaceAll(DownloadBase, "{version}", version)
	name := ArchiveName(version, runtime.GOOS, runtime.GOARCH)
	sums, err := get(ctx, base+"/checksums.txt")
	if err != nil {
		return err
	}
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == name {
			want = f[0]
		}
	}
	if want == "" {
		return fmt.Errorf("%s is not in checksums.txt (no build for %s/%s?)", name, runtime.GOOS, runtime.GOARCH)
	}
	archive, err := get(ctx, base+"/"+name)
	if err != nil {
		return err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("checksum mismatch for %s: refusing to install", name)
	}
	bin, err := extract(archive, runtime.GOOS == "windows")
	if err != nil {
		return err
	}
	return replace(exe, bin)
}

func extract(archive []byte, isZip bool) ([]byte, error) {
	want := "kubeweave"
	if isZip {
		want = "kubeweave.exe"
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == want {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, 300<<20))
			}
		}
		return nil, fmt.Errorf("%s not found in archive", want)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("%s not found in archive", want)
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == want {
			return io.ReadAll(io.LimitReader(tr, 300<<20))
		}
	}
}

func replace(exe string, bin []byte) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".kubeweave-new-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s (try with sudo, or reinstall with the install script): %w", dir, err)
	}
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		_ = os.Rename(old, exe) // put the old one back
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Remove(old)
	}
	return nil
}
