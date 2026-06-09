package common

import (
	"context"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const saNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// GetOperatorImage discovers the running operator's container image by reading its own pod spec.
// If OPERATOR_IMAGE and OPERATOR_NAMESPACE environment variables are set, those are used instead
// of runtime detection (useful for local development with `make run`).
func GetOwnImage(ctx context.Context, reader client.Reader) (image, namespace string, err error) {
	if envImage := os.Getenv("OPERATOR_IMAGE"); envImage != "" {
		if envNs := os.Getenv("OPERATOR_NAMESPACE"); envNs != "" {
			return envImage, envNs, nil
		}
	}

	namespace, err = getInClusterNamespace()
	if err != nil {
		return "", "", fmt.Errorf("detecting operator namespace: %w", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return "", "", fmt.Errorf("getting hostname: %w", err)
	}

	pod := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKey{Name: hostname, Namespace: namespace}, pod); err != nil {
		return "", "", fmt.Errorf("getting operator pod %s/%s: %w", namespace, hostname, err)
	}

	for _, c := range pod.Spec.Containers {
		if c.Name == "manager" {
			return c.Image, namespace, nil
		}
	}

	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Image, namespace, nil
	}

	return "", "", fmt.Errorf("no containers found in pod %s/%s", namespace, hostname)
}

func getInClusterNamespace() (string, error) {
	data, err := os.ReadFile(saNamespaceFile)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", saNamespaceFile, err)
	}
	ns := strings.TrimSpace(string(data))
	if ns == "" {
		return "", fmt.Errorf("%s is empty", saNamespaceFile)
	}
	return ns, nil
}
