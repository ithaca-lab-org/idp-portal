package pipelines

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

const group = "idp.ithaca-idp.com"

var (
	pipelineGVR         = schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "deliverypipelines"}
	runGVR              = schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "deliveryruns"}
	workloadIdentityGVR = schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "workloadidentities"}
)

// Cluster is the portal's view of the delivery resources. The portal's ServiceAccount needs get,
// list and create on deliverypipelines and deliveryruns and get on workloadidentities; no update,
// no delete (the admission policy idp-portal-scope allows nothing else).
type Cluster interface {
	// CreatePipeline returns ErrExists if the pipeline exists, ErrInvalid if the CRD rejects it.
	CreatePipeline(ctx context.Context, p *Pipeline) error
	// GetPipeline returns ErrNotFound if it does not exist.
	GetPipeline(ctx context.Context, ns, name string) (*Pipeline, error)
	ListPipelines(ctx context.Context, ns string) ([]Pipeline, error)
	WorkloadExists(ctx context.Context, ns, name string) (bool, error)
	// CreateRun returns ErrExists on a name clash, ErrInvalid if the CRD rejects it.
	CreateRun(ctx context.Context, r *Run) error
	// ListRuns returns every run of the namespace, newest first, and fails closed when there are
	// more than the portal will list.
	ListRuns(ctx context.Context, ns string) ([]Run, error)
}

type DynamicCluster struct{ Client dynamic.Interface }

func (c DynamicCluster) CreatePipeline(ctx context.Context, p *Pipeline) error {
	// no labels, annotations, finalizers or ownerReferences: the admission policy refuses them
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group + "/v1alpha1",
		"kind":       "DeliveryPipeline",
		"metadata":   map[string]any{"name": p.Name, "namespace": p.Namespace},
		"spec":       map[string]any{"workload": p.Workload, "repo": p.Repo, "path": p.Path},
	}}
	_, err := c.Client.Resource(pipelineGVR).Namespace(p.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		return ErrExists
	case apierrors.IsInvalid(err):
		return invalid("the cluster rejected the pipeline")
	}
	return err
}

func (c DynamicCluster) GetPipeline(ctx context.Context, ns, name string) (*Pipeline, error) {
	obj, err := c.Client.Resource(pipelineGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get deliverypipeline: %w", err)
	}
	return pipelineFrom(obj), nil
}

func (c DynamicCluster) ListPipelines(ctx context.Context, ns string) ([]Pipeline, error) {
	items, err := listAll(ctx, c.Client, pipelineGVR, ns, maxPipes)
	if err != nil {
		return nil, err
	}
	out := make([]Pipeline, 0, len(items))
	for i := range items {
		out = append(out, *pipelineFrom(&items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
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

func (c DynamicCluster) CreateRun(ctx context.Context, r *Run) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group + "/v1alpha1",
		"kind":       "DeliveryRun",
		"metadata":   map[string]any{"name": r.Name, "namespace": r.Namespace},
		"spec":       map[string]any{"pipeline": r.Pipeline, "ref": r.Ref, "requestedBy": r.RequestedBy},
	}}
	_, err := c.Client.Resource(runGVR).Namespace(r.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		return ErrExists
	case apierrors.IsInvalid(err):
		return invalid("the cluster rejected the run")
	}
	return err
}

func (c DynamicCluster) ListRuns(ctx context.Context, ns string) ([]Run, error) {
	items, err := listAll(ctx, c.Client, runGVR, ns, maxListed)
	if err != nil {
		return nil, err
	}
	out := make([]Run, 0, len(items))
	for i := range items {
		out = append(out, *runFrom(&items[i]))
	}
	// the timestamp has second precision: the name breaks ties so the order is stable
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// listAll follows the continue token so an answer (and a cap check built on it) is never silently
// truncated; more than max objects fails closed.
func listAll(ctx context.Context, cl dynamic.Interface, gvr schema.GroupVersionResource, ns string, max int) ([]unstructured.Unstructured, error) {
	var out []unstructured.Unstructured
	opts := metav1.ListOptions{Limit: 100}
	for {
		l, err := cl.Resource(gvr).Namespace(ns).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", gvr.Resource, err)
		}
		out = append(out, l.Items...)
		if len(out) > max {
			return nil, fmt.Errorf("list %s: more than %d in %s", gvr.Resource, max, ns)
		}
		if l.GetContinue() == "" {
			return out, nil
		}
		opts.Continue = l.GetContinue()
	}
}

func created(obj *unstructured.Unstructured) string {
	return obj.GetCreationTimestamp().UTC().Format("2006-01-02T15:04:05Z")
}

func pipelineFrom(obj *unstructured.Unstructured) *Pipeline {
	p := &Pipeline{Namespace: obj.GetNamespace(), Name: obj.GetName(), CreatedAt: created(obj)}
	p.Workload, _, _ = unstructured.NestedString(obj.Object, "spec", "workload")
	p.Repo, _, _ = unstructured.NestedString(obj.Object, "spec", "repo")
	p.Path, _, _ = unstructured.NestedString(obj.Object, "spec", "path")
	if p.Path == "" {
		p.Path = DefaultPath
	}
	p.Phase, _, _ = unstructured.NestedString(obj.Object, "status", "phase")
	if p.Phase == "" {
		p.Phase = "PENDING"
	}
	p.Message, _, _ = unstructured.NestedString(obj.Object, "status", "message")
	p.ApplicationName, _, _ = unstructured.NestedString(obj.Object, "status", "applicationName")
	return p
}

func runFrom(obj *unstructured.Unstructured) *Run {
	r := &Run{Namespace: obj.GetNamespace(), Name: obj.GetName(), CreatedAt: created(obj)}
	r.Pipeline, _, _ = unstructured.NestedString(obj.Object, "spec", "pipeline")
	r.Ref, _, _ = unstructured.NestedString(obj.Object, "spec", "ref")
	r.RequestedBy, _, _ = unstructured.NestedString(obj.Object, "spec", "requestedBy")
	r.Phase, _, _ = unstructured.NestedString(obj.Object, "status", "phase")
	if r.Phase == "" {
		r.Phase = "PENDING"
	}
	r.Message, _, _ = unstructured.NestedString(obj.Object, "status", "message")
	r.ExecutionID, _, _ = unstructured.NestedString(obj.Object, "status", "executionId")
	return r
}
