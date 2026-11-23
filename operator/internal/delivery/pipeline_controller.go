package delivery

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

// The delivery controller's whole Kubernetes surface (ClusterRole idp-delivery,
// generated into config/rbac/delivery-role.yaml): read DeliveryPipelines,
// DeliveryRuns and WorkloadIdentities, update the status of the first two. No
// secrets, no spec writes, no finalizers. The admission policy
// idp-delivery-scope confines those updates further.
//
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=deliverypipelines;deliveryruns;workloadidentities,verbs=get;list;watch
// +kubebuilder:rbac:groups=idp.ithaca-idp.com,resources=deliverypipelines/status;deliveryruns/status,verbs=get;update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,namespace=idp-system,resources=leases,verbs=get;create;update
// Leader election also records a CORE event (API group "") in idp-system; without this it is rejected (log noise only).
// +kubebuilder:rbac:groups="",namespace=idp-system,resources=events,verbs=create;patch

// PipelineReconciler turns a DeliveryPipeline into a Spinnaker application and
// pipeline. A nil Gate (controller disabled) makes Reconcile a no-op.
type PipelineReconciler struct {
	client.Client
	Config
}

// NewPipelineReconciler wires the reconciler; c is the manager's client.
func NewPipelineReconciler(c client.Client, cfg Config) *PipelineReconciler {
	return &PipelineReconciler{Client: c, Config: cfg}
}

func (r *PipelineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Gate == nil {
		return ctrl.Result{}, nil
	}
	dp := &idpv1alpha1.DeliveryPipeline{}
	if err := r.Get(ctx, req.NamespacedName, dp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !dp.DeletionTimestamp.IsZero() {
		// Nothing to release: the Spinnaker application is kept (follow-up).
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)

	path, err := ValidateSpec(dp.Namespace, dp.Name, dp.Spec, r.AllowedOrgs)
	if err != nil {
		log.Info("refusing DeliveryPipeline", "reason", err.Error())
		r.event(dp, corev1.EventTypeWarning, ReasonInvalidSpec, err.Error())
		return ctrl.Result{}, r.fail(ctx, dp, ReasonInvalidSpec, err.Error())
	}
	app, _ := AppName(dp.Namespace, dp.Spec.Workload) // validated above

	wi := &idpv1alpha1.WorkloadIdentity{}
	switch err := r.Get(ctx, types.NamespacedName{Namespace: dp.Namespace, Name: dp.Spec.Workload}, wi); {
	case client.IgnoreNotFound(err) != nil:
		return ctrl.Result{}, err
	case err != nil || !wi.DeletionTimestamp.IsZero():
		msg := fmt.Sprintf("WorkloadIdentity %q does not exist in namespace %s", dp.Spec.Workload, dp.Namespace)
		return ctrl.Result{RequeueAfter: workloadRetry}, r.fail(ctx, dp, ReasonWorkloadNotFound, msg)
	}

	upToDate := dp.Status.Phase == idpv1alpha1.DeliveryPhaseReady && dp.Status.ObservedGeneration == dp.Generation
	if !upToDate && dp.Status.Phase != idpv1alpha1.DeliveryPhaseBuilding {
		if err := r.setPhase(ctx, dp, app, idpv1alpha1.DeliveryPhaseBuilding, metav1.ConditionFalse, ReasonBuilding, "creating the Spinnaker application and pipeline"); err != nil {
			return ctrl.Result{}, err
		}
	}

	res, err := r.sync(ctx, dp, app, path, upToDate)
	if err != nil {
		// A Gate failure never reaches the status as more than a status code.
		msg := Safe(err)
		log.Info("delivery pipeline not synced", "app", app, "reason", msg)
		if upToDate {
			// Keep READY: the pipeline worked, Gate may be restarting.
			return ctrl.Result{RequeueAfter: errorRetry}, nil
		}
		reason := ReasonGateUnavailable
		if IsPermanent(err) {
			reason = ReasonGateRejected
		}
		_ = r.setPhase(ctx, dp, app, idpv1alpha1.DeliveryPhaseBuilding, metav1.ConditionFalse, reason, msg)
		return ctrl.Result{RequeueAfter: errorRetry}, nil
	}
	if res != nil {
		return *res, nil
	}
	if err := r.setPhase(ctx, dp, app, idpv1alpha1.DeliveryPhaseReady, metav1.ConditionTrue, ReasonReady,
		fmt.Sprintf("Spinnaker application %s has pipeline %s", app, PipelineName)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: driftResync}, nil
}

// sync makes the application and pipeline exist (and match the spec unless the
// object is already up to date, when it only checks they are still there). A
// non-nil result asks for a requeue without an error.
func (r *PipelineReconciler) sync(ctx context.Context, dp *idpv1alpha1.DeliveryPipeline, app, path string, upToDate bool) (*ctrl.Result, error) {
	exists, desc, err := r.Gate.Application(ctx, app)
	if err != nil {
		return nil, err
	}
	want := AppDescription(dp.Namespace, dp.Spec.Workload)
	if exists && desc != want {
		// Anything but this pair's own marker is refused: never adopted, never stamped. That covers
		// another pair (the join <ns>-<workload> is ambiguous) and applications made outside the
		// controller (Deck, scripts).
		msg := fmt.Sprintf("Spinnaker application %s exists and is not managed by idp for this namespace and workload", app)
		return &ctrl.Result{}, r.fail(ctx, dp, ReasonAppConflict, msg)
	}
	if !exists {
		if err := r.Gate.CreateApplication(ctx, app, want); err != nil {
			return nil, err
		}
		// The create is a Spinnaker task: wait until the application shows up.
		return &ctrl.Result{RequeueAfter: buildRetry}, nil
	}

	id, err := r.Gate.PipelineID(ctx, app, PipelineName)
	if err != nil {
		return nil, err
	}
	if id == "" || !upToDate {
		body, err := RenderPipeline(dp.Namespace, dp.Spec.Workload, dp.Spec.Repo, path)
		if err != nil {
			return &ctrl.Result{}, r.fail(ctx, dp, ReasonInvalidSpec, err.Error())
		}
		if err := r.Gate.SavePipeline(ctx, id, body); err != nil {
			return nil, err
		}
		// Not READY until Gate lists the pipeline.
		id, err = r.Gate.PipelineID(ctx, app, PipelineName)
		if err != nil {
			return nil, err
		}
		if id == "" {
			return &ctrl.Result{RequeueAfter: pipelineWait}, nil
		}
	}
	return nil, nil
}

func (r *PipelineReconciler) fail(ctx context.Context, dp *idpv1alpha1.DeliveryPipeline, reason, msg string) error {
	app, _ := AppName(dp.Namespace, dp.Spec.Workload)
	return r.setPhase(ctx, dp, app, idpv1alpha1.DeliveryPhaseFailed, metav1.ConditionFalse, reason, msg)
}

func (r *PipelineReconciler) setPhase(ctx context.Context, dp *idpv1alpha1.DeliveryPipeline, app, phase string, ready metav1.ConditionStatus, reason, msg string) error {
	return setStatus(ctx, r.Client, dp, func() {
		dp.Status.Phase = phase
		dp.Status.Message = trunc(msg)
		dp.Status.ApplicationName = app
		dp.Status.ObservedGeneration = dp.Generation
		meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
			Type: idpv1alpha1.ConditionReady, Status: ready, Reason: reason, Message: trunc(msg), ObservedGeneration: dp.Generation,
		})
	})
}

// SetupWithManager watches the pipelines; a WorkloadIdentity event re-queues
// the pipeline of the same name, so one ordered before its workload syncs once
// the workload exists.
func (r *PipelineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	forWorkload := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&idpv1alpha1.DeliveryPipeline{}).
		Watches(&idpv1alpha1.WorkloadIdentity{}, forWorkload).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		Named("deliverypipeline").
		Complete(r)
}
