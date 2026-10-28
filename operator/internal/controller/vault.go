package controller

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

// Name contract with idp-gitops (workloads/vault: Vault roles and the shared
// VaultConnection). Change these only together with that repo.
const (
	VaultFinalizerName = "idp.ithaca-idp.com/vault-purge"

	VaultReaderServiceAccount = "vault-reader"
	VaultAuthName             = "vault-reader"
	// VaultReaderRole is the Vault Kubernetes-auth role VSO logs in with; its
	// templated policy allows reading kv/data/<own namespace>/* only.
	VaultReaderRole = "ns-reader"
	VaultAudience   = "vault"
	// VaultConnectionRef is the shared VaultConnection, "<namespace>/<name>",
	// in the namespace VSO runs in.
	VaultConnectionRef = "vault-secrets-operator/idp-vault"

	ReasonVaultAccessReady  = "VaultAccessReady"
	ReasonVaultAccessError  = "VaultAccessError"
	ReasonVaultPurgeRetry   = "VaultPurgeRetrying"
	vaultTokenExpirationSec = 600
)

var VaultAuthGVK = schema.GroupVersionKind{Group: "secrets.hashicorp.com", Version: "v1beta1", Kind: "VaultAuth"}

// VaultOptions configures the Vault integration. A nil *VaultOptions on the
// reconciler means VAULT_ENABLED=false: no Vault objects and no Vault finalizer.
type VaultOptions struct {
	Addr      *url.URL
	PurgeRole string
	AuthMount string
	KVMount   string
	// TokenPath is a projected ServiceAccount token with audience "vault".
	TokenPath string
	// GiveUpAfter bounds how long a failing purge may block deletion, counted
	// from the TeamNamespace's deletionTimestamp.
	GiveUpAfter time.Duration
	// Now is the clock (tests override it); nil means time.Now.
	Now func() time.Time
	// Purger overrides the HTTP purger (nil means the real one).
	Purger Purger
}

// Purger removes all KV data for one namespace.
type Purger interface {
	Purge(ctx context.Context, namespace string) error
}

const (
	defaultVaultAddr        = "http://vault.vault.svc.cluster.local:8200"
	defaultVaultPurgeRole   = "idp-operator-purge"
	defaultVaultAuthMount   = "kubernetes"
	defaultVaultKVMount     = "kv"
	defaultVaultTokenPath   = "/var/run/secrets/vault/token"
	defaultVaultGiveUpAfter = 30 * time.Minute
)

var (
	mountRe = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)
	roleRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// VaultOptionsFromEnv reads VAULT_* settings. It returns nil, nil when
// VAULT_ENABLED is unset or false.
func VaultOptionsFromEnv(getenv func(string) string) (*VaultOptions, error) {
	env := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	enabled, err := strconv.ParseBool(env("VAULT_ENABLED", "false"))
	if err != nil {
		return nil, fmt.Errorf("VAULT_ENABLED: %w", err)
	}
	if !enabled {
		return nil, nil
	}
	addr, err := url.Parse(env("VAULT_ADDR", defaultVaultAddr))
	if err != nil || (addr.Scheme != "http" && addr.Scheme != "https") || addr.Host == "" ||
		addr.User != nil || addr.RawQuery != "" || addr.Fragment != "" {
		return nil, fmt.Errorf("VAULT_ADDR must be an http(s) URL without credentials, query or fragment")
	}
	o := &VaultOptions{
		Addr:      addr,
		PurgeRole: env("VAULT_PURGE_ROLE", defaultVaultPurgeRole),
		AuthMount: strings.Trim(env("VAULT_AUTH_MOUNT", defaultVaultAuthMount), "/"),
		KVMount:   strings.Trim(env("VAULT_KV_MOUNT", defaultVaultKVMount), "/"),
		TokenPath: env("VAULT_TOKEN_PATH", defaultVaultTokenPath),
	}
	if !roleRe.MatchString(o.PurgeRole) {
		return nil, fmt.Errorf("VAULT_PURGE_ROLE %q is not a valid role name", o.PurgeRole)
	}
	for k, v := range map[string]string{"VAULT_AUTH_MOUNT": o.AuthMount, "VAULT_KV_MOUNT": o.KVMount} {
		if !mountRe.MatchString(v) {
			return nil, fmt.Errorf("%s %q is not a valid mount path", k, v)
		}
	}
	o.GiveUpAfter, err = time.ParseDuration(env("VAULT_PURGE_GIVE_UP_AFTER", defaultVaultGiveUpAfter.String()))
	if err != nil || o.GiveUpAfter <= 0 {
		return nil, fmt.Errorf("VAULT_PURGE_GIVE_UP_AFTER must be a positive duration")
	}
	return o, nil
}

func (o *VaultOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *VaultOptions) purger() Purger {
	if o.Purger != nil {
		return o.Purger
	}
	return newHTTPPurger(o)
}

func newVaultAuth(namespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(VaultAuthGVK)
	u.SetNamespace(namespace)
	u.SetName(VaultAuthName)
	return u
}

// desiredVaultAuthSpec is the full spec the operator owns; VSO reads it to log
// in to Vault as vault-reader with role ns-reader.
func desiredVaultAuthSpec(authMount string) map[string]any {
	return map[string]any{
		"method":             "kubernetes",
		"mount":              authMount,
		"vaultConnectionRef": VaultConnectionRef,
		"kubernetes": map[string]any{
			"role":                   VaultReaderRole,
			"serviceAccount":         VaultReaderServiceAccount,
			"audiences":              []any{VaultAudience},
			"tokenExpirationSeconds": int64(vaultTokenExpirationSec),
		},
	}
}

func newVaultReaderSA(namespace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: VaultReaderServiceAccount, Namespace: namespace}}
}

// VSO requests short-lived tokens for vault-reader through the TokenRequest
// API, so no token is mounted into pods using this account.
func setVaultReaderSA(sa *corev1.ServiceAccount) {
	sa.AutomountServiceAccountToken = ptr.To(false)
}

func vaultCondition(status metav1.ConditionStatus, reason, msg string, tn *idpv1alpha1.TeamNamespace) metav1.Condition {
	return metav1.Condition{
		Type:               idpv1alpha1.ConditionVaultReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: tn.Generation,
	}
}
