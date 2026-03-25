package robot

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
)

// NewFromCluster creates a Robot API client by reading credentials from the
// HetznerRobotCluster's robotSecretRef. This is the single source of truth
// for constructing authenticated Robot clients from CAPI cluster objects.
func NewFromCluster(ctx context.Context, k8sClient client.Reader, hrc *infrav1.HetznerRobotCluster) (*Client, error) {
	secret := &corev1.Secret{}
	ns := hrc.Spec.RobotSecretRef.Namespace
	if ns == "" {
		ns = hrc.Namespace
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: hrc.Spec.RobotSecretRef.Name}, secret); err != nil {
		return nil, fmt.Errorf("get robot secret %s/%s: %w", ns, hrc.Spec.RobotSecretRef.Name, err)
	}
	return New(string(secret.Data["robot-user"]), string(secret.Data["robot-password"])), nil
}
