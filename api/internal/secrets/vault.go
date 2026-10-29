package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Vault is everything the portal does in Vault: write a new version of a secret and remove a
// secret with all its versions. There is deliberately no read: the portal's Vault policy has no
// read on kv/data, so a secret value can never come back through the portal.
//
// The complete list of Vault calls VaultClient makes (keep the portal-secrets policy to these):
//
//	POST   /v1/auth/<auth mount>/login          Kubernetes login (no policy needed)
//	PUT    /v1/<kv mount>/data/<ns>/<name>      new version: capabilities create, update on <kv>/data/*
//	DELETE /v1/<kv mount>/metadata/<ns>/<name>  all versions: capability delete on <kv>/metadata/*
//
// Nothing reads or lists kv/data or kv/metadata.
type Vault interface {
	// Write stores data as a new version of kv/data/<ns>/<name> and returns that version.
	Write(ctx context.Context, ns, name string, data map[string]string) (int, error)
	// DeleteAll removes kv/metadata/<ns>/<name> (every version). A missing secret is not an error.
	DeleteAll(ctx context.Context, ns, name string) error
}

// VaultConfig configures VaultClient. Every field has a default (see DefaultVaultConfig).
type VaultConfig struct {
	Addr      string // e.g. http://vault.vault.svc.cluster.local:8200
	Role      string // Kubernetes auth role
	AuthMount string // Kubernetes auth mount, e.g. kubernetes
	KVMount   string // KV v2 mount, e.g. kv
	TokenPath string // projected ServiceAccount token with audience vault
}

func DefaultVaultConfig() VaultConfig {
	return VaultConfig{
		Addr:      "http://vault.vault.svc.cluster.local:8200",
		Role:      "portal-secrets",
		AuthMount: "kubernetes",
		KVMount:   "kv",
		TokenPath: "/var/run/secrets/vault/token",
	}
}

const (
	vaultTimeout    = 10 * time.Second
	maxVaultReply   = 64 << 10 // replies we parse are small; anything bigger is cut off
	maxJWT          = 16 << 10
	tokenSafety     = 30 * time.Second // re-login this long before the token expires
	defaultTokenTTL = 5 * time.Minute  // used when Vault reports no lease duration
)

// ErrVault is returned for every Vault failure. It never carries the reply body, the token or
// any value: callers map it to a generic 502.
var ErrVault = errors.New("vault request failed")

var mountRe = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)

// VaultClient talks to Vault over plain HTTP(S). It logs in with the pod's projected
// ServiceAccount token (re-read on every login, the kubelet rotates it), caches the Vault token
// until shortly before it expires and logs in again once when Vault answers 403.
type VaultClient struct {
	cfg  VaultConfig
	base *url.URL
	http *http.Client
	now  func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

func NewVaultClient(cfg VaultConfig) (*VaultClient, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Addr, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("VAULT_ADDR must be an http(s) URL without a path")
	}
	if cfg.Role == "" || !mountRe.MatchString(cfg.AuthMount) || !mountRe.MatchString(cfg.KVMount) || cfg.TokenPath == "" {
		return nil, errors.New("invalid Vault role, auth mount, KV mount or token path")
	}
	return &VaultClient{
		cfg:  cfg,
		base: u,
		now:  time.Now,
		http: &http.Client{
			Timeout: vaultTimeout,
			// a redirect could send the token elsewhere: never follow one
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// endpoint builds base + /v1/ + the escaped segments. Mount names are split on "/" (they were
// validated); every other segment is escaped on its own so it cannot add path elements.
func (c *VaultClient) endpoint(mount string, segs ...string) string {
	parts := []string{"v1"}
	for _, m := range strings.Split(mount, "/") {
		parts = append(parts, url.PathEscape(m))
	}
	for _, s := range segs {
		parts = append(parts, url.PathEscape(s))
	}
	return c.base.String() + "/" + strings.Join(parts, "/")
}

// do sends one request and decodes a JSON reply into out (if out is non-nil and the status is
// 2xx). It returns the status code; transport errors come back as ErrVault without detail.
func (c *VaultClient) do(ctx context.Context, method, endpoint, token string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, ErrVault
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rd)
	if err != nil {
		return 0, ErrVault
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// the error text holds the URL only (no token, no body), but keep it generic anyway
		return 0, fmt.Errorf("%w: transport", ErrVault)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxVaultReply))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode/100 == 2 && out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxVaultReply)).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("%w: unreadable reply", ErrVault)
		}
	}
	return resp.StatusCode, nil
}

func (c *VaultClient) login(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expires) {
		return c.token, nil
	}
	jwt, err := readJWT(c.cfg.TokenPath)
	if err != nil {
		return "", err
	}
	var reply struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int64  `json:"lease_duration"`
		} `json:"auth"`
	}
	st, err := c.do(ctx, http.MethodPost, c.endpoint("auth/"+c.cfg.AuthMount, "login"), "",
		map[string]string{"role": c.cfg.Role, "jwt": jwt}, &reply)
	if err != nil {
		return "", err
	}
	if st != http.StatusOK || reply.Auth.ClientToken == "" {
		return "", fmt.Errorf("%w: login status %d", ErrVault, st)
	}
	ttl := time.Duration(reply.Auth.LeaseDuration) * time.Second
	if ttl <= 0 {
		ttl = defaultTokenTTL
	}
	// stop using the token shortly before it expires: 30 s early, or 10% early for short tokens
	ttl -= min(tokenSafety, ttl/10)
	c.token, c.expires = reply.Auth.ClientToken, c.now().Add(ttl)
	return c.token, nil
}

func (c *VaultClient) forget(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token = ""
	}
}

func readJWT(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%w: service account token unreadable", ErrVault)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxJWT+1))
	if err != nil || len(b) > maxJWT {
		return "", fmt.Errorf("%w: service account token unreadable", ErrVault)
	}
	jwt := strings.TrimSpace(string(b))
	if jwt == "" {
		return "", fmt.Errorf("%w: service account token empty", ErrVault)
	}
	return jwt, nil
}

// call runs one authenticated request and retries it once with a fresh login when Vault answers
// 403 (the cached token may have been revoked or expired early).
func (c *VaultClient) call(ctx context.Context, method, endpoint string, body, out any) (int, error) {
	for attempt := 0; ; attempt++ {
		tok, err := c.login(ctx)
		if err != nil {
			return 0, err
		}
		st, err := c.do(ctx, method, endpoint, tok, body, out)
		if err != nil {
			return st, err
		}
		if st == http.StatusForbidden && attempt == 0 {
			c.forget(tok)
			continue
		}
		return st, nil
	}
}

func (c *VaultClient) Write(ctx context.Context, ns, name string, data map[string]string) (int, error) {
	if !validPathSegment(ns) || !validPathSegment(name) {
		return 0, fmt.Errorf("%w: invalid path", ErrVault)
	}
	var reply struct {
		Data struct {
			Version int `json:"version"`
		} `json:"data"`
	}
	// No CAS: check-and-set needs the current version, which only a read of the metadata could
	// give; a whole-secret replace is what the caller asked for anyway.
	st, err := c.call(ctx, http.MethodPut, c.endpoint(c.cfg.KVMount, "data", ns, name), map[string]any{"data": data}, &reply)
	if err != nil {
		return 0, err
	}
	if st != http.StatusOK || reply.Data.Version < 1 {
		return 0, fmt.Errorf("%w: write status %d", ErrVault, st)
	}
	return reply.Data.Version, nil
}

func (c *VaultClient) DeleteAll(ctx context.Context, ns, name string) error {
	if !validPathSegment(ns) || !validPathSegment(name) {
		return fmt.Errorf("%w: invalid path", ErrVault)
	}
	st, err := c.call(ctx, http.MethodDelete, c.endpoint(c.cfg.KVMount, "metadata", ns, name), nil, nil)
	if err != nil {
		return err
	}
	if st != http.StatusNoContent && st != http.StatusOK && st != http.StatusNotFound {
		return fmt.Errorf("%w: delete status %d", ErrVault, st)
	}
	return nil
}
