package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

// Tenant certificate (ADR 0009, option 2b). A wildcard matches one label, so
// the platform Certificate istio-ingress/wildcard-apps (the single
// certificateRef of the Gateway) must also list *.<ns>.<base> for every managed
// namespace, or <workload>.<ns>.<base> fails TLS. The controller edits only
// spec.dnsNames of that one Certificate. Name contract with idp-gitops and the
// admission policy idp-operator-scope (it pins the namespace, the name and
// the allowed dnsNames); change them together.
const (
	DefaultTenantCertNamespace = "istio-ingress"
	DefaultTenantCertName      = "wildcard-apps"

	// MaxTenantCertNames is the number of tenant names: Let's Encrypt allows 100
	// names per certificate and one is the platform wildcard. The admission
	// policy caps the list at 100 entries.
	MaxTenantCertNames = 99

	// tenantCertAbsentRetry is the requeue delay while the Certificate is missing.
	tenantCertAbsentRetry = time.Minute
)

var (
	CertificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

	// tenantCertKey is the one request key every event maps to.
	tenantCertKey = reconcile.Request{NamespacedName: types.NamespacedName{Name: "tenant-cert"}}
)

// No kubebuilder RBAC marker on purpose: the Role in idp-gitops (namespace
// istio-ingress) carries get/list/watch and update/patch (resourceNames). The
// cache is limited to that namespace, so nothing is needed cluster-wide.

// TenantNamespace is a TeamNamespace as the certificate sees it.
type TenantNamespace struct {
	Name    string
	Created time.Time
}

// TenantCertOptions configure the TenantCertReconciler.
type TenantCertOptions struct {
	Namespace string
	Name      string
	// BaseDomain is the hostname suffix without a leading dot, for example
	// apps.ithaca-idp.com. Hostnames are <workload>.<ns>.<BaseDomain>.
	BaseDomain string
	// MaxNames caps the tenant names; zero means MaxTenantCertNames.
	MaxNames int
}

// TenantCertBaseDomain derives the base domain from --workload-hostname-suffix,
// which may be written with or without a leading dot.
func TenantCertBaseDomain(suffix string) (string, error) {
	base := strings.TrimPrefix(strings.TrimSpace(suffix), ".")
	if errs := validation.IsDNS1123Subdomain(base); len(errs) > 0 {
		return "", fmt.Errorf("hostname suffix %q: %v", suffix, errs)
	}
	return base, nil
}

func (o TenantCertOptions) Validate() error {
	if errs := validation.IsDNS1123Label(o.Namespace); len(errs) > 0 {
		return fmt.Errorf("tenant certificate namespace %q: %v", o.Namespace, errs)
	}
	if errs := validation.IsDNS1123Subdomain(o.Name); len(errs) > 0 {
		return fmt.Errorf("tenant certificate name %q: %v", o.Name, errs)
	}
	if _, err := TenantCertBaseDomain(o.BaseDomain); err != nil {
		return err
	}
	return nil
}

func (o TenantCertOptions) maxNames() int {
	if o.MaxNames > 0 {
		return o.MaxNames
	}
	return MaxTenantCertNames
}

// DesiredDNSNames returns the wildcard plus one name per namespace. When there
// are more than maxNames usable namespaces the oldest ones (creationTimestamp,
// ties by name) are kept, so a new namespace never evicts an older one; the kept
// names are then sorted for a stable list. Namespaces the admission policy
// would refuse (reserved, over 40 characters, not a DNS-1123 label) are skipped,
// since one refused name would block every update. skipped lists the namespaces
// dropped by the cap, invalid those dropped as unusable.
func (o TenantCertOptions) DesiredDNSNames(namespaces []TenantNamespace) (names, skipped, invalid []string) {
	var usable []TenantNamespace
	for _, ns := range namespaces {
		if len(ns.Name) > 40 || IsReserved(ns.Name) || len(validation.IsDNS1123Label(ns.Name)) > 0 {
			invalid = append(invalid, ns.Name)
			continue
		}
		usable = append(usable, ns)
	}
	slices.SortFunc(usable, func(a, b TenantNamespace) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	usable = slices.CompactFunc(usable, func(a, b TenantNamespace) bool { return a.Name == b.Name })
	var kept []string
	for i, ns := range usable {
		if i >= o.maxNames() {
			skipped = append(skipped, ns.Name)
			continue
		}
		kept = append(kept, ns.Name)
	}
	slices.Sort(kept)
	names = []string{"*." + o.BaseDomain}
	for _, ns := range kept {
		names = append(names, "*."+ns+"."+o.BaseDomain)
	}
	slices.Sort(skipped)
	slices.Sort(invalid)
	return names, skipped, invalid
}

// certSettled reports whether dnsNames may be changed now: not while an
// issuance runs (Issuing=True) or the last one failed (Ready present and not
// True). A Certificate without a Ready condition is fresh and is patched.
func certSettled(cert *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		st, _ := m["status"].(string)
		if (typ == "Issuing" && st == "True") || (typ == "Ready" && st != "True") {
			return false
		}
	}
	return true
}

// TenantCertReconciler keeps spec.dnsNames of one Certificate equal to the
// platform wildcard plus one wildcard per live TeamNamespace. It never creates
// or deletes the Certificate and writes nothing else.
type TenantCertReconciler struct {
	client.Client
	Options TenantCertOptions
}

// TenantCertCRDMissing reports whether the cert-manager Certificate CRD is not
// served. The caller skips the controller then, as for WorkloadIdentity.
func TenantCertCRDMissing(m meta.RESTMapper) (bool, error) {
	if _, err := m.RESTMapping(CertificateGVK.GroupKind(), CertificateGVK.Version); meta.IsNoMatchError(err) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return false, nil
}

func (o TenantCertOptions) certificate() *unstructured.Unstructured {
	return newUnstructured(CertificateGVK, o.Namespace, o.Name)
}

func (r *TenantCertReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("certificate", r.Options.Namespace+"/"+r.Options.Name)

	cert := r.Options.certificate()
	if err := r.Get(ctx, client.ObjectKeyFromObject(cert), cert); apierrors.IsNotFound(err) {
		log.Info("tenant certificate not found; it is never created here, retrying", "after", tenantCertAbsentRetry.String())
		return ctrl.Result{RequeueAfter: tenantCertAbsentRetry}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}

	list := &idpv1alpha1.TeamNamespaceList{}
	if err := r.List(ctx, list); err != nil {
		return ctrl.Result{}, err
	}
	var live []TenantNamespace
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp == nil {
			live = append(live, TenantNamespace{Name: list.Items[i].Name, Created: list.Items[i].CreationTimestamp.Time})
		}
	}
	want, skipped, invalid := r.Options.DesiredDNSNames(live)
	if len(invalid) > 0 {
		log.Error(nil, "TeamNamespaces without a usable certificate name were skipped", "namespaces", invalid)
	}
	if len(skipped) > 0 {
		log.Error(nil, "tenant certificate name cap reached: these namespaces get no TLS name until others are removed",
			"cap", r.Options.maxNames(), "skippedCount", len(skipped), "skipped", skipped)
	}

	have, _, err := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	if err != nil {
		// Unexpected shape: overwrite it with the desired list.
		have = nil
	}
	if equality.Semantic.DeepEqual(have, want) {
		return ctrl.Result{}, nil
	}

	// Every change reissues the certificate. While one is running or failing,
	// wait: the list is recomputed on the next pass, so changes made meanwhile
	// are coalesced into one patch. Not implemented: falling back to the last
	// Ready list.
	if !certSettled(cert) {
		log.Info("tenant certificate is not Ready or is issuing; dnsNames change deferred", "after", tenantCertAbsentRetry.String())
		return ctrl.Result{RequeueAfter: tenantCertAbsentRetry}, nil
	}

	// A merge patch of spec.dnsNames alone (a list is replaced whole): no other
	// field is sent, and without a resourceVersion it cannot fail on a stale cache.
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"dnsNames": want}})
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Patch(ctx, cert, client.RawPatch(types.MergePatchType, patch),
		client.FieldOwner("idp-operator")); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: tenantCertAbsentRetry}, nil
		}
		return ctrl.Result{}, fmt.Errorf("patching dnsNames: %w", err)
	}
	log.Info("updated tenant certificate dnsNames", "count", len(want), "was", len(have))
	return ctrl.Result{}, nil
}

// SetupWithManager watches TeamNamespaces (create, delete, start of deletion)
// and the one Certificate (drift); every event maps to the same request.
// Check TenantCertCRDMissing first.
func (r *TenantCertReconciler) SetupWithManager(mgr ctrl.Manager) error {
	one := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{tenantCertKey}
	})
	tnChanges := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return (e.ObjectOld.GetDeletionTimestamp() == nil) != (e.ObjectNew.GetDeletionTimestamp() == nil)
		},
	}
	isCert := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == r.Options.Namespace && o.GetName() == r.Options.Name
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("tenantcert").
		Watches(&idpv1alpha1.TeamNamespace{}, one, builder.WithPredicates(tnChanges)).
		Watches(r.Options.certificate(), one, builder.WithPredicates(isCert)).
		Complete(r)
}
