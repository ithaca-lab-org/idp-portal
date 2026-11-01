package secrets

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writePEM writes cert as a PEM file and returns its path.
func writePEM(t *testing.T, der []byte) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(f, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// otherCA returns a self-signed CA certificate that did not sign the test server's certificate.
func otherCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func tlsVault(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/kubernetes/login" {
			_, _ = io.WriteString(w, `{"auth":{"client_token":"tok","lease_duration":600}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"version":1}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func tlsClient(t *testing.T, addr, caFile string) (*VaultClient, error) {
	t.Helper()
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultVaultConfig()
	cfg.Addr, cfg.TokenPath, cfg.CACertFile = addr, tok, caFile
	return NewVaultClient(cfg)
}

func TestVaultTrustsOnlyTheConfiguredCA(t *testing.T) {
	srv := tlsVault(t)
	ctx := context.Background()

	c, err := tlsClient(t, srv.URL, writePEM(t, srv.Certificate().Raw))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := c.Write(ctx, "team-a", "db", map[string]string{"A": "b"}); err != nil || v != 1 {
		t.Fatalf("with the server's CA: %d, %v", v, err)
	}

	// A different CA must not verify the server: the configured file replaces the system roots.
	c, err = tlsClient(t, srv.URL, writePEM(t, otherCA(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(ctx, "team-a", "db", map[string]string{"A": "b"}); !errors.Is(err, ErrVault) {
		t.Fatalf("untrusted server accepted: %v", err)
	}

	// No CA file: system roots only, which do not include the test server's certificate.
	c, err = tlsClient(t, srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(ctx, "team-a", "db", map[string]string{"A": "b"}); !errors.Is(err, ErrVault) {
		t.Fatalf("self-signed server accepted without VAULT_CACERT: %v", err)
	}
}

func TestVaultCACertConfigErrors(t *testing.T) {
	ca := writePEM(t, otherCA(t))
	if _, err := tlsClient(t, "http://vault:8200", ca); err == nil {
		t.Error("accepted VAULT_CACERT with an http address")
	}
	if _, err := tlsClient(t, "https://vault:8200", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("accepted a missing CA file")
	}
	junk := filepath.Join(t.TempDir(), "junk")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tlsClient(t, "https://vault:8200", junk); err == nil {
		t.Error("accepted a CA file without a PEM certificate")
	}
	if _, err := tlsClient(t, "https://vault:8200", ca); err != nil {
		t.Errorf("valid CA file rejected: %v", err)
	}
	if d := DefaultVaultConfig(); !strings.HasPrefix(d.Addr, "https://") {
		t.Errorf("default address %q is not https", d.Addr)
	}
}
