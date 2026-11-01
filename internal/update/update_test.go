package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChecksumMatches(t *testing.T) {
	sums := "abc  pairdesk_linux_amd64.tar.gz\ndef *pairdesk_linux_arm64.tar.gz\n"
	if !checksumMatches(sums, "pairdesk_linux_amd64.tar.gz", "ABC") {
		t.Fatal("match")
	}
	if !checksumMatches(sums, "pairdesk_linux_arm64.tar.gz", "def") {
		t.Fatal("star prefix")
	}
	if checksumMatches(sums, "pairdesk_linux_amd64.tar.gz", "zzz") {
		t.Fatal("mismatch accepted")
	}
}

func TestLatestByRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://github.com/x/y/releases/tag/v9.9.9", http.StatusFound)
	}))
	defer srv.Close()
	old := ReleasesURL
	ReleasesURL = srv.URL
	defer func() { ReleasesURL = old }()
	r, err := fetchRelease(context.Background())
	if err != nil || r.TagName != "v9.9.9" {
		t.Fatalf("got %+v, %v", r, err)
	}
	if len(r.Assets) != 2 || !strings.HasSuffix(r.Assets[1].URL, "/releases/download/v9.9.9/checksums.txt") {
		t.Fatalf("assets: %+v", r.Assets)
	}
}

func TestGetRetriesAndShortErrors(t *testing.T) {
	old := retryDelay
	retryDelay = 0
	defer func() { retryDelay = old }()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close() // reset the connection
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	b, err := get(context.Background(), srv.URL+"/pairdesk_linux_amd64.tar.gz?sig=secret", 1<<20, nil)
	if err != nil || string(b) != "ok" || calls != 3 {
		t.Fatalf("got %q %v after %d calls", b, err, calls)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	_, err = get(context.Background(), bad.URL+"/checksums.txt?sig=secret", 1<<20, nil)
	if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "checksums.txt") {
		t.Fatalf("error should name the file, not the signed URL: %v", err)
	}
}
