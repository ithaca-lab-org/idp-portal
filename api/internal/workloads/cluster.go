package workloads

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var workloadIdentityGVR = schema.GroupVersionResource{Group: "idp.ithaca-idp.com", Version: "v1alpha1", Resource: "workloadidentities"}

// Cluster is the portal's view of WorkloadIdentity resources. The resource has an empty spec;
// the operator fills in the status. The portal's ServiceAccount needs create, get and list on
// workloadidentities and nothing else (no update, no delete).
type Cluster interface {
	// Create creates the resource; ErrExists if it exists, ErrInvalid if the CRD rejects the name.
	Create(ctx context.Context, ns, name string) error
	// Get returns ErrNotFound if the resource does not exist.
	Get(ctx context.Context, ns, name string) (*Workload, error)
	List(ctx context.Context, ns string) ([]Workload, error)
}

type DynamicCluster struct{ Client dynamic.Interface }

func (c DynamicCluster) Create(ctx context.Context, ns, name string) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "idp.ithaca-idp.com/v1alpha1",
		"kind":       "WorkloadIdentity",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       map[string]any{},
	}}
	_, err := c.Client.Resource(workloadIdentityGVR).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		return ErrExists
	case apierrors.IsInvalid(err):
		// the CRD's validation rejected the name; the API's own check should have caught it
		return invalid("invalid name")
	}
	return err
}

func (c DynamicCluster) Get(ctx context.Context, ns, name string) (*Workload, error) {
	obj, err := c.Client.Resource(workloadIdentityGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get workloadidentity: %w", err)
	}
	return fromObject(obj), nil
}

func (c DynamicCluster) List(ctx context.Context, ns string) ([]Workload, error) {
	// follow the continue token so the answer (and the cap check built on it) is never silently
	// truncated; more than maxPerNamespace objects fails closed
	out := []Workload{}
	opts := metav1.ListOptions{Limit: 100}
	for {
		l, err := c.Client.Resource(workloadIdentityGVR).Namespace(ns).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list workloadidentities: %w", err)
		}
		for i := range l.Items {
			out = append(out, *fromObject(&l.Items[i]))
		}
		if len(out) > maxPerNamespace {
			return nil, fmt.Errorf("list workloadidentities: more than %d in %s", maxPerNamespace, ns)
		}
		if l.GetContinue() == "" {
			break
		}
		opts.Continue = l.GetContinue()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// fromObject reads what the operator reported; nothing is invented. The ServiceAccount is named
// like the workload (a CRD rule of the operator), so it is not read from the status.
func fromObject(obj *unstructured.Unstructured) *Workload {
	w := &Workload{Namespace: obj.GetNamespace(), Name: obj.GetName(), ServiceAccount: obj.GetName(),
		CreatedAt: obj.GetCreationTimestamp().UTC().Format("2006-01-02T15:04:05Z")}
	w.SpiffeID, _, _ = unstructured.NestedString(obj.Object, "status", "spiffeID")
	w.Hostname, _, _ = unstructured.NestedString(obj.Object, "status", "hostname")
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	w.Conditions = []Condition{}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		var cd Condition
		cd.Type, _ = m["type"].(string)
		cd.Status, _ = m["status"].(string)
		cd.Reason, _ = m["reason"].(string)
		cd.Message, _ = m["message"].(string)
		if cd.Reason == "SyncError" {
			// the operator's text can carry error details from other systems; show the reason only
			cd.Message = "The operator could not sync this workload. Ask the platform team."
		}
		cd.LastTransitionTime, _ = m["lastTransitionTime"].(string)
		w.Conditions = append(w.Conditions, cd)
	}
	w.Status, w.Reason = statusOf(w.Conditions)
	return w
}
