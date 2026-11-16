package vault

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeVault struct {
	server  *httptest.Server
	valid   map[string]bool
	minted  atomic.Int32
	lookups atomic.Int32
	mintTTL int
}

func newFakeVault(t *testing.T, valid map[string]bool) *fakeVault {
	t.Helper()
	fv := &fakeVault{valid: valid, mintTTL: 28800}
	fv.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Vault-Token")
		switch r.URL.Path {
		case "/v1/auth/token/lookup-self":
			fv.lookups.Add(1)
			if fv.valid[tok] {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"data":{"ttl":3600}}`))
				return
			}
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
		case "/v1/auth/token/create":
			if !fv.valid[tok] {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"errors":["invalid parent token"]}`))
				return
			}
			var req createTokenRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Renewable || req.DisplayName != "aviary" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			n := fv.minted.Add(1)
			child := "child-token-" + string(rune('0'+n))
			fv.valid[child] = true
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"auth": map[string]any{
					"client_token":   child,
					"lease_duration": fv.mintTTL,
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fv.server.Close)
	return fv
}

func testClient(t *testing.T, fv *fakeVault, now time.Time) *Client {
	t.Helper()
	return &Client{
		Addr:       fv.server.URL,
		TTL:        8 * time.Hour,
		CacheDir:   t.TempDir(),
		HomeDir:    t.TempDir(),
		HTTPClient: fv.server.Client(),
		Now:        func() time.Time { return now },
	}
}

func TestTokenMintsFromHomeTokenAndCachesAcrossCalls(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "")
	fv := newFakeVault(t, map[string]bool{"parent-token": true})
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	c := testClient(t, fv, now)
	if err := os.WriteFile(filepath.Join(c.HomeDir, ".vault-token"), []byte("parent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tok1, err := c.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok1 != "child-token-1" || fv.minted.Load() != 1 {
		t.Fatalf("tok1 = %q, minted = %d", tok1, fv.minted.Load())
	}

	// Still more than half TTL remaining: reuses the cached child token.
	c.Now = func() time.Time { return now.Add(3 * time.Hour) }
	tok2, err := c.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok2 != tok1 || fv.minted.Load() != 1 {
		t.Fatalf("tok2 = %q, minted = %d; want cached token reused", tok2, fv.minted.Load())
	}

	// Past half TTL: mints a fresh child token.
	c.Now = func() time.Time { return now.Add(5 * time.Hour) }
	tok3, err := c.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok3 != "child-token-2" || fv.minted.Load() != 2 {
		t.Fatalf("tok3 = %q, minted = %d; want renewed child token", tok3, fv.minted.Load())
	}
}

func TestStaleEnvVaultTokenFallsBackToHomeToken(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "expired-agent-shell-token")
	fv := newFakeVault(t, map[string]bool{"parent-token": true})
	c := testClient(t, fv, time.Now().UTC())
	if err := os.WriteFile(filepath.Join(c.HomeDir, ".vault-token"), []byte("parent-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	tok, err := c.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "child-token-1" {
		t.Fatalf("tok = %q, want minted child token", tok)
	}
}

func TestValidEnvVaultTokenWinsWithoutMinting(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "ci-provided-token")
	fv := newFakeVault(t, map[string]bool{"ci-provided-token": true})
	c := testClient(t, fv, time.Now().UTC())

	tok, err := c.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ci-provided-token" || fv.minted.Load() != 0 {
		t.Fatalf("tok = %q, minted = %d; want env token used directly", tok, fv.minted.Load())
	}
}

func TestMissingOrExpiredLoginInstructsVaultLogin(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "")
	fv := newFakeVault(t, map[string]bool{})
	c := testClient(t, fv, time.Now().UTC())

	_, err := c.Token()
	if err == nil || !strings.Contains(err.Error(), "vault login -method=oidc") {
		t.Fatalf("missing ~/.vault-token err = %v, want oidc login hint", err)
	}

	if err := os.WriteFile(filepath.Join(c.HomeDir, ".vault-token"), []byte("expired-parent"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = c.Token()
	if err == nil || !strings.Contains(err.Error(), "vault login -method=oidc") {
		t.Fatalf("expired ~/.vault-token err = %v, want oidc login hint", err)
	}
}

func TestCachedTokenIsUsedWithoutAskingVault(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "")
	fv := newFakeVault(t, map[string]bool{"parent-token": true})
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	c := testClient(t, fv, now)
	if err := os.WriteFile(filepath.Join(c.HomeDir, ".vault-token"), []byte("parent-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Token(); err != nil {
		t.Fatal(err)
	}

	fv.server.Close()
	c.Now = func() time.Time { return now.Add(time.Hour) }
	tok, err := c.Token()
	if err != nil {
		t.Fatalf("cached token needed Vault: %v", err)
	}
	if tok != "child-token-1" || fv.lookups.Load() != 0 {
		t.Fatalf("tok = %q, lookups = %d; want cached token and no lookups", tok, fv.lookups.Load())
	}
}

func TestShortLeaseWarnsAndIsReusedForHalfItsLife(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "")
	fv := newFakeVault(t, map[string]bool{"parent-token": true})
	fv.mintTTL = 3600
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	c := testClient(t, fv, now)
	var stderr strings.Builder
	c.Stderr = &stderr
	if err := os.WriteFile(filepath.Join(c.HomeDir, ".vault-token"), []byte("parent-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Token(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "granted a 1h0m0s token instead of 8h0m0s") {
		t.Errorf("stderr = %q, want a warning about the 1h lease", stderr.String())
	}
	c.Now = func() time.Time { return now.Add(20 * time.Minute) }
	if _, err := c.Token(); err != nil {
		t.Fatal(err)
	}
	if fv.minted.Load() != 1 {
		t.Errorf("minted = %d; want the 1h token reused at 20 minutes", fv.minted.Load())
	}
	c.Now = func() time.Time { return now.Add(40 * time.Minute) }
	if _, err := c.Token(); err != nil {
		t.Fatal(err)
	}
	if fv.minted.Load() != 2 {
		t.Errorf("minted = %d; want a new token past half the 1h lease", fv.minted.Load())
	}
}

func TestEnvTokenWithoutHomeLoginPassesThroughUnchecked(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "ci-provided-token")
	fv := newFakeVault(t, map[string]bool{})
	c := testClient(t, fv, time.Now().UTC())
	fv.server.Close()

	tok, err := c.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ci-provided-token" {
		t.Fatalf("tok = %q, want the env token as is", tok)
	}
}
