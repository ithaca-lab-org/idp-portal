package secrets

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var managedSecretGVR = schema.GroupVersionResource{Group: "idp.ithaca-idp.com", Version: "v1alpha1", Resource: "managedsecrets"}

// Cluster is the portal's view of ManagedSecret resources. The resource carries no data: it only
// tells the operator that kv/<ns>/<name> should be synced into the namespace. The portal's
// ServiceAccount needs create, get and delete on managedsecrets and nothing else.
type Cluster interface {
	// EnsureManagedSecret creates the resource; an existing one is fine. A name the CRD rejects
	// gives an ErrInvalid error.
	EnsureManagedSecret(ctx context.Context, ns, name string) error
	// DeleteManagedSecret deletes the resource; a missing one is fine.
	DeleteManagedSecret(ctx context.Context, ns, name string) error
}

type DynamicCluster struct{ Client dynamic.Interface }

func (c DynamicCluster) EnsureManagedSecret(ctx context.Context, ns, name string) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "idp.ithaca-idp.com/v1alpha1",
		"kind":       "ManagedSecret",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       map[string]any{},
	}}
	_, err := c.Client.Resource(managedSecretGVR).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		return nil
	case apierrors.IsInvalid(err):
		// the CRD's validation rejected the name; the API's own check should have caught it
		return invalid("invalid name")
	}
	return err
}

func (c DynamicCluster) DeleteManagedSecret(ctx context.Context, ns, name string) error {
	err := c.Client.Resource(managedSecretGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
