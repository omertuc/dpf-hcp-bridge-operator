package common

import (
	"context"
	"fmt"

	configv1 "github.com/openshift/api/config/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	WorkerDPUNodeRoleLabel = "node-role.kubernetes.io/worker-dpu"
	WorkerNodeRoleLabel    = "node-role.kubernetes.io/worker"
)

// GetDPUWorkerNodeRoleLabel returns the appropriate node role label for DPU worker nodes.
// On SingleReplica clusters, DPU workers use the "worker" role; otherwise "worker-dpu".
func GetDPUWorkerNodeRoleLabel(ctx context.Context, c client.Client) (string, error) {
	infra := &configv1.Infrastructure{}
	if err := c.Get(ctx, client.ObjectKey{Name: "cluster"}, infra); err != nil {
		return "", fmt.Errorf("fetching Infrastructure CR: %w", err)
	}

	if infra.Status.ControlPlaneTopology == configv1.SingleReplicaTopologyMode {
		return WorkerNodeRoleLabel, nil
	}

	return WorkerDPUNodeRoleLabel, nil
}
