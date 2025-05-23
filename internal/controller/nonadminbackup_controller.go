/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package controller contains all controllers of the project
package controller

import (
	"context"
	"errors"
	"reflect"

	"github.com/go-logr/logr"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	velerov2alpha1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v2alpha1"
	"github.com/vmware-tanzu/velero/pkg/builder"
	veleroclient "github.com/vmware-tanzu/velero/pkg/client"
	"github.com/vmware-tanzu/velero/pkg/label"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nacv1alpha1 "github.com/migtools/oadp-non-admin/api/v1alpha1"
	"github.com/migtools/oadp-non-admin/internal/common/constant"
	"github.com/migtools/oadp-non-admin/internal/common/function"
	"github.com/migtools/oadp-non-admin/internal/handler"
	"github.com/migtools/oadp-non-admin/internal/predicate"
)

// NonAdminBackupReconciler reconciles a NonAdminBackup object
type NonAdminBackupReconciler struct {
	client.Client
	Scheme             *runtime.Scheme
	EnforcedBackupSpec *velerov1.BackupSpec
	OADPNamespace      string
}

type nonAdminBackupReconcileStepFunction func(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error)

const (
	veleroReferenceUpdated = "NonAdminBackup - Status Updated with UUID reference"
	statusUpdateExit       = "NonAdminBackup - Exit after Status Update"
	statusUpdateError      = "Failed to update NonAdminBackup Status"
	findSingleVBError      = "Error encountered while retrieving VeleroBackup for NAB during the Delete operation"
	findSingleVDBRError    = "Error encountered while retrieving DeleteBackupRequest for NAB during the Delete operation"
)

var (
	alwaysExcludedNamespacedResources = []string{
		nacv1alpha1.NonAdminBackups,
		nacv1alpha1.NonAdminRestores,
		nacv1alpha1.NonAdminBackupStorageLocations,
	}
	alwaysExcludedClusterResources = []string{
		"securitycontextconstraints",
		"clusterroles",
		"clusterrolebindings",
		"priorityclasses",
		"customresourcedefinitions",
		"virtualmachineclusterinstancetypes",
		"virtualmachineclusterpreferences",
	}
)

// +kubebuilder:rbac:groups=oadp.openshift.io,resources=nonadminbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=oadp.openshift.io,resources=nonadminbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=oadp.openshift.io,resources=nonadminbackups/finalizers,verbs=update

// +kubebuilder:rbac:groups=velero.io,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=velero.io,resources=deletebackuprequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=velero.io,resources=podvolumebackups,verbs=get;list;watch
// +kubebuilder:rbac:groups=velero.io,resources=datauploads,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state,
// defined in NonAdminBackup object Spec.
func (r *NonAdminBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("NonAdminBackup Reconcile start")

	// Get the NonAdminBackup object
	nab := &nacv1alpha1.NonAdminBackup{}
	err := r.Get(ctx, req.NamespacedName, nab)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info(err.Error())
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Unable to fetch NonAdminBackup")
		return ctrl.Result{}, err
	}

	// Determine which path to take
	var reconcileSteps []nonAdminBackupReconcileStepFunction

	// First switch statement takes precedence over the next one
	switch {
	case nab.Spec.DeleteBackup:
		// Standard delete path - creates DeleteBackupRequest and waits for VeleroBackup deletion
		logger.V(1).Info("Executing standard delete path")
		reconcileSteps = []nonAdminBackupReconcileStepFunction{
			r.setStatusAndConditionForDeletionAndCallDelete,
			r.deleteNonAdminRestores,
			r.createVeleroDeleteBackupRequest,
		}

	case !nab.DeletionTimestamp.IsZero():
		// Direct deletion path - sets status and condition
		// Remove dependent VeleroBackup object
		// Remove finalizer from the NonAdminBackup object
		// If there was existing BSL pointing to the Backup object
		// the Backup will be restored causing the NAB to be recreated
		logger.V(1).Info("Executing direct deletion path")
		reconcileSteps = []nonAdminBackupReconcileStepFunction{
			r.setStatusForDirectKubernetesAPIDeletion,
			r.deleteDeleteBackupRequestObjects,
			r.deleteVeleroBackupObjects,
		}

	case function.CheckLabelAnnotationValueIsValid(nab.Labels, constant.NabSyncLabel):
		logger.V(1).Info("Executing nab sync path")
		reconcileSteps = []nonAdminBackupReconcileStepFunction{
			r.setBackupUUIDInStatus,
			r.setFinalizerOnNonAdminBackup,
			r.createVeleroBackupAndSyncWithNonAdminBackup,
		}

	default:
		// Standard creation/update path
		logger.V(1).Info("Executing nab creation/update path")
		reconcileSteps = []nonAdminBackupReconcileStepFunction{
			r.initNabCreate,
			r.validateSpec,
			r.setBackupUUIDInStatus,
			r.setFinalizerOnNonAdminBackup,
			r.createVeleroBackupAndSyncWithNonAdminBackup,
		}
	}

	// Execute the selected reconciliation steps
	for _, step := range reconcileSteps {
		requeue, err := step(ctx, logger, nab)
		if err != nil {
			return ctrl.Result{}, err
		} else if requeue {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	logger.V(1).Info("NonAdminBackup Reconcile exit")
	return ctrl.Result{}, nil
}

// setStatusAndConditionForDeletionAndCallDelete updates the NonAdminBackup status and conditions
// to reflect that deletion has been initiated, and triggers the actual deletion if needed.
//
// Parameters:
//   - ctx: Context for managing request lifetime.
//   - logger: Logger instance for logging messages.
//   - nab: Pointer to the NonAdminBackup object being processed.
//
// Returns:
//   - bool: true if reconciliation should be requeued, false otherwise
//   - error: any error encountered during the process
func (r *NonAdminBackupReconciler) setStatusAndConditionForDeletionAndCallDelete(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	requeueRequired := false
	updatedPhase := updateNonAdminPhase(&nab.Status.Phase, nacv1alpha1.NonAdminPhaseDeleting)
	updatedCondition := meta.SetStatusCondition(&nab.Status.Conditions,
		metav1.Condition{
			Type:    string(nacv1alpha1.NonAdminConditionDeleting),
			Status:  metav1.ConditionTrue,
			Reason:  "DeletionPending",
			Message: "backup accepted for deletion",
		},
	)
	if updatedPhase || updatedCondition {
		if err := r.Status().Update(ctx, nab); err != nil {
			logger.Error(err, statusUpdateError)
			return false, err
		}
		logger.V(1).Info("NonAdminBackup status marked for deletion")
	} else {
		logger.V(1).Info("NonAdminBackup status unchanged during deletion")
	}
	if nab.DeletionTimestamp.IsZero() {
		logger.V(1).Info("Marking NonAdminBackup for deletion", constant.NameString, nab.Name)
		if err := r.Delete(ctx, nab); err != nil {
			logger.Error(err, "Failed to call Delete on the NonAdminBackup object")
			return false, err
		}
		requeueRequired = true // Requeue to allow deletion to proceed
	}
	return requeueRequired, nil
}

// setStatusForDirectKubernetesAPIDeletion updates the status and conditions when a NonAdminBackup
// is deleted directly through the Kubernetes API. Only updates status and conditions
// if the NAB finalizer exists.
//
// Parameters:
//   - ctx: Context for managing request lifetime
//   - logger: Logger instance
//   - nab: NonAdminBackup being deleted
//
// Returns:
//   - bool: true if status was updated
//   - error: any error encountered
func (r *NonAdminBackupReconciler) setStatusForDirectKubernetesAPIDeletion(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	// We don't need to check here if the finalizer exists as we already checked if !nab.ObjectMeta.DeletionTimestamp.IsZero()
	// which means that something prevented the NAB object from being deleted
	updatedPhase := updateNonAdminPhase(&nab.Status.Phase, nacv1alpha1.NonAdminPhaseDeleting)
	updatedCondition := meta.SetStatusCondition(&nab.Status.Conditions,
		metav1.Condition{
			Type:    string(nacv1alpha1.NonAdminConditionDeleting),
			Status:  metav1.ConditionTrue,
			Reason:  "DeletionPending",
			Message: "permanent backup deletion requires setting spec.deleteBackup to true",
		},
	)
	if updatedPhase || updatedCondition {
		if err := r.Status().Update(ctx, nab); err != nil {
			logger.Error(err, statusUpdateError)
			return false, err
		}
		logger.V(1).Info("NonAdminBackup status marked for deletion during direct API deletion")
		// This is final step in the direct API deletion path we do not want to requeue
	} else {
		logger.V(1).Info("NonAdminBackup status unchanged during direct API deletion")
	}
	return false, nil
}

func (r *NonAdminBackupReconciler) deleteNonAdminRestores(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	logger.V(1).Info("checking for NonAdminRestores to delete")
	nonAdminRestores := &nacv1alpha1.NonAdminRestoreList{}
	if err := r.List(ctx, nonAdminRestores, client.InNamespace(nab.Namespace)); err != nil {
		logger.Error(err, "Failed to list NonAdminRestores in NonAdminBackup namespace")
		return false, err
	}

	for _, nonAdminRestore := range nonAdminRestores.Items {
		if nonAdminRestore.Spec.RestoreSpec.BackupName == nab.Name {
			if err := r.Delete(ctx, &nonAdminRestore); err != nil {
				logger.Error(err, "Failed to delete NonAdminRestore in NonAdminBackup namespace")
				return false, err
			}
			logger.V(1).Info("NonAdminRestore deleted")
		}
	}

	return false, nil
}

// createVeleroDeleteBackupRequest initiates deletion of the associated VeleroBackup object
// that is referenced by the NACUUID within the NonAdminBackup (NAB) object.
// This ensures the VeleroBackup is deleted before the NAB object itself is removed.
//
// Parameters:
//   - ctx: Context to manage request lifetime.
//   - logger: Logger instance for logging messages.
//   - nab: Pointer to the NonAdminBackup object to be managed.
//
// This function first checks if the NAB object has the finalizer. If yes, it attempts to locate the associated
// VeleroBackup object by the UUID stored in the NAB status. If a unique VeleroBackup object is found,
// deletion is initiated, and the reconcile loop will be requeued. If multiple VeleroBackup objects
// are found with the same label, the function logs an error and returns.
//
// Returns:
//   - A boolean indicating whether to requeue the reconcile loop,
//   - An error if VeleroBackup deletion or retrieval fails, for example, due to multiple VeleroBackup objects found with the same UUID label.
func (r *NonAdminBackupReconciler) createVeleroDeleteBackupRequest(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	// This function is called just after setStatusAndConditionForDeletionAndCallDelete - standard delete path, which already
	// requeued the reconciliation to get the latest NAB object. There is no need to fetch the latest NAB object here.
	if !controllerutil.ContainsFinalizer(nab, constant.NabFinalizerName) ||
		nab.Status.VeleroBackup == nil ||
		nab.Status.VeleroBackup.NACUUID == constant.EmptyString {
		return false, nil
	}

	// Initiate deletion of the VeleroBackup object only when the finalizer exists.
	veleroBackupNACUUID := nab.Status.VeleroBackup.NACUUID
	veleroBackup, err := function.GetVeleroBackupByLabel(ctx, r.Client, r.OADPNamespace, veleroBackupNACUUID)

	if err != nil {
		// Log error if multiple VeleroBackup objects are found
		logger.Error(err, findSingleVBError, constant.UUIDString, veleroBackupNACUUID)
		return false, err
	}

	if veleroBackup == nil {
		return r.removeNabFinalizerUponVeleroBackupDeletion(ctx, logger, nab)
	}

	deleteBackupRequest, err := function.GetVeleroDeleteBackupRequestByLabel(ctx, r.Client, r.OADPNamespace, veleroBackupNACUUID)
	if err != nil {
		// Log error if multiple DeleteBackupRequest objects are found
		logger.Error(err, findSingleVDBRError, constant.UUIDString, veleroBackupNACUUID)
		return false, err
	}

	if deleteBackupRequest == nil {
		// Build the delete request for VeleroBackup created by NAC
		deleteBackupRequest = builder.ForDeleteBackupRequest(r.OADPNamespace, "").
			BackupName(veleroBackup.Name).
			ObjectMeta(
				builder.WithLabels(
					velerov1.BackupNameLabel, label.GetValidName(veleroBackup.Name),
					velerov1.BackupUIDLabel, string(veleroBackup.UID),
					constant.NabOriginNACUUIDLabel, veleroBackupNACUUID,
				),
				builder.WithLabelsMap(function.GetNonAdminLabels()),
				builder.WithAnnotationsMap(function.GetNonAdminBackupAnnotations(nab.ObjectMeta)),
				builder.WithGenerateName(veleroBackup.Name+"-"),
			).Result()

		// Use CreateRetryGenerateName for retry logic in creating the delete request
		if err := veleroclient.CreateRetryGenerateName(r.Client, ctx, deleteBackupRequest); err != nil {
			logger.Error(err, "Failed to create delete request for VeleroBackup", "VeleroBackup name", veleroBackup.Name, "NonAdminBackup name", nab.Name)
			return false, err
		}
		logger.V(1).Info("Request to delete backup submitted successfully", "VeleroBackup name", veleroBackup.Name, "NonAdminBackup name", nab.Name)
		nab.Status.VeleroDeleteBackupRequest = &nacv1alpha1.VeleroDeleteBackupRequest{
			NACUUID:   veleroBackupNACUUID,
			Namespace: r.OADPNamespace,
			Name:      deleteBackupRequest.Name,
		}
	}
	// Ensure that the NonAdminBackup's NonAdminBackupStatus is in sync
	// with the DeleteBackupRequest. Any required updates to the NonAdminBackup
	// Status will be applied based on the current state of the DeleteBackupRequest.
	updated := updateNonAdminBackupDeleteBackupRequestStatus(&nab.Status, deleteBackupRequest)
	if updated {
		if err := r.Status().Update(ctx, nab); err != nil {
			logger.Error(err, "Failed to update NonAdminBackup Status after DeleteBackupRequest reconciliation")
			return false, err
		}
		logger.V(1).Info("NonAdminBackup DeleteBackupRequest Status updated successfully")
	} else {
		logger.V(1).Info("NonAdminBackup DeleteBackupRequest Status unchanged")
	}

	return false, nil // Continue so initNabDeletion can initialize deletion of a NonAdminBackup object
}

// deleteVeleroBackupObjects deletes the VeleroBackup objects
// associated with a given NonAdminBackup
//
// Parameters:
//   - ctx: Context for managing request lifetime
//   - logger: Logger instance
//   - nab: NonAdminBackup object
//
// Returns:
//   - bool: whether to requeue (always false)
//   - error: any error encountered during deletion
func (r *NonAdminBackupReconciler) deleteVeleroBackupObjects(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	// This function is called in a workflows where requeue just happened.
	// There is no need to fetch the latest NAB object here.
	if nab.Status.VeleroBackup == nil || nab.Status.VeleroBackup.NACUUID == constant.EmptyString {
		return false, nil
	}

	veleroBackupNACUUID := nab.Status.VeleroBackup.NACUUID
	veleroBackup, err := function.GetVeleroBackupByLabel(ctx, r.Client, r.OADPNamespace, veleroBackupNACUUID)

	if err != nil {
		// Case where more than one VeleroBackup is found with the same label UUID
		// TODO (migi): Determine if all objects with this UUID should be deleted
		logger.Error(err, findSingleVBError, constant.UUIDString, veleroBackupNACUUID)
		return false, err
	}

	if veleroBackup != nil {
		if err = r.Delete(ctx, veleroBackup); err != nil {
			logger.Error(err, "Failed to delete VeleroBackup", constant.NameString, veleroBackup.Name)
			return false, err
		}
		logger.V(1).Info("VeleroBackup deletion initiated", constant.NameString, veleroBackup.Name)
		return false, nil
	}

	return r.removeNabFinalizerUponVeleroBackupDeletion(ctx, logger, nab)
}

// deleteDeleteBackupRequestObjects deletes the VeleroBackup DeleteBackupRequestObjects
// associated with a given NonAdminBackup
//
// Parameters:
//   - ctx: Context for managing request lifetime
//   - logger: Logger instance
//   - nab: NonAdminBackup object
//
// Returns:
//   - bool: whether to requeue (always false)
//   - error: any error encountered during deletion
func (r *NonAdminBackupReconciler) deleteDeleteBackupRequestObjects(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	// There is no need to fetch the latest NAB object here.
	if nab.Status.VeleroBackup == nil || nab.Status.VeleroBackup.NACUUID == constant.EmptyString {
		return false, nil
	}

	veleroBackupNACUUID := nab.Status.VeleroBackup.NACUUID
	deleteBackupRequest, err := function.GetVeleroDeleteBackupRequestByLabel(ctx, r.Client, r.OADPNamespace, veleroBackupNACUUID)
	if err != nil {
		// Log error if multiple DeleteBackupRequest objects are found
		logger.Error(err, findSingleVDBRError, constant.UUIDString, veleroBackupNACUUID)
		return false, err
	}
	if deleteBackupRequest != nil {
		if err = r.Delete(ctx, deleteBackupRequest); err != nil {
			logger.Error(err, "Failed to delete VeleroDeleteBackupRequest", constant.NameString, deleteBackupRequest.Name)
			return false, err
		}
		logger.V(1).Info("VeleroDeleteBackupRequest deletion initiated", constant.NameString, deleteBackupRequest.Name)
	}
	return false, nil // Continue so initNabDeletion can initialize deletion of an NonAdminBackup object
}

// removeNabFinalizerUponVeleroBackupDeletion ensures the associated VeleroBackup object is deleted
// and removes the finalizer from the NonAdminBackup (NAB) object to complete its cleanup process.
//
// Parameters:
//   - ctx: Context for managing request lifetime.
//   - logger: Logger instance for logging messages.
//   - nab: Pointer to the NonAdminBackup object undergoing cleanup.
//
// This function first checks if the `DeleteBackup` field in the NAB spec is set to true or if
// the NAB has been marked for deletion by Kubernetes. If either condition is met, it verifies
// the existence of an associated VeleroBackup object by consulting the UUID stored in the NAB’s status.
// If the VeleroBackup is found, the function waits for it to be deleted before proceeding further.
// After confirming VeleroBackup deletion, it removes the finalizer from the NAB, allowing Kubernetes
// to complete the garbage collection process for the NAB object itself.
//
// Returns:
//   - A boolean indicating whether to requeue the reconcile loop (true if waiting for VeleroBackup deletion).
//   - An error if any update operation or deletion check fails.
func (r *NonAdminBackupReconciler) removeNabFinalizerUponVeleroBackupDeletion(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	logger.V(1).Info("VeleroBackup deleted, removing NonAdminBackup finalizer")

	controllerutil.RemoveFinalizer(nab, constant.NabFinalizerName)

	if err := r.Update(ctx, nab); err != nil {
		logger.Error(err, "Failed to remove finalizer from NonAdminBackup")
		return false, err
	}

	logger.V(1).Info("NonAdminBackup finalizer removed and object deleted")

	return false, nil
}

// initNabCreate initializes the Status.Phase from the NonAdminBackup.
//
// Parameters:
//
//	ctx: Context for the request.
//	logger: Logger instance for logging messages.
//	nab: Pointer to the NonAdminBackup object.
//
// The function checks if the Phase of the NonAdminBackup object is empty.
// If it is empty, it sets the Phase to "New".
// It then returns boolean values indicating whether the reconciliation loop should requeue or exit
// and error value whether the status was updated successfully.
func (r *NonAdminBackupReconciler) initNabCreate(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	// If phase is already set, nothing to do
	if nab.Status.Phase != constant.EmptyString {
		logger.V(1).Info("NonAdminBackup Phase already initialized", constant.CurrentPhaseString, nab.Status.Phase)
		return false, nil
	}

	// Set phase to New
	if updated := updateNonAdminPhase(&nab.Status.Phase, nacv1alpha1.NonAdminPhaseNew); updated {
		if err := r.Status().Update(ctx, nab); err != nil {
			logger.Error(err, statusUpdateError)
			return false, err
		}
		logger.V(1).Info("NonAdminBackup Phase set to New")
	} else {
		logger.V(1).Info("NonAdminBackup Phase update skipped", constant.CurrentPhaseString, nab.Status.Phase)
	}

	return false, nil
}

// validateSpec validates the Spec from the NonAdminBackup.
//
// Parameters:
//
//	ctx: Context for the request.
//	logger: Logger instance for logging messages.
//	nab: Pointer to the NonAdminBackup object.
//
// The function validates the Spec from the NonAdminBackup object.
// If the BackupSpec is invalid, the function sets the NonAdminBackup phase to "BackingOff".
// If the BackupSpec is invalid, the function sets the NonAdminBackup condition Accepted to "False".
// If the BackupSpec is valid, the function sets the NonAdminBackup condition Accepted to "True".
func (r *NonAdminBackupReconciler) validateSpec(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	err := function.ValidateBackupSpec(ctx, r.Client, r.OADPNamespace, nab, r.EnforcedBackupSpec)
	if err != nil {
		updatedPhase := updateNonAdminPhase(&nab.Status.Phase, nacv1alpha1.NonAdminPhaseBackingOff)
		updatedCondition := meta.SetStatusCondition(&nab.Status.Conditions,
			metav1.Condition{
				Type:    string(nacv1alpha1.NonAdminConditionAccepted),
				Status:  metav1.ConditionFalse,
				Reason:  "InvalidBackupSpec",
				Message: err.Error(),
			},
		)
		if updatedPhase || updatedCondition {
			if updateErr := r.Status().Update(ctx, nab); updateErr != nil {
				logger.Error(updateErr, statusUpdateError)
				return false, updateErr
			}
			logger.V(1).Info("NonAdminBackup Phase set to BackingOff")
			logger.V(1).Info("NonAdminBackup condition set to InvalidBackupSpec")
		}
		return false, reconcile.TerminalError(err)
	}

	logger.V(1).Info("NonAdminBackup Spec is valid")

	updated := meta.SetStatusCondition(&nab.Status.Conditions,
		metav1.Condition{
			Type:    string(nacv1alpha1.NonAdminConditionAccepted),
			Status:  metav1.ConditionTrue,
			Reason:  "BackupAccepted",
			Message: "backup accepted",
		},
	)
	if updated {
		if err := r.Status().Update(ctx, nab); err != nil {
			logger.Error(err, statusUpdateError)
			return false, err
		}
		logger.V(1).Info("NonAdminBackup condition set to Accepted")
	} else {
		logger.V(1).Info("NonAdminBackup already has Accepted condition")
	}
	return false, nil
}

// setBackupUUIDInStatus generates a UUID for VeleroBackup and stores it in the NonAdminBackup status.
//
// Parameters:
//
//	ctx: Context for the request.
//	logger: Logger instance for logging messages.
//	nab: Pointer to the NonAdminBackup object.
//
// This function generates a UUID and stores it in the VeleroBackup status field of NonAdminBackup.
func (r *NonAdminBackupReconciler) setBackupUUIDInStatus(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	// Get the latest version of the NAB object just before checking if the NACUUID is set
	// to ensure we do not miss any updates to the NAB object
	nabOriginal := nab.DeepCopy()
	if err := r.Get(ctx, types.NamespacedName{Name: nabOriginal.Name, Namespace: nabOriginal.Namespace}, nab); err != nil {
		logger.Error(err, "Failed to re-fetch NonAdminBackup")
		return false, err
	}

	if nab.Status.VeleroBackup == nil || nab.Status.VeleroBackup.NACUUID == constant.EmptyString {
		var veleroBackupNACUUID string
		if value, ok := nab.Labels[constant.NabSyncLabel]; ok {
			// TODO check value is valid?
			veleroBackupNACUUID = value
		} else {
			veleroBackupNACUUID = function.GenerateNacObjectUUID(nab.Namespace, nab.Name)
		}
		nab.Status.VeleroBackup = &nacv1alpha1.VeleroBackup{
			NACUUID:   veleroBackupNACUUID,
			Namespace: r.OADPNamespace,
			Name:      veleroBackupNACUUID,
		}
		if err := r.Status().Update(ctx, nab); err != nil {
			logger.Error(err, statusUpdateError)
			return false, err
		}
		logger.V(1).Info(veleroReferenceUpdated)
	} else {
		logger.V(1).Info("NonAdminBackup already contains VeleroBackup UUID reference")
	}
	return false, nil
}

func (r *NonAdminBackupReconciler) setFinalizerOnNonAdminBackup(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	// If the object does not have the finalizer, add it before creating Velero Backup
	// to ensure we won't risk having orphant Velero Backup resource, due to an unexpected error
	// while adding finalizer after creatign Velero Backup
	if !controllerutil.ContainsFinalizer(nab, constant.NabFinalizerName) {
		controllerutil.AddFinalizer(nab, constant.NabFinalizerName)
		if err := r.Update(ctx, nab); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return false, err
		}
		logger.V(1).Info("Finalizer added to NonAdminBackup", "finalizer", constant.NabFinalizerName)
	} else {
		logger.V(1).Info("Finalizer exists on the NonAdminBackup object", "finalizer", constant.NabFinalizerName)
	}
	return false, nil
}

// createVeleroBackupAndSyncWithNonAdminBackup ensures the VeleroBackup associated with the given NonAdminBackup resource
// is created, if it does not exist.
// The function also updates the status and conditions of the NonAdminBackup resource to reflect the state
// of the VeleroBackup.
//
// Parameters:
//
//	ctx: Context for the request.
//	logger: Logger instance for logging messages.
//	nab: Pointer to the NonAdminBackup object.
func (r *NonAdminBackupReconciler) createVeleroBackupAndSyncWithNonAdminBackup(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	if nab.Status.VeleroBackup == nil || nab.Status.VeleroBackup.NACUUID == constant.EmptyString {
		return false, errors.New("unable to get Velero Backup UUID from NonAdminBackup Status")
	}

	veleroBackupNACUUID := nab.Status.VeleroBackup.NACUUID

	veleroBackup, err := function.GetVeleroBackupByLabel(ctx, r.Client, r.OADPNamespace, veleroBackupNACUUID)

	if err != nil {
		// Case in which more then one VeleroBackup is found with the same label UUID
		logger.Error(err, findSingleVBError, constant.UUIDString, veleroBackupNACUUID)
		return false, err
	}

	if veleroBackup == nil {
		if function.CheckLabelAnnotationValueIsValid(nab.Labels, constant.NabSyncLabel) || nab.Status.Phase == nacv1alpha1.NonAdminPhaseCreated {
			if function.CheckLabelAnnotationValueIsValid(nab.Labels, constant.NabSyncLabel) {
				err = errors.New("related Velero Backup to be synced from does not exist")
			}
			if meta.IsStatusConditionTrue(nab.Status.Conditions, string(nacv1alpha1.NonAdminConditionQueued)) {
				err = errors.New("NonAdminBackup is finalized and its associated Velero Backup has been removed. Please create a new NonAdminBackup to initiate a new backup")
			}
			logger.Error(err, "related Velero Backup not found")
			updatedPhase := updateNonAdminPhase(&nab.Status.Phase, nacv1alpha1.NonAdminPhaseBackingOff)
			updatedCondition := meta.SetStatusCondition(&nab.Status.Conditions,
				metav1.Condition{
					// TODO create new condition?
					Type:    string(nacv1alpha1.NonAdminConditionAccepted),
					Status:  metav1.ConditionFalse,
					Reason:  "VeleroBackupNotFound",
					Message: err.Error(),
				},
			)
			if updatedPhase || updatedCondition {
				if updateErr := r.Status().Update(ctx, nab); updateErr != nil {
					logger.Error(updateErr, nonAdminRestoreStatusUpdateFailureMessage)
					return false, updateErr
				}
			}
			return false, reconcile.TerminalError(err)
		}
		logger.Info("VeleroBackup with label not found, creating one", constant.UUIDString, veleroBackupNACUUID)

		backupSpec := nab.Spec.BackupSpec.DeepCopy()
		enforcedSpec := reflect.ValueOf(r.EnforcedBackupSpec).Elem()
		for index := range enforcedSpec.NumField() {
			enforcedField := enforcedSpec.Field(index)
			enforcedFieldName := enforcedSpec.Type().Field(index).Name
			currentField := reflect.ValueOf(backupSpec).Elem().FieldByName(enforcedFieldName)
			if !enforcedField.IsZero() && currentField.IsZero() {
				currentField.Set(enforcedField)
			}
		}

		// Included Namespaces are set by the controller and can not be overridden by the user
		// nor admin user
		backupSpec.IncludedNamespaces = []string{nab.Namespace}
		if backupSpec.StorageLocation != constant.EmptyString {
			nonAdminBsl := &nacv1alpha1.NonAdminBackupStorageLocation{}

			if nabslErr := r.Get(ctx, types.NamespacedName{Name: backupSpec.StorageLocation, Namespace: nab.Namespace}, nonAdminBsl); nabslErr != nil {
				return false, nabslErr
			}

			backupSpec.StorageLocation = nonAdminBsl.Status.VeleroBackupStorageLocation.Name
		}

		// Exclude NAC resources (NAB, NAR, NABSL) from Non-Admin backups
		// Determine if any of the new-style resource filter parameters are set
		haveNewResourceFilterParameters := len(backupSpec.IncludedClusterScopedResources) > 0 ||
			len(backupSpec.ExcludedClusterScopedResources) > 0 ||
			len(backupSpec.IncludedNamespaceScopedResources) > 0 ||
			len(backupSpec.ExcludedNamespaceScopedResources) > 0

		if haveNewResourceFilterParameters {
			// Use the new-style exclusion list
			backupSpec.ExcludedNamespaceScopedResources = append(backupSpec.ExcludedNamespaceScopedResources,
				alwaysExcludedNamespacedResources...)
			backupSpec.ExcludedClusterScopedResources = append(backupSpec.ExcludedClusterScopedResources,
				alwaysExcludedClusterResources...)
		} else {
			// Fallback to the old-style exclusion list
			backupSpec.ExcludedResources = append(backupSpec.ExcludedResources,
				alwaysExcludedNamespacedResources...)
			backupSpec.ExcludedResources = append(backupSpec.ExcludedResources,
				alwaysExcludedClusterResources...)
		}

		veleroBackup = &velerov1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:        veleroBackupNACUUID,
				Namespace:   r.OADPNamespace,
				Labels:      function.GetNonAdminLabels(),
				Annotations: function.GetNonAdminBackupAnnotations(nab.ObjectMeta),
			},
			Spec: *backupSpec,
		}

		// Add NonAdminBackup's veleroBackupNACUUID as the label to the VeleroBackup object
		// We don't add this as an argument of GetNonAdminLabels(), because there may be
		// situations where NAC object do not require NabOriginUUIDLabel
		veleroBackup.Labels[constant.NabOriginNACUUIDLabel] = veleroBackupNACUUID

		err = r.Create(ctx, veleroBackup)

		if err != nil {
			// We do not retry here as the veleroBackupNACUUID
			// should be guaranteed to be unique
			logger.Error(err, "Failed to create VeleroBackup")
			return false, err
		}
		logger.Info("VeleroBackup successfully created")
	} else if veleroBackup.Annotations == nil || veleroBackup.Annotations[constant.NabOriginNamespaceAnnotation] != nab.Namespace {
		err = errors.New("related Velero Backup does not point to NonAdminBackup namespace")
		return false, reconcile.TerminalError(err)
	}

	updatedQueueInfo := false

	// Determine how many Backups are scheduled before the given VeleroBackup in the OADP namespace.
	queueInfo, err := function.GetBackupQueueInfo(ctx, r.Client, r.OADPNamespace, veleroBackup)
	if err != nil {
		// Log error and continue with the reconciliation, this is not critical error as it's just
		// about the Velero Backup queue position information
		logger.Error(err, "Failed to get the queue position for the VeleroBackup")
	} else {
		nab.Status.QueueInfo = &queueInfo
		updatedQueueInfo = true
	}

	updatedPhase := updateNonAdminPhase(&nab.Status.Phase, nacv1alpha1.NonAdminPhaseCreated)

	updatedCondition := meta.SetStatusCondition(&nab.Status.Conditions,
		metav1.Condition{
			Type:    string(nacv1alpha1.NonAdminConditionQueued),
			Status:  metav1.ConditionTrue,
			Reason:  "BackupScheduled",
			Message: "Created Velero Backup object",
		},
	)

	// Ensure that the NonAdminBackup's NonAdminBackupStatus is in sync
	// with the VeleroBackup. Any required updates to the NonAdminBackup
	// Status will be applied based on the current state of the VeleroBackup.
	updated := updateNonAdminBackupVeleroBackupSpecStatus(&nab.Status, veleroBackup)

	podVolumeBackups := &velerov1.PodVolumeBackupList{}
	err = r.List(ctx, podVolumeBackups, &client.ListOptions{
		Namespace:     r.OADPNamespace,
		LabelSelector: labels.SelectorFromSet(labels.Set{velerov1.BackupNameLabel: label.GetValidName(veleroBackup.Name)}),
	})
	if err != nil {
		// Log error and continue with the reconciliation, this is not critical error
		logger.Error(err, "Failed to list PodVolumeBackups in OADP namespace")
	}
	updatedPodVolumeBackupStatus := updateNonAdminBackupPodVolumeBackupStatus(&nab.Status, podVolumeBackups)

	dataUploads := &velerov2alpha1.DataUploadList{}
	err = r.List(ctx, dataUploads, &client.ListOptions{
		Namespace:     r.OADPNamespace,
		LabelSelector: labels.SelectorFromSet(labels.Set{velerov1.BackupNameLabel: label.GetValidName(veleroBackup.Name)}),
	})
	if err != nil {
		// Log error and continue with the reconciliation, this is not critical error
		logger.Error(err, "Failed to list DataUploads in OADP namespace")
	}
	updatedDataUploadStatus := updateNonAdminBackupDataUploadStatus(&nab.Status, dataUploads)

	if updated || updatedPhase || updatedCondition || updatedQueueInfo || updatedPodVolumeBackupStatus || updatedDataUploadStatus {
		if err := r.Status().Update(ctx, nab); err != nil {
			logger.Error(err, statusUpdateError)
			return false, err
		}
		logger.V(1).Info(statusUpdateExit)
	} else {
		logger.V(1).Info("NonAdminBackup status unchanged during VeleroBackup reconciliation")
	}

	return false, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NonAdminBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nacv1alpha1.NonAdminBackup{}).
		WithEventFilter(predicate.CompositeBackupPredicate{
			NonAdminBackupPredicate: predicate.NonAdminBackupPredicate{},
			VeleroBackupQueuePredicate: predicate.VeleroBackupQueuePredicate{
				OADPNamespace: r.OADPNamespace,
			},
			VeleroBackupPredicate: predicate.VeleroBackupPredicate{
				OADPNamespace: r.OADPNamespace,
			},
			VeleroPodVolumeBackupPredicate: predicate.VeleroPodVolumeBackupPredicate{
				Client:        r.Client,
				OADPNamespace: r.OADPNamespace,
			},
			VeleroDataUploadPredicate: predicate.VeleroDataUploadPredicate{
				Client:        r.Client,
				OADPNamespace: r.OADPNamespace,
			},
		}).
		// handler runs after predicate
		Watches(&velerov1.Backup{}, &handler.VeleroBackupHandler{}).
		Watches(&velerov1.Backup{}, &handler.VeleroBackupQueueHandler{
			Client:        r.Client,
			OADPNamespace: r.OADPNamespace,
		}).
		Watches(&velerov1.PodVolumeBackup{}, &handler.VeleroPodVolumeBackupHandler{
			Client:        r.Client,
			OADPNamespace: r.OADPNamespace,
		}).
		Watches(&velerov2alpha1.DataUpload{}, &handler.VeleroDataUploadHandler{
			Client:        r.Client,
			OADPNamespace: r.OADPNamespace,
		}).
		Complete(r)
}

// updateNonAdminPhase sets the phase in NonAdmin object status and returns true
// if the phase is changed by this call.
func updateNonAdminPhase(phase *nacv1alpha1.NonAdminPhase, newPhase nacv1alpha1.NonAdminPhase) bool {
	if *phase == newPhase {
		return false
	}

	*phase = newPhase
	return true
}

// updateNonAdminBackupVeleroBackupSpecStatus sets the VeleroBackup spec and status fields in NonAdminBackup object status and returns true
// if the VeleroBackup fields are changed by this call.
func updateNonAdminBackupVeleroBackupSpecStatus(status *nacv1alpha1.NonAdminBackupStatus, veleroBackup *velerov1.Backup) bool {
	if status == nil || veleroBackup == nil {
		return false
	}

	if status.VeleroBackup == nil {
		status.VeleroBackup = &nacv1alpha1.VeleroBackup{}
	}

	if status.VeleroBackup.Spec == nil {
		status.VeleroBackup.Spec = &velerov1.BackupSpec{}
	}
	if status.VeleroBackup.Status == nil {
		status.VeleroBackup.Status = &velerov1.BackupStatus{}
	}

	if reflect.DeepEqual(*status.VeleroBackup.Spec, veleroBackup.Spec) &&
		reflect.DeepEqual(*status.VeleroBackup.Status, veleroBackup.Status) {
		return false
	}

	status.VeleroBackup.Spec = veleroBackup.Spec.DeepCopy()
	status.VeleroBackup.Status = veleroBackup.Status.DeepCopy()
	return true
}

// updateNonAdminBackupDeleteBackupRequestStatus sets the VeleroDeleteBackupRequest status field in NonAdminBackup object status and returns true
// if the VeleroDeleteBackupRequest fields are changed by this call.
func updateNonAdminBackupDeleteBackupRequestStatus(status *nacv1alpha1.NonAdminBackupStatus, veleroDeleteBackupRequest *velerov1.DeleteBackupRequest) bool {
	if status == nil || veleroDeleteBackupRequest == nil {
		return false
	}

	if status.VeleroDeleteBackupRequest == nil {
		status.VeleroDeleteBackupRequest = &nacv1alpha1.VeleroDeleteBackupRequest{}
	}

	if status.VeleroDeleteBackupRequest.Status == nil {
		status.VeleroDeleteBackupRequest.Status = &velerov1.DeleteBackupRequestStatus{}
	}

	if reflect.DeepEqual(*status.VeleroDeleteBackupRequest.Status, veleroDeleteBackupRequest.Status) {
		return false
	}

	status.VeleroDeleteBackupRequest.Status = veleroDeleteBackupRequest.Status.DeepCopy()
	return true
}

func updateNonAdminBackupPodVolumeBackupStatus(status *nacv1alpha1.NonAdminBackupStatus, podVolumeBackupList *velerov1.PodVolumeBackupList) bool {
	if status.FileSystemPodVolumeBackups == nil {
		status.FileSystemPodVolumeBackups = &nacv1alpha1.FileSystemPodVolumeBackups{}
	}

	updated := false
	if len(podVolumeBackupList.Items) != status.FileSystemPodVolumeBackups.Total {
		status.FileSystemPodVolumeBackups.Total = len(podVolumeBackupList.Items)
		updated = true
	}
	numberOfNew := 0
	numberOfInProgress := 0
	numberOfFailed := 0
	numberOfCompleted := 0
	for _, podVolumeBackup := range podVolumeBackupList.Items {
		switch podVolumeBackup.Status.Phase {
		case velerov1.PodVolumeBackupPhaseNew:
			numberOfNew++
		case velerov1.PodVolumeBackupPhaseInProgress:
			numberOfInProgress++
		case velerov1.PodVolumeBackupPhaseFailed:
			numberOfFailed++
		case velerov1.PodVolumeBackupPhaseCompleted:
			numberOfCompleted++
		default:
			continue
		}
	}
	if status.FileSystemPodVolumeBackups.New != numberOfNew {
		status.FileSystemPodVolumeBackups.New = numberOfNew
		updated = true
	}
	if status.FileSystemPodVolumeBackups.InProgress != numberOfInProgress {
		status.FileSystemPodVolumeBackups.InProgress = numberOfInProgress
		updated = true
	}
	if status.FileSystemPodVolumeBackups.Failed != numberOfFailed {
		status.FileSystemPodVolumeBackups.Failed = numberOfFailed
		updated = true
	}
	if status.FileSystemPodVolumeBackups.Completed != numberOfCompleted {
		status.FileSystemPodVolumeBackups.Completed = numberOfCompleted
		updated = true
	}

	return updated
}

func updateNonAdminBackupDataUploadStatus(status *nacv1alpha1.NonAdminBackupStatus, dataUploadList *velerov2alpha1.DataUploadList) bool {
	if status.DataMoverDataUploads == nil {
		status.DataMoverDataUploads = &nacv1alpha1.DataMoverDataUploads{}
	}

	updated := false
	if len(dataUploadList.Items) != status.DataMoverDataUploads.Total {
		status.DataMoverDataUploads.Total = len(dataUploadList.Items)
		updated = true
	}
	numberOfNew := 0
	numberOfAccepted := 0
	numberOfPrepared := 0
	numberOfInProgress := 0
	numberOfCanceling := 0
	numberOfCanceled := 0
	numberOfFailed := 0
	numberOfCompleted := 0
	for _, dataUpload := range dataUploadList.Items {
		switch dataUpload.Status.Phase {
		case velerov2alpha1.DataUploadPhaseNew:
			numberOfNew++
		case velerov2alpha1.DataUploadPhaseAccepted:
			numberOfAccepted++
		case velerov2alpha1.DataUploadPhasePrepared:
			numberOfPrepared++
		case velerov2alpha1.DataUploadPhaseInProgress:
			numberOfInProgress++
		case velerov2alpha1.DataUploadPhaseCanceling:
			numberOfCanceling++
		case velerov2alpha1.DataUploadPhaseCanceled:
			numberOfCanceled++
		case velerov2alpha1.DataUploadPhaseFailed:
			numberOfFailed++
		case velerov2alpha1.DataUploadPhaseCompleted:
			numberOfCompleted++
		default:
			continue
		}
	}
	if status.DataMoverDataUploads.New != numberOfNew {
		status.DataMoverDataUploads.New = numberOfNew
		updated = true
	}
	if status.DataMoverDataUploads.Accepted != numberOfAccepted {
		status.DataMoverDataUploads.Accepted = numberOfAccepted
		updated = true
	}
	if status.DataMoverDataUploads.Prepared != numberOfPrepared {
		status.DataMoverDataUploads.Prepared = numberOfPrepared
		updated = true
	}
	if status.DataMoverDataUploads.InProgress != numberOfInProgress {
		status.DataMoverDataUploads.InProgress = numberOfInProgress
		updated = true
	}
	if status.DataMoverDataUploads.Canceling != numberOfCanceling {
		status.DataMoverDataUploads.Canceling = numberOfCanceling
		updated = true
	}
	if status.DataMoverDataUploads.Canceled != numberOfCanceled {
		status.DataMoverDataUploads.Canceled = numberOfCanceled
		updated = true
	}
	if status.DataMoverDataUploads.Failed != numberOfFailed {
		status.DataMoverDataUploads.Failed = numberOfFailed
		updated = true
	}
	if status.DataMoverDataUploads.Completed != numberOfCompleted {
		status.DataMoverDataUploads.Completed = numberOfCompleted
		updated = true
	}

	return updated
}
