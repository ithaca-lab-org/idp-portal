package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCAFile(t *testing.T, der []byte) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(f, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// unrelatedCA is a self-signed CA that did not sign the test server's certificate.
func unrelatedCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unrelated-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// newTLSFakeVault is newFakeVault behind HTTPS (httptest's self-signed certificate).
func newTLSFakeVault(t *testing.T, keys ...string) *fakeVault {
	fv := newFakeVault(t, keys...)
	fv.srv.Close()
	fv.srv = httptest.NewTLSServer(http.HandlerFunc(fv.serve))
	t.Cleanup(fv.srv.Close)
	return fv
}

func TestPurgeOverTLSTrustsOnlyTheConfiguredCA(t *testing.T) {
	fv := newTLSFakeVault(t, "team-a/app/db", "team-b/app/db")
	envOf := func(ca string) func(string) string {
		m := map[string]string{"VAULT_ENABLED": "true", "VAULT_ADDR": fv.srv.URL, "VAULT_CACERT": ca}
		return func(k string) string { return m[k] }
	}

	o, err := VaultOptionsFromEnv(envOf(writeCAFile(t, fv.srv.Certificate().Raw)))
	if err != nil || o.RootCAs == nil {
		t.Fatalf("options: %v %v", o, err)
	}
	o.TokenPath = fv.options(t).TokenPath
	if err := newHTTPPurger(o).Purge(context.Background(), "team-a"); err != nil {
		t.Fatalf("purge with the server's CA: %v", err)
	}
	if _, remaining := fv.snapshot(); len(remaining) != 1 || remaining[0] != "team-b/app/db" {
		t.Fatalf("remaining = %v", remaining)
	}

	// A different CA must not verify the server: the file replaces the system roots.
	o, err = VaultOptionsFromEnv(envOf(writeCAFile(t, unrelatedCA(t))))
	if err != nil {
		t.Fatal(err)
	}
	o.TokenPath = fv.options(t).TokenPath
	if err := newHTTPPurger(o).Purge(context.Background(), "team-b"); err == nil {
		t.Fatal("purge succeeded against a server the configured CA did not sign")
	}

	// No VAULT_CACERT: system roots, which do not trust httptest's certificate.
	o, err = VaultOptionsFromEnv(envOf(""))
	if err != nil || o.RootCAs != nil {
		t.Fatalf("options without CA: %v %v", o, err)
	}
	o.TokenPath = fv.options(t).TokenPath
	if err := newHTTPPurger(o).Purge(context.Background(), "team-b"); err == nil {
		t.Fatal("purge succeeded against a self-signed server without VAULT_CACERT")
	}
	if _, remaining := fv.snapshot(); len(remaining) != 1 {
		t.Fatalf("an untrusted connection deleted data: %v", remaining)
	}
}

func TestVaultCACertOptionErrors(t *testing.T) {
	ca := writeCAFile(t, unrelatedCA(t))
	junk := filepath.Join(t.TempDir(), "junk")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string]map[string]string{
		"http address": {"VAULT_ADDR": "http://vault:8200", "VAULT_CACERT": ca},
		"default http": {"VAULT_CACERT": ca},
		"missing file": {"VAULT_ADDR": "https://vault:8200", "VAULT_CACERT": filepath.Join(t.TempDir(), "missing")},
		"no PEM":       {"VAULT_ADDR": "https://vault:8200", "VAULT_CACERT": junk},
	} {
		env["VAULT_ENABLED"] = "true"
		if _, err := VaultOptionsFromEnv(func(k string) string { return env[k] }); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
