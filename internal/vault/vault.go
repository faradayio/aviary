// Package vault mints and caches short-lived child Vault tokens from a
// developer's local Vault login (`~/.vault-token`) or an ambient VAULT_TOKEN,
// so stacks never need manual token exports in every shell. A child token
// carries every policy of its parent; it differs only in expiring sooner and
// being revoked with the parent.
package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/faradayio/aviary/internal/config"
)

// DefaultTTL is the lifetime requested for child tokens when aviary.yml omits
// vault.ttl.
const DefaultTTL = 8 * time.Hour

// Client talks to one Vault server and keeps a locked on-disk cache of minted
// child tokens.
type Client struct {
	Addr       string
	TTL        time.Duration
	CacheDir   string
	HomeDir    string
	HTTPClient *http.Client
	Now        func() time.Time
	// Stderr receives a warning when Vault grants a child token a shorter
	// lease than TTL, usually because the parent login is about to expire.
	Stderr io.Writer
}

// cachedToken is a minted child token as saved on disk, shared by every
// worktree that uses the same Vault server.
type cachedToken struct {
	Addr      string    `json:"addr"`
	Token     string    `json:"token"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// fresh reports whether less than half of the token's granted lease has
// passed, so a long test suite does not start with a token about to expire.
// It trusts the recorded expiry instead of asking Vault, so commands work
// offline while the token lasts. Measuring against the granted lease, not the
// requested TTL, keeps a short lease (from a parent login near its end) in
// use for half its life instead of minting a new token on every command.
func (t cachedToken) fresh(now time.Time) bool {
	return now.Before(t.IssuedAt.Add(t.ExpiresAt.Sub(t.IssuedAt) / 2))
}

type createTokenRequest struct {
	TTL         string `json:"ttl"`
	Renewable   bool   `json:"renewable"`
	DisplayName string `json:"display_name"`
}

type vaultResponse struct {
	Auth *struct {
		ClientToken   string `json:"client_token"`
		LeaseDuration int    `json:"lease_duration"`
	} `json:"auth"`
	Errors []string `json:"errors"`
}

// NewClient builds a Vault client from the aviary.yml vault section.
func NewClient(cfg *config.Vault) (*Client, error) {
	cacheDir, err := defaultCacheDir()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	ttl := DefaultTTL
	if cfg.TTL != "" {
		parsed, err := time.ParseDuration(cfg.TTL)
		if err != nil {
			return nil, fmt.Errorf("vault.ttl %q: %w", cfg.TTL, err)
		}
		ttl = parsed
	}
	return &Client{
		Addr:       strings.TrimRight(cfg.Addr, "/"),
		TTL:        ttl,
		CacheDir:   cacheDir,
		HomeDir:    home,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		Now:        time.Now,
		Stderr:     os.Stderr,
	}, nil
}

func defaultCacheDir() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "aviary"), nil
}

// Token returns a live Vault token for services to read secrets with.
//
// Resolution order:
//  1. A non-empty VAULT_TOKEN in env, for CI or explicit overrides. With no
//     `~/.vault-token` to fall back to, it is used as is without asking
//     Vault. Otherwise it is used only if Vault accepts it on lookup-self,
//     because agent shells often inherit stale VAULT_TOKEN exports.
//  2. A fresh cached child token on disk for this Vault server (see
//     cachedToken.fresh), used without asking Vault.
//  3. A new non-renewable child token minted from `~/.vault-token` and saved
//     to the cache with mode 0600.
func (c *Client) Token() (string, error) {
	envToken := strings.TrimSpace(os.Getenv("VAULT_TOKEN"))
	homeToken := c.readHomeToken()

	if envToken != "" {
		if homeToken == "" {
			return envToken, nil
		}
		if ok, err := c.validToken(envToken); err != nil {
			return "", err
		} else if ok {
			return envToken, nil
		}
	}

	if err := os.MkdirAll(c.CacheDir, 0o700); err != nil {
		return "", err
	}
	cachePath := filepath.Join(c.CacheDir, "vault-token.json")
	lock := flock.New(cachePath + ".lock")
	if err := lock.Lock(); err != nil {
		return "", fmt.Errorf("lock %s: %w", lock.Path(), err)
	}
	defer lock.Unlock()

	if cached, ok := c.loadCached(cachePath); ok {
		return cached.Token, nil
	}

	if homeToken == "" {
		return "", fmt.Errorf("no Vault login found at %s; ask the user to run `VAULT_ADDR=%s vault login -method=oidc`", c.homeTokenPath(), c.Addr)
	}

	minted, err := c.mintChild(homeToken)
	if err != nil {
		return "", err
	}
	if err := c.saveCached(cachePath, minted); err != nil {
		return "", err
	}
	return minted.Token, nil
}

func (c *Client) homeTokenPath() string {
	return filepath.Join(c.HomeDir, ".vault-token")
}

func (c *Client) readHomeToken() string {
	raw, err := os.ReadFile(c.homeTokenPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func (c *Client) loadCached(path string) (cachedToken, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return cachedToken{}, false
	}
	var tok cachedToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return cachedToken{}, false
	}
	if tok.Addr != c.Addr || tok.Token == "" || !tok.fresh(c.Now()) {
		return cachedToken{}, false
	}
	return tok, true
}

func (c *Client) saveCached(path string, tok cachedToken) error {
	raw, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := writeSyncedSecret(tmp, raw); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func writeSyncedSecret(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (c *Client) validToken(token string) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, c.Addr+"/v1/auth/token/lookup-self", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-Vault-Token", token)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("reaching Vault at %s: %w", c.Addr, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return false, nil
	}
	return false, fmt.Errorf("Vault lookup-self at %s returned HTTP %d", c.Addr, resp.StatusCode)
}

func (c *Client) mintChild(parentToken string) (cachedToken, error) {
	body, err := json.Marshal(createTokenRequest{
		TTL:         fmt.Sprintf("%ds", int(c.TTL.Seconds())),
		Renewable:   false,
		DisplayName: "aviary",
	})
	if err != nil {
		return cachedToken{}, err
	}
	req, err := http.NewRequest(http.MethodPost, c.Addr+"/v1/auth/token/create", bytes.NewReader(body))
	if err != nil {
		return cachedToken{}, err
	}
	req.Header.Set("X-Vault-Token", parentToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return cachedToken{}, fmt.Errorf("reaching Vault at %s: %w", c.Addr, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return cachedToken{}, err
	}
	var parsed vaultResponse
	_ = json.Unmarshal(raw, &parsed)
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		detail := strings.Join(parsed.Errors, "; ")
		if detail == "" {
			detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return cachedToken{}, fmt.Errorf("Vault login at %s is expired or cannot create child tokens (%s); ask the user to run `VAULT_ADDR=%s vault login -method=oidc`", c.homeTokenPath(), detail, c.Addr)
	}
	if resp.StatusCode != http.StatusOK {
		return cachedToken{}, fmt.Errorf("Vault token create at %s returned HTTP %d: %s", c.Addr, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if parsed.Auth == nil || parsed.Auth.ClientToken == "" {
		return cachedToken{}, errors.New("Vault token create returned an empty client_token")
	}
	lease := c.TTL
	if parsed.Auth.LeaseDuration > 0 {
		lease = time.Duration(parsed.Auth.LeaseDuration) * time.Second
	}
	// Vault also caps leases at a role's max TTL, so a short lease does not
	// prove the login is ending.
	if lease < c.TTL && c.Stderr != nil {
		fmt.Fprintf(c.Stderr, "aviary: Vault granted a %s token instead of %s; if your Vault login at %s is about to expire, run `VAULT_ADDR=%s vault login -method=oidc`\n", lease.Round(time.Second), c.TTL, c.homeTokenPath(), c.Addr)
	}
	issuedAt := c.Now().UTC()
	return cachedToken{
		Addr:      c.Addr,
		Token:     parsed.Auth.ClientToken,
		IssuedAt:  issuedAt,
		ExpiresAt: issuedAt.Add(lease),
	}, nil
}
