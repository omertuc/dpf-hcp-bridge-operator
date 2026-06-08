package dpuworkerocpconfiguratorcontroller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	provisioningv1alpha1 "github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/api/v1alpha1"
	"github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/internal/common"
)

// CleanupHandler handles cleanup of the DPU worker configuration DaemonSet
// when a DPFHCPProvisioner CR is deleted.
type CleanupHandler struct {
	client    client.Client
	recorder  record.EventRecorder
	namespace string
}

// NewCleanupHandler creates a new cleanup handler for DPU worker configuration resources.
func NewCleanupHandler(c client.Client, recorder record.EventRecorder, namespace string) *CleanupHandler {
	return &CleanupHandler{
		client:    c,
		recorder:  recorder,
		namespace: namespace,
	}
}

func (h *CleanupHandler) Name() string {
	return "dpu-worker-ocp-configurator"
}

func (h *CleanupHandler) Cleanup(ctx context.Context, cr *provisioningv1alpha1.DPFHCPProvisioner) error {
	log := logf.FromContext(ctx).WithValues(
		"handler", h.Name(),
		common.DPFHCPProvisionerName, fmt.Sprintf("%s/%s", cr.Namespace, cr.Name),
	)

	log.Info("Cleaning up DPU worker configuration DaemonSet")

	ds := &appsv1.DaemonSet{}
	err := h.client.Get(ctx, client.ObjectKey{
		Name:      DaemonSetName,
		Namespace: h.namespace,
	}, ds)

	if err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("DaemonSet already deleted or never existed")
			return nil
		}
		return fmt.Errorf("getting DaemonSet: %w", err)
	}

	if !common.IsOwnedByProvisioner(ds.Labels, cr.Name, cr.Namespace) {
		log.Info("Skipping DaemonSet deletion - not owned by this DPFHCPProvisioner")
		return nil
	}

	log.Info("Deleting DaemonSet", "name", ds.Name, "namespace", ds.Namespace)
	if err := h.client.Delete(ctx, ds); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("deleting DaemonSet: %w", err)
	}

	log.Info("DPU worker configuration cleanup completed")
	h.recorder.Event(cr, "Normal", "DPUWorkerCleanupComplete", "DPU worker configuration DaemonSet cleaned up")

	return nil
}
