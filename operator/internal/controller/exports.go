package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NamespaceManaged is the namespace check of ManagedSecrets and
// WorkloadIdentities (a live namespace created by this operator for a live
// TeamNamespace of the same name), shared with the resource broker so both
// apply one definition of "managed".
func NamespaceManaged(ctx context.Context, r client.Reader, name string) (bool, string, error) {
	return namespaceManaged(ctx, r, name)
}
