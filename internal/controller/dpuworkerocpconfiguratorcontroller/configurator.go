package dpuworkerocpconfiguratorcontroller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	provisioningv1alpha1 "github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/api/v1alpha1"
	"github.com/rh-ecosystem-edge/dpf-hcp-provisioner-operator/internal/common"
)

const (
	DaemonSetName           = "dpu-worker-ocp-configurator"
	conditionType           = "DPUWorkerConfigured"
	initContainerName       = "setup"
	mainContainerName       = "p0-routing"
	legacyMachineConfigName = "dpu-worker-configuration"
)

// DPUWorkerOCPConfiguratorController manages a DaemonSet that configures networking on DPU worker nodes.
// It replaces the MachineConfig approach (which requires reboots) with a privileged DaemonSet
// that runs init containers for one-shot setup and a long-running container for route reconciliation.
type DPUWorkerOCPConfiguratorController struct {
	client    client.Client
	recorder  record.EventRecorder
	image     string
	namespace string
}

// New creates a new DPUWorkerOCPConfiguratorController.
// image is the operator image used for the DaemonSet pods.
// namespace is the operator namespace where the DaemonSet is created.
func New(c client.Client, recorder record.EventRecorder, image, namespace string) *DPUWorkerOCPConfiguratorController {
	return &DPUWorkerOCPConfiguratorController{
		client:    c,
		recorder:  recorder,
		image:     image,
		namespace: namespace,
	}
}

// EnsureDaemonSet creates or updates the DPU worker configuration DaemonSet.
// If the legacy MachineConfig from the old Helm-based approach exists, the DaemonSet is skipped
// to avoid conflicting with already-configured nodes.
func (d *DPUWorkerOCPConfiguratorController) EnsureDaemonSet(ctx context.Context, provisioner *provisioningv1alpha1.DPFHCPProvisioner, operatorConfig *common.OperatorConfig) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("provisioner", client.ObjectKeyFromObject(provisioner))

	skip, err := d.legacyMachineConfigExists(ctx)
	if err != nil {
		log.Error(err, "Failed to check for legacy MachineConfig")
		return ctrl.Result{}, err
	}
	if skip {
		log.Info("Legacy MachineConfig exists, skipping DaemonSet creation", "machineconfig", legacyMachineConfigName)
		if condErr := d.setCondition(ctx, provisioner, metav1.ConditionTrue, "LegacyMachineConfigExists",
			"Skipping DaemonSet — legacy MachineConfig already configures DPU worker nodes"); condErr != nil {
			log.Error(condErr, "Failed to update condition")
		}
		return ctrl.Result{}, nil
	}

	nodeRoleLabel, err := common.GetDPUWorkerNodeRoleLabel(ctx, d.client)
	if err != nil {
		log.Error(err, "Failed to determine DPU worker node role")
		if condErr := d.setCondition(ctx, provisioner, metav1.ConditionFalse, "InfrastructureDetectionFailed",
			fmt.Sprintf("Failed to detect cluster topology: %v", err)); condErr != nil {
			log.Error(condErr, "Failed to update condition")
		}
		return ctrl.Result{}, err
	}

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DaemonSetName,
			Namespace: d.namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, d.client, ds, func() error {
		if ds.Labels == nil {
			ds.Labels = make(map[string]string)
		}
		ds.Labels[common.LabelDPFHCPProvisionerName] = provisioner.Name
		ds.Labels[common.LabelDPFHCPProvisionerNamespace] = provisioner.Namespace

		d.buildDaemonSetSpec(ds, nodeRoleLabel, operatorConfig)
		return nil
	})

	if err != nil {
		log.Error(err, "Failed to ensure DaemonSet")
		if condErr := d.setCondition(ctx, provisioner, metav1.ConditionFalse, "DaemonSetFailed",
			fmt.Sprintf("Failed to create/update DaemonSet: %v", err)); condErr != nil {
			log.Error(condErr, "Failed to update condition")
		}
		return ctrl.Result{}, err
	}

	switch op {
	case controllerutil.OperationResultCreated:
		log.Info("Created DPU worker configuration DaemonSet", "name", DaemonSetName)
	case controllerutil.OperationResultUpdated:
		log.Info("Updated DPU worker configuration DaemonSet (drift corrected)", "name", DaemonSetName)
		d.recorder.Event(provisioner, "Normal", "DPUWorkerDriftCorrected",
			fmt.Sprintf("Corrected spec drift in DaemonSet %s", DaemonSetName))
	case controllerutil.OperationResultNone:
		log.V(1).Info("DaemonSet already matches desired state", "name", DaemonSetName)
	}

	if err := d.setCondition(ctx, provisioner, metav1.ConditionTrue, "DaemonSetReady",
		"DPU worker configuration DaemonSet is running"); err != nil {
		log.Error(err, "Failed to update condition")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (d *DPUWorkerOCPConfiguratorController) buildDaemonSetSpec(ds *appsv1.DaemonSet, nodeRoleLabel string, operatorConfig *common.OperatorConfig) {
	privileged := true
	hostPID := true

	mtuEnv := []corev1.EnvVar{}
	if operatorConfig != nil && operatorConfig.NetworkMTU != "" {
		mtuEnv = append(mtuEnv, corev1.EnvVar{
			Name:  "NETWORK_MTU",
			Value: operatorConfig.NetworkMTU,
		})
	}

	ds.Spec = appsv1.DaemonSetSpec{
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app.kubernetes.io/name": DaemonSetName,
			},
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"app.kubernetes.io/name":      DaemonSetName,
					"app.kubernetes.io/component": "dpu-worker-networking",
				},
			},
			Spec: corev1.PodSpec{
				HostPID:     hostPID,
				HostNetwork: true,
				NodeSelector: map[string]string{
					nodeRoleLabel: "",
				},
				Tolerations: []corev1.Toleration{
					{
						Key:      nodeRoleLabel,
						Operator: corev1.TolerationOpExists,
						Effect:   corev1.TaintEffectNoSchedule,
					},
				},
				InitContainers: []corev1.Container{
					{
						Name:    initContainerName,
						Image:   d.image,
						Command: []string{"/manager", "dpu-worker-configure", "setup"},
						Env:     mtuEnv,
						SecurityContext: &corev1.SecurityContext{
							Privileged: &privileged,
						},
					},
				},
				Containers: []corev1.Container{
					{
						Name:    mainContainerName,
						Image:   d.image,
						Command: []string{"/manager", "dpu-worker-configure", "p0-routing"},
						SecurityContext: &corev1.SecurityContext{
							Privileged: &privileged,
						},
					},
				},
			},
		},
	}
}

func (d *DPUWorkerOCPConfiguratorController) setCondition(ctx context.Context, provisioner *provisioningv1alpha1.DPFHCPProvisioner, status metav1.ConditionStatus, reason, message string) error {
	log := logf.FromContext(ctx)

	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: provisioner.Generation,
	}

	if changed := meta.SetStatusCondition(&provisioner.Status.Conditions, condition); changed {
		log.V(1).Info("Updating DPUWorkerConfigured condition",
			"status", status,
			"reason", reason,
			"message", message)

		eventType := "Normal"
		eventReason := "DPUWorkerConfigured"
		if status == metav1.ConditionFalse {
			eventType = "Warning"
			eventReason = "DPUWorkerConfigurationFailed"
		}
		d.recorder.Event(provisioner, eventType, eventReason, message)

		if err := d.client.Status().Update(ctx, provisioner); err != nil {
			return fmt.Errorf("updating DPUWorkerConfigured condition: %w", err)
		}
	}

	return nil
}

func (d *DPUWorkerOCPConfiguratorController) legacyMachineConfigExists(ctx context.Context) (bool, error) {
	mc := &unstructured.Unstructured{}
	mc.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "machineconfiguration.openshift.io",
		Version: "v1",
		Kind:    "MachineConfig",
	})
	err := d.client.Get(ctx, client.ObjectKey{Name: legacyMachineConfigName}, mc)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking for legacy MachineConfig: %w", err)
	}
	return true, nil
}
