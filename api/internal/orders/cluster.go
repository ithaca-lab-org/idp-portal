package orders

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var teamNamespaceGVR = schema.GroupVersionResource{Group: "idp.ithaca-idp.com", Version: "v1alpha1", Resource: "teamnamespaces"}

// TeamNamespaceInfo is what the portal reads back from a TeamNamespace.
type TeamNamespaceInfo struct {
	Phase   string
	Message string
	OrderID string
}

// Cluster is the portal's only view of Kubernetes: create and read TeamNamespace resources.
type Cluster interface {
	// CreateTeamNamespace returns ErrNameTaken if the resource already exists.
	CreateTeamNamespace(ctx context.Context, name, owner, businessUnit, size, orderID string) error
	// TeamNamespace returns ErrNotFound if it does not exist.
	TeamNamespace(ctx context.Context, name string) (*TeamNamespaceInfo, error)
}

type DynamicCluster struct{ Client dynamic.Interface }

func (c DynamicCluster) CreateTeamNamespace(ctx context.Context, name, owner, businessUnit, size, orderID string) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "idp.ithaca-idp.com/v1alpha1",
		"kind":       "TeamNamespace",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"businessUnit": businessUnit,
			"owner":        owner,
			"size":         size,
			"orderId":      orderID,
		},
	}}
	_, err := c.Client.Resource(teamNamespaceGVR).Create(ctx, obj, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return ErrNameTaken
	}
	return err
}

func (c DynamicCluster) TeamNamespace(ctx context.Context, name string) (*TeamNamespaceInfo, error) {
	obj, err := c.Client.Resource(teamNamespaceGVR).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get teamnamespace: %w", err)
	}
	info := &TeamNamespaceInfo{}
	info.OrderID, _, _ = unstructured.NestedString(obj.Object, "spec", "orderId")
	info.Phase, _, _ = unstructured.NestedString(obj.Object, "status", "phase")
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == "Ready" {
			info.Message, _ = m["message"].(string)
		}
	}
	return info, nil
}
