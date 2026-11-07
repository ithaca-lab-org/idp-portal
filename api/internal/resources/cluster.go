package resources

import (
	"context"
	"fmt"
	"regexp"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const group = "idp.ithaca-idp.com"

var (
	bucketClaimGVR      = schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "bucketclaims"}
	resourceGrantGVR    = schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "resourcegrants"}
	workloadIdentityGVR = schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "workloadidentities"}
)

// Cluster is the portal's view of BucketClaims and ResourceGrants. The portal's ServiceAccount
// needs get, list and create on bucketclaims, get, list, create and delete on resourcegrants, and
// get on workloadidentities (already granted for the Workloads page); no update, patch or watch.
// There is no bucket delete: see the package comment.
type Cluster interface {
	// CreateBucket creates the claim (empty spec); ErrBucketExists if it exists, ErrInvalid if
	// the CRD rejects the name.
	CreateBucket(ctx context.Context, ns, name string) error
	// GetBucket returns ErrNotFound if the claim does not exist.
	GetBucket(ctx context.Context, ns, name string) (*Bucket, error)
	ListBuckets(ctx context.Context, ns string) ([]Bucket, error)
	// CreateGrant creates the grant. If the name is taken it reads that grant: ErrGrantExists when
	// it is for the same workload and bucket (a repeat), ErrGrantCollision when it is for another
	// pair. ErrInvalid if the CRD rejects it.
	CreateGrant(ctx context.Context, ns, name, workload, bucket, access string) error
	ListGrants(ctx context.Context, ns string) ([]Grant, error)
	// DeleteGrant returns ErrNotFound if the grant does not exist.
	DeleteGrant(ctx context.Context, ns, name string) error
	WorkloadExists(ctx context.Context, ns, name string) (bool, error)
}

type DynamicCluster struct{ Client dynamic.Interface }

func (c DynamicCluster) CreateBucket(ctx context.Context, ns, name string) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group + "/v1alpha1",
		"kind":       "BucketClaim",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       map[string]any{},
	}}
	_, err := c.Client.Resource(bucketClaimGVR).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		return ErrBucketExists
	case apierrors.IsInvalid(err):
		// the CRD's validation rejected the name; the API's own check should have caught it
		return invalid("invalid name")
	}
	return err
}

func (c DynamicCluster) GetBucket(ctx context.Context, ns, name string) (*Bucket, error) {
	obj, err := c.Client.Resource(bucketClaimGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get bucketclaim: %w", err)
	}
	return bucketFrom(obj), nil
}

func (c DynamicCluster) ListBuckets(ctx context.Context, ns string) ([]Bucket, error) {
	items, err := listAll(ctx, c.Client, bucketClaimGVR, ns)
	if err != nil {
		return nil, err
	}
	out := make([]Bucket, 0, len(items))
	for i := range items {
		out = append(out, *bucketFrom(&items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c DynamicCluster) CreateGrant(ctx context.Context, ns, name, workload, bucket, access string) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group + "/v1alpha1",
		"kind":       "ResourceGrant",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       map[string]any{"workload": workload, "resource": bucket, "access": access},
	}}
	_, err := c.Client.Resource(resourceGrantGVR).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		old, gerr := c.Client.Resource(resourceGrantGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return ErrGrantExists // it was deleted meanwhile or cannot be read: a plain duplicate, retry
		}
		w, _, _ := unstructured.NestedString(old.Object, "spec", "workload")
		b, _, _ := unstructured.NestedString(old.Object, "spec", "resource")
		if w != workload || b != bucket {
			return ErrGrantCollision
		}
		return ErrGrantExists
	case apierrors.IsInvalid(err):
		return invalid("invalid grant")
	}
	return err
}

func (c DynamicCluster) ListGrants(ctx context.Context, ns string) ([]Grant, error) {
	items, err := listAll(ctx, c.Client, resourceGrantGVR, ns)
	if err != nil {
		return nil, err
	}
	out := make([]Grant, 0, len(items))
	for i := range items {
		out = append(out, *grantFrom(&items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c DynamicCluster) DeleteGrant(ctx context.Context, ns, name string) error {
	err := c.Client.Resource(resourceGrantGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

func (c DynamicCluster) WorkloadExists(ctx context.Context, ns, name string) (bool, error) {
	_, err := c.Client.Resource(workloadIdentityGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get workloadidentity: %w", err)
	}
	return true, nil
}

// listAll follows the continue token so the answer (and the cap check built on it) is never
// silently truncated; more than maxListed objects fails closed.
func listAll(ctx context.Context, c dynamic.Interface, gvr schema.GroupVersionResource, ns string) ([]unstructured.Unstructured, error) {
	var out []unstructured.Unstructured
	opts := metav1.ListOptions{Limit: 100}
	for {
		l, err := c.Resource(gvr).Namespace(ns).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", gvr.Resource, err)
		}
		out = append(out, l.Items...)
		if len(out) > maxListed {
			return nil, fmt.Errorf("list %s: more than %d in %s", gvr.Resource, maxListed, ns)
		}
		if l.GetContinue() == "" {
			return out, nil
		}
		opts.Continue = l.GetContinue()
	}
}

// What the broker reports is never relayed as text: it can carry errors from Google. The page
// gets the reason (only if the broker's code knows it) and a message written here. A reason that
// is not listed, CreateFailed and GrantFailed included, gets the generic text.
const generic = "The platform could not complete this request. Ask the platform team."

var bucketReasons = map[string]string{
	"NamespaceNotManaged": "This namespace is not managed by the platform, so it cannot own buckets.",
	"InvalidName":         "This name cannot be used for a bucket here. Order another name.",
	"NameTaken":           "A bucket with the name derived from this claim already exists and is not owned by it. Order another name.",
	"Terminating":         "This bucket claim is being deleted.",
	"Orphaned":            "This bucket was retired with data in it and tenants can no longer reach it. Ask the platform team.",
	"CreateFailed":        generic,
}

var grantReasons = map[string]string{
	"NotManaged":       "This namespace is not managed by the platform.",
	"WorkloadNotFound": "The workload does not exist (any more).",
	"ClaimNotFound":    "The bucket does not exist (any more).",
	"ClaimNotReady":    "The bucket is not ready yet; access follows once it is.",
	"GrantFailed":      generic,
	"Revoking":         "Access is being removed.",
}

// bucketNameRe is the one name the broker gives this claim, idp-<namespace>-<claim>-<8 hex>;
// anything else in the status is not shown.
func bucketNameRe(ns, name string) *regexp.Regexp {
	return regexp.MustCompile(`^idp-` + regexp.QuoteMeta(ns) + `-` + regexp.QuoteMeta(name) + `-[0-9a-f]{8}$`)
}

// ready reads the Ready condition: status True/False/Unknown and its reason; absent is Unknown.
func ready(obj *unstructured.Unstructured) (status, reason string) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != "Ready" {
			continue
		}
		status, _ = m["status"].(string)
		reason, _ = m["reason"].(string)
		return status, reason
	}
	return "", ""
}

// phase maps Ready to the order status: no condition (or Unknown) is PENDING, True is COMPLETE
// and False is FAILED, except the reasons that clear by themselves (pending is listed by the
// caller). reasons is the allow-list for the kind; message is the portal's text, "" when Ready.
func phase(status, reason string, reasons map[string]string, pending ...string) (st, rs, msg string) {
	switch status {
	case "True":
		return StatusComplete, "", ""
	case "False":
		st = StatusFailed
		for _, p := range pending {
			if reason == p {
				st = StatusPending
			}
		}
		if m, ok := reasons[reason]; ok {
			return st, reason, m
		}
		return st, "Unknown", generic
	}
	return StatusPending, "", ""
}

func created(obj *unstructured.Unstructured) string {
	return obj.GetCreationTimestamp().UTC().Format("2006-01-02T15:04:05Z")
}

func bucketFrom(obj *unstructured.Unstructured) *Bucket {
	b := &Bucket{Namespace: obj.GetNamespace(), Name: obj.GetName(), CreatedAt: created(obj)}
	if n, _, _ := unstructured.NestedString(obj.Object, "status", "bucketName"); bucketNameRe(b.Namespace, b.Name).MatchString(n) {
		b.BucketName = n
	}
	st, rs := ready(obj)
	b.Status, b.Reason, b.Message = phase(st, rs, bucketReasons)
	return b
}

func grantFrom(obj *unstructured.Unstructured) *Grant {
	g := &Grant{Namespace: obj.GetNamespace(), Name: obj.GetName(), CreatedAt: created(obj)}
	g.Workload, _, _ = unstructured.NestedString(obj.Object, "spec", "workload")
	g.Bucket, _, _ = unstructured.NestedString(obj.Object, "spec", "resource")
	g.Access, _, _ = unstructured.NestedString(obj.Object, "spec", "access")
	if obj.GetDeletionTimestamp() != nil {
		g.Status, g.Reason, g.Message = StatusPending, "Revoking", grantReasons["Revoking"]
		return g
	}
	st, rs := ready(obj)
	// ClaimNotReady and WorkloadNotFound can clear without a change to the grant
	g.Status, g.Reason, g.Message = phase(st, rs, grantReasons, "ClaimNotReady")
	return g
}
