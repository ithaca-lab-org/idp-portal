package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

// Name contract with idp-gitops (workloads/ingress/gateway.yaml: Gateway
// istio-ingress/ingress, listener https, allowedRoutes selector
// LabelGatewayAccess) and with the Vault config Job (Kubernetes-auth role
// workload-reader). Pinned by the admission policy idp-operator-scope; change
// them together.
const (
	GatewayNamespace   = "istio-ingress"
	GatewayName        = "ingress"
	GatewaySectionName = "https"
	// GatewayServiceAccount is the account Istio's gateway deployment controller
	// gives the pods of Gateway "ingress" with class "istio" (<gateway>-<class>).
	GatewayServiceAccount = "ingress-istio"
	// WorkloadVaultRole is the Vault Kubernetes-auth role VSO logs in with for
	// a workload; its templated policy reads kv/data/<ns>/<sa name>/* only.
	WorkloadVaultRole = "workload-reader"

	// LabelWorkload marks the operator's children of a WorkloadIdentity and is
	// the pod label the AuthorizationPolicy selects: pods must carry
	// idp.ithaca-idp.com/workload=<name> to be covered by it.
	LabelWorkload = "idp.ithaca-idp.com/workload"

	// WorkloadVaultFinalizerName holds a deleted WorkloadIdentity (VAULT_ENABLED
	// only) until kv/<ns>/<name>/ is purged, so a later workload of that name
	// does not inherit its secrets.
	WorkloadVaultFinalizerName = "idp.ithaca-idp.com/workload-vault-purge"

	DefaultTrustDomain = "cluster.local"
	// DefaultHostnameSuffix is pinned by the admission policy: a different
	// suffix makes every HTTPRoute write fail until the policy changes too.
	DefaultHostnameSuffix = "apps.ithaca-idp.com"
)

var (
	HTTPRouteGVK           = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}
	AuthorizationPolicyGVK = schema.GroupVersionKind{Group: "security.istio.io", Version: "v1", Kind: "AuthorizationPolicy"}

	// Same shape as the trust-domain check in the admission policy.
	trustDomainRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
)

// WorkloadIdentityOptions are the platform-fixed parts of an identity.
type WorkloadIdentityOptions struct {
	// TrustDomain is Istio's mesh trust domain (meshConfig.trustDomain).
	TrustDomain string
	// HostnameSuffix follows <name>.<namespace>. in the reserved hostname.
	HostnameSuffix string
}

func (o WorkloadIdentityOptions) Validate() error {
	if len(o.TrustDomain) > 253 || !trustDomainRe.MatchString(o.TrustDomain) {
		return fmt.Errorf("trust domain %q is not a lowercase DNS name", o.TrustDomain)
	}
	if errs := validation.IsDNS1123Subdomain(o.HostnameSuffix); len(errs) > 0 {
		return fmt.Errorf("hostname suffix %q: %v", o.HostnameSuffix, errs)
	}
	return nil
}

func (o WorkloadIdentityOptions) spiffeID(ns, name string) string {
	return "spiffe://" + o.TrustDomain + "/ns/" + ns + "/sa/" + name
}

func (o WorkloadIdentityOptions) hostname(ns, name string) string {
	return name + "." + ns + "." + o.HostnameSuffix
}

func newUnstructured(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

// desiredHTTPRouteSpec reserves the hostname on the https listener of the
// shared Gateway. The single rule has no backendRefs and no filters, so it
// cannot forward anywhere: the Gateway API spec requires a 500 for traffic
// matching a rule whose backendRefs are all invalid (vacuously true here) and
// that has no filters; Istio is expected to answer with a direct 500 (not yet
// observed on the cluster). Defaulted fields (parentRef group/kind, the
// PathPrefix "/" match) are written explicitly so the stored object compares
// equal.
func desiredHTTPRouteSpec(o WorkloadIdentityOptions, ns, name string) map[string]any {
	return map[string]any{
		"parentRefs": []any{map[string]any{
			"group":       "gateway.networking.k8s.io",
			"kind":        "Gateway",
			"namespace":   GatewayNamespace,
			"name":        GatewayName,
			"sectionName": GatewaySectionName,
		}},
		"hostnames": []any{o.hostname(ns, name)},
		"rules": []any{map[string]any{
			"matches": []any{map[string]any{
				"path": map[string]any{"type": "PathPrefix", "value": "/"},
			}},
		}},
	}
}

// desiredAuthorizationPolicySpec allows, at L4, only identities of the same
// namespace and the ingress gateway to reach pods labelled
// LabelWorkload=<name>. Ambient without a waypoint: ztunnel enforces it, and
// it understands only L4 attributes, so there are no HTTP fields. A
// ServiceAccount cannot be a policy target in Istio 1.27 (targetRefs takes
// Gateway, GatewayClass, Service and ServiceEntry, and those are enforced by
// waypoints, not ztunnel), so the workload is selected by pod label.
func desiredAuthorizationPolicySpec(o WorkloadIdentityOptions, ns, name string) map[string]any {
	return map[string]any{
		"selector": map[string]any{"matchLabels": map[string]any{LabelWorkload: name}},
		"action":   "ALLOW",
		"rules": []any{map[string]any{
			"from": []any{
				map[string]any{"source": map[string]any{"namespaces": []any{ns}}},
				map[string]any{"source": map[string]any{"principals": []any{
					o.TrustDomain + "/ns/" + GatewayNamespace + "/sa/" + GatewayServiceAccount,
				}}},
			},
		}},
	}
}

// WorkloadIdentityReconciler turns a WorkloadIdentity into a ServiceAccount,
// AuthorizationPolicy, HTTPRoute and (with Vault on) a VaultAuth, all named
// like it and controlled by it.
type WorkloadIdentityReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Options WorkloadIdentityOptions
	// Vault is nil unless VAULT_ENABLED=true; then no VaultAuth is created.
	Vault *VaultOptions
}

// Child names follow the WorkloadIdentities, so update cannot be pinned with
// resourceNames; the admission policy confines it. ServiceAccounts are only
// created (create is already granted for vault-reader). No delete: garbage
// collection removes the children with their WorkloadIdentity.
//
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=workloadidentities,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=workloadidentities/status,verbs=get;update
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=workloadidentities/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=security.istio.io,resources=authorizationpolicies,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=secrets.hashicorp.com,resources=vaultauths,verbs=get;list;watch;create;update

func (r *WorkloadIdentityReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	wi := &idpv1alpha1.WorkloadIdentity{}
	if err := r.Get(ctx, req.NamespacedName, wi); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !wi.DeletionTimestamp.IsZero() {
		// Owner references remove the children; only the Vault data needs us.
		if controllerutil.ContainsFinalizer(wi, WorkloadVaultFinalizerName) {
			return r.finalizeVault(ctx, wi)
		}
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)

	managed, why, err := namespaceManaged(ctx, r.Client, wi.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !managed {
		log.Info("ignoring WorkloadIdentity outside an operator-managed namespace", "reason", why)
		return ctrl.Result{}, r.setSynced(ctx, wi, metav1.ConditionFalse, ReasonNamespaceNotManaged, why)
	}

	// The finalizer is in place before the VaultAuth can grant access to
	// kv/<ns>/<name>/.
	if r.Vault != nil && controllerutil.AddFinalizer(wi, WorkloadVaultFinalizerName) {
		if err := r.Update(ctx, wi); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The AuthorizationPolicy is created before the ServiceAccount. It selects
	// pods by the label LabelWorkload, not by ServiceAccount, so it covers only
	// pods that carry that label, whenever they start.
	for _, c := range r.children(wi) {
		res, reason, msg, err := r.ensureChild(ctx, wi, c)
		switch {
		case err != nil:
			_ = r.setSynced(ctx, wi, metav1.ConditionFalse, ReasonSyncError, err.Error())
			return ctrl.Result{}, fmt.Errorf("%s %s: %w", c.kind, wi.Name, err)
		case reason != "":
			log.Info("not touching existing child", "kind", c.kind, "reason", reason)
			return res, r.setSynced(ctx, wi, metav1.ConditionFalse, reason, msg)
		case !res.IsZero():
			return res, nil
		}
	}
	msg := "ServiceAccount, AuthorizationPolicy and HTTPRoute are in place"
	if r.Vault != nil {
		msg = "ServiceAccount, AuthorizationPolicy, HTTPRoute and VaultAuth are in place"
	}
	return ctrl.Result{}, r.setSynced(ctx, wi, metav1.ConditionTrue, ReasonSynced, msg)
}

// wiChild is one object the operator keeps for a WorkloadIdentity. set writes
// the desired spec; update=false means the object is only ever created.
type wiChild struct {
	kind   string
	obj    client.Object
	set    func(client.Object)
	update bool
}

func setUnstructuredSpec(want map[string]any) func(client.Object) {
	return func(o client.Object) {
		u := o.(*unstructured.Unstructured)
		if !equality.Semantic.DeepEqual(u.Object["spec"], want) {
			u.Object["spec"] = want
		}
	}
}

func (r *WorkloadIdentityReconciler) children(wi *idpv1alpha1.WorkloadIdentity) []wiChild {
	ns, name := wi.Namespace, wi.Name
	cs := []wiChild{
		{kind: "AuthorizationPolicy", obj: newUnstructured(AuthorizationPolicyGVK, ns, name),
			set: setUnstructuredSpec(desiredAuthorizationPolicySpec(r.Options, ns, name)), update: true},
		// Pods get no token by default; Istio ambient and VSO do not need one
		// mounted (pods may still opt in).
		{kind: "ServiceAccount", obj: &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}},
			set: func(o client.Object) { o.(*corev1.ServiceAccount).AutomountServiceAccountToken = ptr.To(false) }},
		{kind: "HTTPRoute", obj: newUnstructured(HTTPRouteGVK, ns, name),
			set: setUnstructuredSpec(desiredHTTPRouteSpec(r.Options, ns, name)), update: true},
	}
	if r.Vault != nil {
		cs = append(cs, wiChild{kind: "VaultAuth", obj: newVaultAuthNamed(ns, name),
			set: setUnstructuredSpec(vaultAuthSpec(WorkloadVaultRole, name)), update: true})
	}
	return cs
}

// ensureChild creates the child, or brings one controlled by this very
// WorkloadIdentity back to the desired state. Anything else of that name is
// left alone (childBlocked): a non-empty reason is reported as Synced=False.
func (r *WorkloadIdentityReconciler) ensureChild(ctx context.Context, wi *idpv1alpha1.WorkloadIdentity, c wiChild) (ctrl.Result, string, string, error) {
	cur := c.obj.DeepCopyObject().(client.Object)
	err := r.Get(ctx, client.ObjectKeyFromObject(c.obj), cur)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.setChild(wi, c, c.obj); err != nil {
			return ctrl.Result{}, "", "", err
		}
		if err := r.Create(ctx, c.obj); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ctrl.Result{RequeueAfter: time.Second}, "", "", nil
			}
			return ctrl.Result{}, "", "", err
		}
		return ctrl.Result{}, "", "", nil
	case err != nil:
		return ctrl.Result{}, "", "", err
	}
	if res, reason, msg := childBlocked(wi, "WorkloadIdentity", LabelWorkload, cur, c.kind); reason != "" {
		return res, reason, msg, nil
	}
	if !c.update {
		return ctrl.Result{}, "", "", nil
	}
	before := cur.DeepCopyObject()
	if err := r.setChild(wi, c, cur); err != nil {
		return ctrl.Result{}, "", "", err
	}
	if equality.Semantic.DeepEqual(before, cur) {
		return ctrl.Result{}, "", "", nil
	}
	return ctrl.Result{}, "", "", r.Update(ctx, cur)
}

// setChild writes the operator's labels, spec and controller reference, and
// drops all annotations (the admission policy refuses any, e.g. Istio's
// istio.io/dry-run, which would turn the AuthorizationPolicy off).
func (r *WorkloadIdentityReconciler) setChild(wi *idpv1alpha1.WorkloadIdentity, c wiChild, obj client.Object) error {
	obj.SetAnnotations(nil)
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, map[string]string{LabelManagedBy: ManagedByValue, LabelWorkload: wi.Name})
	obj.SetLabels(labels)
	c.set(obj)
	return controllerutil.SetControllerReference(wi, obj, r.Scheme)
}

// setSynced records the condition plus the identity and hostname, which follow
// from the namespace, name and operator flags whatever the outcome.
func (r *WorkloadIdentityReconciler) setSynced(ctx context.Context, wi *idpv1alpha1.WorkloadIdentity, status metav1.ConditionStatus, reason, msg string) error {
	before := wi.Status.DeepCopy()
	wi.Status.ObservedGeneration = wi.Generation
	wi.Status.SpiffeID = r.Options.spiffeID(wi.Namespace, wi.Name)
	wi.Status.Hostname = r.Options.hostname(wi.Namespace, wi.Name)
	meta.SetStatusCondition(&wi.Status.Conditions, metav1.Condition{
		Type: idpv1alpha1.ConditionSynced, Status: status, Reason: reason, Message: msg, ObservedGeneration: wi.Generation,
	})
	if equality.Semantic.DeepEqual(before, &wi.Status) {
		return nil
	}
	return r.Status().Update(ctx, wi)
}

// finalizeVault purges kv/<ns>/<name>/ and then drops the finalizer, with the
// TeamNamespace purge's retry, backoff and give-up (VAULT_PURGE_GIVE_UP_AFTER
// from the deletionTimestamp). The purge is skipped when a namespace of that
// name exists without the operator labels: the KV prefix belongs to its holder.
func (r *WorkloadIdentityReconciler) finalizeVault(ctx context.Context, wi *idpv1alpha1.WorkloadIdentity) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	switch {
	case r.Vault == nil:
		log.Info("Vault integration disabled; dropping the workload Vault purge finalizer without purging")
	case IsReserved(wi.Namespace):
		log.Info("reserved namespace; not purging Vault")
	default:
		ns := &corev1.Namespace{}
		if err := r.Get(ctx, types.NamespacedName{Name: wi.Namespace}, ns); err == nil &&
			(ns.Labels[LabelManagedBy] != ManagedByValue || ns.Labels[LabelTeamNamespace] != ns.Name) {
			log.Info("namespace is not managed by idp-operator; not purging Vault")
			break
		} else if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		err := r.Vault.purger().Purge(ctx, wi.Namespace, wi.Name)
		if errors.Is(err, ErrInvalidNamespace) || errors.Is(err, ErrInvalidSubPath) {
			log.Error(err, "not purging Vault for an invalid path")
			err = nil
		}
		if err != nil {
			elapsed := r.Vault.now().Sub(wi.DeletionTimestamp.Time)
			if elapsed < r.Vault.GiveUpAfter {
				log.Info("workload Vault purge failed, will retry", "error", err.Error())
				before := wi.Status.DeepCopy()
				meta.SetStatusCondition(&wi.Status.Conditions, metav1.Condition{
					Type: idpv1alpha1.ConditionVaultPurgePending, Status: metav1.ConditionTrue, Reason: ReasonVaultPurgeRetry,
					Message: fmt.Sprintf("purging kv/%s/%s/ failed, retrying until %s: %v", wi.Namespace, wi.Name,
						wi.DeletionTimestamp.Add(r.Vault.GiveUpAfter).UTC().Format(time.RFC3339), err),
					ObservedGeneration: wi.Generation,
				})
				if !equality.Semantic.DeepEqual(before, &wi.Status) {
					if serr := r.Status().Update(ctx, wi); serr != nil {
						return ctrl.Result{}, serr
					}
				}
				return ctrl.Result{RequeueAfter: purgeBackoff(elapsed, r.Vault.GiveUpAfter)}, nil
			}
			log.Error(err, "giving up on the workload Vault purge; KV data may remain in Vault",
				"giveUpAfter", r.Vault.GiveUpAfter.String())
		} else {
			log.Info("purged workload Vault KV data")
		}
	}
	controllerutil.RemoveFinalizer(wi, WorkloadVaultFinalizerName)
	return ctrl.Result{}, r.Update(ctx, wi)
}

// WorkloadIdentityCRDsMissing lists the kinds the controller watches that the
// API server does not serve (Gateway API HTTPRoute, Istio AuthorizationPolicy,
// and with Vault on VSO VaultAuth). The caller skips the controller then,
// rather than letting a failing watch stop the whole manager.
func WorkloadIdentityCRDsMissing(m meta.RESTMapper, vault bool) ([]string, error) {
	gvks := []schema.GroupVersionKind{HTTPRouteGVK, AuthorizationPolicyGVK}
	if vault {
		gvks = append(gvks, VaultAuthGVK)
	}
	var missing []string
	for _, gvk := range gvks {
		if _, err := m.RESTMapping(gvk.GroupKind(), gvk.Version); meta.IsNoMatchError(err) {
			missing = append(missing, gvk.String())
		} else if err != nil {
			return nil, err
		}
	}
	return missing, nil
}

// SetupWithManager watches HTTPRoutes and AuthorizationPolicies (and VaultAuths
// with Vault on); check WorkloadIdentityCRDsMissing first. Namespace and
// TeamNamespace events re-queue the WorkloadIdentities in that namespace, so
// one created before its namespace was managed syncs once it is.
func (r *WorkloadIdentityReconciler) SetupWithManager(mgr ctrl.Manager) error {
	inNamespace := handler.EnqueueRequestsFromMapFunc(r.requestsForNamespace)
	b := ctrl.NewControllerManagedBy(mgr).
		For(&idpv1alpha1.WorkloadIdentity{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(newUnstructured(AuthorizationPolicyGVK, "", "")).
		Owns(newUnstructured(HTTPRouteGVK, "", "")).
		Watches(&corev1.Namespace{}, inNamespace).
		Watches(&idpv1alpha1.TeamNamespace{}, inNamespace)
	if r.Vault != nil {
		b = b.Owns(newVaultAuthNamed("", ""))
	}
	return b.Named("workloadidentity").Complete(r)
}

// requestsForNamespace maps a Namespace or TeamNamespace (both named after the
// namespace) to the WorkloadIdentities in that namespace.
func (r *WorkloadIdentityReconciler) requestsForNamespace(ctx context.Context, o client.Object) []reconcile.Request {
	list := &idpv1alpha1.WorkloadIdentityList{}
	if err := r.List(ctx, list, client.InNamespace(o.GetName())); err != nil {
		logf.FromContext(ctx).Error(err, "listing WorkloadIdentities", "namespace", o.GetName())
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, wi := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: wi.Namespace, Name: wi.Name}})
	}
	return reqs
}
