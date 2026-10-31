package update

import "testing"

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
