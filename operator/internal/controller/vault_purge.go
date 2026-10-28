package controller

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
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	vaultRequestTimeout = 10 * time.Second
	vaultPurgeTimeout   = 2 * time.Minute
	vaultMaxBody        = 1 << 20
	vaultMaxJWT         = 64 << 10
	// A namespace holds kv/<ns>/<workload>/<secret>; depth and key caps bound a
	// hostile or corrupt tree. Each pass deletes what it reaches, so a tree above
	// the cap still shrinks on every retry.
	vaultMaxDepth = 8
	vaultMaxKeys  = 5000
)

var ErrInvalidNamespace = errors.New("namespace name is not a DNS-1123 label")

// httpPurger talks to the Vault HTTP API with net/http. Errors carry method,
// path and status only: never tokens, the JWT or response bodies.
type httpPurger struct {
	opts   *VaultOptions
	client *http.Client
}

func newHTTPPurger(o *VaultOptions) *httpPurger {
	return &httpPurger{opts: o, client: &http.Client{
		Timeout: vaultRequestTimeout,
		// A redirect would resend X-Vault-Token to wherever it points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Purge deletes kv/metadata/<namespace>/... recursively (all versions of every
// key). A namespace with no data (404) is a success.
func (p *httpPurger) Purge(ctx context.Context, namespace string) error {
	if len(validation.IsDNS1123Label(namespace)) != 0 {
		return ErrInvalidNamespace
	}
	ctx, cancel := context.WithTimeout(ctx, vaultPurgeTimeout)
	defer cancel()

	token, err := p.login(ctx)
	if err != nil {
		return err
	}
	defer p.revokeSelf(token)

	keys := 0
	return p.purgeDir(ctx, token, []string{namespace}, 1, &keys)
}

func (p *httpPurger) login(ctx context.Context) (string, error) {
	jwt, err := readJWT(p.opts.TokenPath)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]string{"role": p.opts.PurgeRole, "jwt": jwt})
	if err != nil {
		return "", err
	}
	segs := append(append([]string{"auth"}, strings.Split(p.opts.AuthMount, "/")...), "login")
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	status, err := p.do(ctx, http.MethodPost, segs, nil, "", bytes.NewReader(body), &out)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK || out.Auth.ClientToken == "" {
		return "", fmt.Errorf("vault login (role %s, mount %s): HTTP %d", p.opts.PurgeRole, p.opts.AuthMount, status)
	}
	return out.Auth.ClientToken, nil
}

func readJWT(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read Vault ServiceAccount token: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, vaultMaxJWT))
	if err != nil {
		return "", fmt.Errorf("read Vault ServiceAccount token: %w", err)
	}
	jwt := strings.TrimSpace(string(b))
	if jwt == "" {
		return "", errors.New("Vault ServiceAccount token file is empty")
	}
	return jwt, nil
}

// revokeSelf is best effort: the token has a short TTL anyway, and the purge
// role may not carry the default policy that allows revoke-self.
func (p *httpPurger) revokeSelf(token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = p.do(ctx, http.MethodPost, []string{"auth", "token", "revoke-self"}, nil, token, nil, nil)
}

func (p *httpPurger) purgeDir(ctx context.Context, token string, dir []string, depth int, keys *int) error {
	if depth > vaultMaxDepth {
		return fmt.Errorf("vault purge: %s is nested deeper than %d levels", strings.Join(dir, "/"), vaultMaxDepth)
	}
	var out struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	status, err := p.do(ctx, http.MethodGet, p.metadataPath(dir), url.Values{"list": {"true"}}, token, nil, &out)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNotFound:
		return nil
	case http.StatusOK:
	default:
		return fmt.Errorf("vault list %s/metadata/%s: HTTP %d", p.opts.KVMount, strings.Join(dir, "/"), status)
	}
	for _, k := range out.Data.Keys {
		name, isDir := strings.CutSuffix(k, "/")
		if !validKeySegment(name) {
			return fmt.Errorf("vault purge: refusing unexpected key %q under %s", k, strings.Join(dir, "/"))
		}
		child := append(append([]string{}, dir...), name)
		if isDir {
			if err := p.purgeDir(ctx, token, child, depth+1, keys); err != nil {
				return err
			}
			continue
		}
		if *keys >= vaultMaxKeys {
			return fmt.Errorf("vault purge: more than %d keys under %s, continuing on the next pass", vaultMaxKeys, dir[0])
		}
		*keys++
		status, err := p.do(ctx, http.MethodDelete, p.metadataPath(child), nil, token, nil, nil)
		if err != nil {
			return err
		}
		if status != http.StatusNoContent && status != http.StatusOK && status != http.StatusNotFound {
			return fmt.Errorf("vault delete %s/metadata/%s: HTTP %d", p.opts.KVMount, strings.Join(child, "/"), status)
		}
	}
	return nil
}

// validKeySegment rejects anything that could change the path once joined:
// empty, dot segments, separators, and control or escape characters.
func validKeySegment(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 512 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' || r == '%' || r == '?' || r == '#' {
			return false
		}
	}
	return true
}

func (p *httpPurger) metadataPath(segs []string) []string {
	out := strings.Split(p.opts.KVMount, "/")
	out = append(out, "metadata")
	return append(out, segs...)
}

// do sends one request to /v1/<segs...>. Each segment is escaped; the result is
// decoded into out only on 200. Response bodies are never returned or logged.
func (p *httpPurger) do(ctx context.Context, method string, segs []string, q url.Values, token string,
	body io.Reader, out any) (int, error) {
	u := *p.opts.Addr
	base := strings.TrimSuffix(u.EscapedPath(), "/")
	raw := make([]string, 0, len(segs)+1)
	plain := make([]string, 0, len(segs)+1)
	raw, plain = append(raw, "v1"), append(plain, "v1")
	for _, s := range segs {
		raw = append(raw, url.PathEscape(s))
		plain = append(plain, s)
	}
	u.RawPath = base + "/" + strings.Join(raw, "/")
	u.Path = strings.TrimSuffix(p.opts.Addr.Path, "/") + "/" + strings.Join(plain, "/")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return 0, fmt.Errorf("vault %s: %w", method, err)
	}
	req.Header.Set("X-Vault-Request", "true")
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		// url.Error includes the URL (no secrets in it) and the cause.
		return 0, fmt.Errorf("vault unreachable: %w", err)
	}
	defer resp.Body.Close()
	lr := io.LimitReader(resp.Body, vaultMaxBody)
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.NewDecoder(lr).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("vault %s %s: decode response: %w", method, u.Path, err)
		}
	}
	_, _ = io.Copy(io.Discard, lr)
	return resp.StatusCode, nil
}
