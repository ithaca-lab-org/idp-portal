package delivery

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	idpv1alpha1 "github.com/ithaca-lab-org/idp-portal/operator/api/v1alpha1"
)

// RunReconciler turns a DeliveryRun into a Spinnaker pipeline execution and
// follows it to a terminal phase. A nil Gate makes Reconcile a no-op.
type RunReconciler struct {
	client.Client
	Config
}

// NewRunReconciler wires the reconciler; c is the manager's client.
func NewRunReconciler(c client.Client, cfg Config) *RunReconciler {
	return &RunReconciler{Client: c, Config: cfg}
}

func (r *RunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Gate == nil {
		return ctrl.Result{}, nil
	}
	run := &idpv1alpha1.DeliveryRun{}
	if err := r.Get(ctx, req.NamespacedName, run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if run.Status.Phase == idpv1alpha1.DeliveryRunSucceeded || run.Status.Phase == idpv1alpha1.DeliveryRunFailed ||
		!run.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)

	if !ValidRef(run.Spec.Ref) {
		return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed, "spec.ref must be a full 40-hex commit SHA")
	}
	if run.Status.ExecutionID != "" {
		return r.poll(ctx, run)
	}
	if run.Status.Phase == idpv1alpha1.DeliveryRunRunning {
		// RUNNING was written before the trigger and no execution id followed: the trigger may
		// or may not have happened (a crash, or a Gate error after Gate accepted it). Never
		// trigger again from here; a new DeliveryRun is the retry.
		return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed,
			"the start of the execution was interrupted and its outcome is unknown; check Spinnaker and create a new run if needed")
	}

	dp := &idpv1alpha1.DeliveryPipeline{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.Pipeline}, dp); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed, fmt.Sprintf("DeliveryPipeline %q does not exist", run.Spec.Pipeline))
	}
	switch dp.Status.Phase {
	case idpv1alpha1.DeliveryPhaseReady:
	case idpv1alpha1.DeliveryPhaseFailed:
		return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed, fmt.Sprintf("DeliveryPipeline %q failed: %s", dp.Name, dp.Status.Message))
	default:
		if r.timedOut(run) {
			return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed, fmt.Sprintf("DeliveryPipeline %q did not become ready in time", dp.Name))
		}
		if err := r.setRun(ctx, run, idpv1alpha1.DeliveryRunPending, "", fmt.Sprintf("waiting for DeliveryPipeline %q to be ready", dp.Name)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: pipelineWait}, nil
	}
	// The pipeline's own checks passed to be READY; repeat the cheap one that
	// does not depend on the pipeline's status being current.
	if _, err := ValidateSpec(dp.Namespace, dp.Name, dp.Spec, r.AllowedOrgs); err != nil {
		return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed, trunc(err.Error()))
	}
	app, err := AppName(dp.Namespace, dp.Spec.Workload)
	if err != nil {
		return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed, trunc(err.Error()))
	}

	// The application must be this pair's: an application stamped for another namespace and
	// workload (the join is ambiguous) is never triggered. This read also keeps a Gate that is
	// down away from the RUNNING write below, so a transient outage is a retry, not a failure.
	exists, desc, err := r.Gate.Application(ctx, app)
	if err != nil {
		log.Info("application check failed", "app", app, "reason", Safe(err))
		if IsPermanent(err) || r.timedOut(run) {
			return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed, "could not start the pipeline: "+Safe(err))
		}
		_ = r.setRun(ctx, run, idpv1alpha1.DeliveryRunPending, "", "could not start the pipeline yet: "+Safe(err))
		return ctrl.Result{RequeueAfter: errorRetry}, nil
	}
	if !exists || desc != AppDescription(dp.Namespace, dp.Spec.Workload) {
		return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed,
			fmt.Sprintf("Spinnaker application %s is missing or does not belong to this workload", app))
	}

	// RUNNING (no execution id) is written BEFORE the trigger, with the resourceVersion we read:
	// a stale cached object, or a second reconcile that got there first, fails with a conflict
	// and nothing is triggered. From RUNNING the controller never triggers (see above).
	if err := r.setRun(ctx, run, idpv1alpha1.DeliveryRunRunning, "", "starting the pipeline execution"); err != nil {
		return ctrl.Result{}, err
	}
	id, err := r.Gate.Trigger(ctx, app, PipelineName, map[string]string{"ref": run.Spec.Ref})
	if err != nil {
		log.Info("trigger failed", "app", app, "reason", Safe(err))
		r.event(run, corev1.EventTypeWarning, ReasonGateRejected, Safe(err))
		return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed, "could not start the pipeline (create a new run to retry): "+Safe(err))
	}
	// A crash before this write leaves RUNNING without an id: the run is then failed, not repeated.
	if err := r.setRun(ctx, run, idpv1alpha1.DeliveryRunRunning, id, "pipeline execution started"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

func (r *RunReconciler) poll(ctx context.Context, run *idpv1alpha1.DeliveryRun) (ctrl.Result, error) {
	if !ValidExecutionID(run.Status.ExecutionID) {
		return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed, "status.executionId is not an execution id")
	}
	st, execApp, err := r.Gate.ExecutionStatus(ctx, run.Status.ExecutionID)
	if err != nil {
		logf.FromContext(ctx).Info("execution status unavailable", "reason", Safe(err))
		if r.timedOut(run) {
			return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed, "gave up waiting for the execution: "+Safe(err))
		}
		return ctrl.Result{RequeueAfter: errorRetry}, nil
	}
	// The id must belong to this run's application (the pipeline is named after its workload).
	if want, aerr := AppName(run.Namespace, run.Spec.Pipeline); aerr != nil || execApp != want {
		return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed, "the execution does not belong to this workload's application")
	}
	switch st {
	case "SUCCEEDED":
		return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunSucceeded, "pipeline execution succeeded")
	case "TERMINAL", "CANCELED", "STOPPED", "FAILED_CONTINUE":
		return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed, "pipeline execution ended with status "+safeStatus(st))
	}
	if r.timedOut(run) {
		return ctrl.Result{}, r.finish(ctx, run, idpv1alpha1.DeliveryRunFailed, "pipeline execution did not finish in time")
	}
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

// safeStatus keeps only the known Spinnaker status names in a message.
func safeStatus(s string) string {
	switch s {
	case "TERMINAL", "CANCELED", "STOPPED", "FAILED_CONTINUE":
		return s
	}
	return "UNKNOWN"
}

func (r *RunReconciler) timedOut(run *idpv1alpha1.DeliveryRun) bool {
	return r.now().Sub(run.CreationTimestamp.Time) > RunTimeout
}

func (r *RunReconciler) finish(ctx context.Context, run *idpv1alpha1.DeliveryRun, phase, msg string) error {
	return r.setRun(ctx, run, phase, run.Status.ExecutionID, msg)
}

func (r *RunReconciler) setRun(ctx context.Context, run *idpv1alpha1.DeliveryRun, phase, execID, msg string) error {
	return setStatus(ctx, r.Client, run, func() {
		run.Status.Phase = phase
		run.Status.ExecutionID = execID
		run.Status.Message = trunc(msg)
	})
}

// SetupWithManager watches the runs; a DeliveryPipeline event re-queues the
// runs of that pipeline, so a run waiting for READY proceeds at once.
func (r *RunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	forPipeline := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		list := &idpv1alpha1.DeliveryRunList{}
		if err := r.List(ctx, list, client.InNamespace(o.GetNamespace())); err != nil {
			return nil
		}
		var out []reconcile.Request
		for _, run := range list.Items {
			if run.Spec.Pipeline == o.GetName() && run.Status.ExecutionID == "" {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: run.Namespace, Name: run.Name}})
			}
		}
		return out
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&idpv1alpha1.DeliveryRun{}).
		Watches(&idpv1alpha1.DeliveryPipeline{}, forPipeline).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		Named("deliveryrun").
		Complete(r)
}
