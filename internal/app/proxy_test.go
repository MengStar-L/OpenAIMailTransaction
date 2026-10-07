package app

import (
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientIPTrustBoundary(t *testing.T) {
	a := &App{trustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}}
	for _, tc := range []struct{ name, peer, forwarded, want string }{
		{"untrusted spoof ignored", "198.51.100.7:54321", "203.0.113.42", "198.51.100.7"},
		{"untrusted mapped IPv4 normalized", "[::ffff:198.51.100.7]:54321", "203.0.113.42", "198.51.100.7"},
		{"trusted single proxy", "10.0.0.2:54321", "198.51.100.7", "198.51.100.7"},
		{"rightmost untrusted wins", "10.0.0.2:54321", "203.0.113.42, 198.51.100.7, 10.1.1.2", "198.51.100.7"},
		{"attacker controlled leftmost ignored", "10.0.0.2:54321", "garbage, 198.51.100.7, 10.1.1.2", "198.51.100.7"},
		{"trusted mapped IPv4 normalized", "[::ffff:10.0.0.2]:54321", "::ffff:198.51.100.7", "198.51.100.7"},
		{"IPv6 trusted chain", "[fd00::2]:54321", "2001:db8::7, fd00::3", "2001:db8::7"},
		{"malformed nearest hop falls back", "10.0.0.2:54321", "198.51.100.7, garbage", "10.0.0.2"},
		{"malformed encountered hop falls back", "10.0.0.2:54321", "garbage, 10.1.1.2", "10.0.0.2"},
		{"empty forwarded falls back", "10.0.0.2:54321", "", "10.0.0.2"},
		{"empty trailing hop falls back", "10.0.0.2:54321", "198.51.100.7,", "10.0.0.2"},
		{"all trusted falls back", "10.0.0.2:54321", "10.2.2.3, 10.1.1.2", "10.0.0.2"},
		{"excessive chain falls back", "10.0.0.2:54321", strings.Repeat("10.1.1.2,", 20) + "198.51.100.7", "10.0.0.2"},
		{"bare peer supported", "198.51.100.7", "203.0.113.42", "198.51.100.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://example.test/api/config", nil)
			r.RemoteAddr = tc.peer
			r.Header.Set("X-Forwarded-For", tc.forwarded)
			if got := a.clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestForwardedHeaderIgnoredUnlessProxyExplicitlyTrusted(t *testing.T) {
	a := &App{}
	r := httptest.NewRequest("GET", "http://example.test/api/config", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := a.clientIP(r); got != "127.0.0.1" {
		t.Fatalf("default config trusted forwarded IP %q", got)
	}
}

func TestInvalidTrustedProxyConfigReleasesDatabaseAndInstanceLock(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(dir, &testProvider{})
	opts.TrustedProxies = "10.0.0.0/8, invalid-cidr"
	a, err := New(opts)
	if err == nil {
		a.Close()
		t.Fatal("invalid trusted proxy CIDR was accepted")
	}
	if !strings.Contains(err.Error(), "TRUSTED_PROXIES") {
		t.Fatalf("unexpected config error: %v", err)
	}
	// Windows refuses moving an open SQLite file; moving it away and back also
	// verifies that failed initialization did not retain a database handle.
	databasePath := filepath.Join(dir, "atelier.db")
	movedPath := filepath.Join(dir, "atelier.db.closed-check")
	if _, err := os.Stat(databasePath); err == nil {
		if err := os.Rename(databasePath, movedPath); err != nil {
			t.Fatalf("database remained open after initialization failed: %v", err)
		}
		if err := os.Rename(movedPath, databasePath); err != nil {
			t.Fatal(err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	opts.TrustedProxies = " 10.0.0.0/8 , fd00::/8 "
	reopened, err := New(opts)
	if err != nil {
		t.Fatalf("valid app failed after invalid config: %v", err)
	}
	defer reopened.Close()
	if len(reopened.trustedProxies) != 2 {
		t.Fatalf("parsed %d trusted CIDRs, want 2", len(reopened.trustedProxies))
	}
}
