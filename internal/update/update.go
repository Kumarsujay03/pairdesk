// Package update implements `pairdesk --update`: fetch the latest GitHub
// release, verify the SHA-256 against the published checksums file, and
// replace the running binary atomically.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Repo is the GitHub owner/name.
var Repo = "Kumarsujay03/pairdesk"

type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// Latest returns the newest release tag.
func Latest(ctx context.Context) (string, error) {
	r, err := fetchRelease(ctx)
	if err != nil {
		return "", err
	}
	return r.TagName, nil
}

// fetchRelease resolves the latest tag from the github.com redirect, which
// is not rate limited per IP the way the API is (shared networks hit the
// API's 60/hour limit and get 403s). The asset URLs follow from the tag;
// the API is only a fallback.
func fetchRelease(ctx context.Context) (*release, error) {
	if tag, err := latestByRedirect(ctx); err == nil && tag != "" {
		base := "https://github.com/" + Repo + "/releases/download/" + tag + "/"
		r := &release{TagName: tag}
		for _, n := range []string{"pairdesk_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz", "checksums.txt"} {
			r.Assets = append(r.Assets, struct {
				Name string `json:"name"`
				URL  string `json:"browser_download_url"`
				Size int64  `json:"size"`
			}{Name: n, URL: base + n})
		}
		return r, nil
	}
	return fetchReleaseAPI(ctx)
}

// ReleasesURL is the page that redirects to the newest tag; tests override it.
var ReleasesURL = "https://github.com/" + Repo + "/releases/latest"

func latestByRedirect(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "HEAD", ReleasesURL, nil)
	req.Header.Set("User-Agent", "pairdesk-updater")
	c := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/releases/tag/")
	if resp.StatusCode/100 != 3 || i < 0 {
		return "", fmt.Errorf("github: %s", resp.Status)
	}
	return loc[i+len("/releases/tag/"):], nil
}

func fetchReleaseAPI(ctx context.Context) (*release, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/"+Repo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "pairdesk-updater")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github: %s", resp.Status)
	}
	var r release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Apply downloads the asset for this OS/arch, checks it against
// checksums.txt and swaps the binary at exe. It returns the new tag.
func Apply(ctx context.Context, current string, progress func(string)) (string, error) {
	if progress == nil {
		progress = func(string) {}
	}
	r, err := fetchRelease(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimPrefix(r.TagName, "v") == strings.TrimPrefix(current, "v") {
		return r.TagName, errors.New("already up to date")
	}
	want := fmt.Sprintf("pairdesk_%s_%s", runtime.GOOS, runtime.GOARCH)
	var binURL, sumURL string
	for _, a := range r.Assets {
		if strings.HasPrefix(a.Name, want) && strings.HasSuffix(a.Name, ".tar.gz") {
			binURL = a.URL
		}
		if a.Name == "checksums.txt" {
			sumURL = a.URL
		}
	}
	if binURL == "" || sumURL == "" {
		return "", fmt.Errorf("no release asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	progress("downloading " + r.TagName)
	data, err := get(ctx, binURL, 200<<20)
	if err != nil {
		return "", err
	}
	sums, err := get(ctx, sumURL, 1<<20)
	if err != nil {
		return "", err
	}
	name := binURL[strings.LastIndex(binURL, "/")+1:]
	h := sha256.Sum256(data)
	if !checksumMatches(string(sums), name, hex.EncodeToString(h[:])) {
		return "", errors.New("checksum mismatch: refusing to install")
	}
	bin, err := extractBinary(data, "pairdesk")
	if err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return "", err
	}
	progress("installed " + r.TagName)
	return r.TagName, nil
}

func get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("User-Agent", "pairdesk-updater")
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func checksumMatches(sums, name, hexsum string) bool {
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.EqualFold(f[0], hexsum)
		}
	}
	return false
}
