package delivery

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// MaxConcurrentReconciles bounds parallel reconciles per controller.
	MaxConcurrentReconciles = 4

	ReasonReady            = "Ready"
	ReasonInvalidSpec      = "InvalidSpec"
	ReasonWorkloadNotFound = "WorkloadNotFound"
	ReasonAppConflict      = "ApplicationConflict"
	ReasonGateUnavailable  = "GateUnavailable"
	ReasonGateRejected     = "GateRejected"
	ReasonBuilding         = "Building"

	buildRetry    = 5 * time.Second
	workloadRetry = 30 * time.Second
	errorRetry    = 30 * time.Second
	driftResync   = 10 * time.Minute
	pollInterval  = 8 * time.Second
	pipelineWait  = 15 * time.Second
	// RunTimeout is how long a run may stay RUNNING before it is failed.
	RunTimeout = 30 * time.Minute

	maxMessage = 300
)

// Config is what both reconcilers need besides the Kubernetes client.
type Config struct {
	// Gate is nil unless the controller is enabled; Reconcile is then a no-op.
	Gate Gate
	// AllowedOrgs are the lowercased GitHub owners a repository may belong to.
	AllowedOrgs []string
	// Recorder may be nil (tests).
	Recorder events.EventRecorder
	// Now may be nil (time.Now); tests set it.
	Now func() time.Time
}

func (c *Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Config) event(obj runtime.Object, typ, reason, note string) {
	if c.Recorder != nil {
		c.Recorder.Eventf(obj, nil, typ, reason, reason, "%s", note)
	}
}

// trunc keeps a message within the CRD's limit.
func trunc(s string) string {
	if len(s) > maxMessage {
		return s[:maxMessage]
	}
	return s
}

// setStatus applies mutate to obj's status and writes it only if the object
// changed.
func setStatus[T client.Object](ctx context.Context, c client.Client, obj T, mutate func()) error {
	before := obj.DeepCopyObject()
	mutate()
	if equality.Semantic.DeepEqual(before, obj) {
		return nil
	}
	return c.Status().Update(ctx, obj)
}
