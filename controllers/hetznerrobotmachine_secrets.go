package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"gopkg.in/yaml.v3"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
	"github.com/SylphxAI/cluster-api-provider-hetzner-robot/pkg/robot"
)


// talosSecretBundle represents the relevant fields from the Talos secret bundle.
type talosSecretBundle struct {
	Secrets struct {
		SecretboxEncryptionSecret string `yaml:"secretboxencryptionsecret"`
		K8sServiceAccount        struct {
			Key string `yaml:"key"`
		} `yaml:"k8sserviceaccount"`
	} `yaml:"secrets"`
}


// getBootstrapData retrieves the bootstrap data from the machine's bootstrap secret.
func (r *HetznerRobotMachineReconciler) getBootstrapData(ctx context.Context, machine *clusterv1.Machine) ([]byte, error) {
	if machine.Spec.Bootstrap.DataSecretName == nil {
		return nil, fmt.Errorf("bootstrap data secret not yet available on machine %s", machine.Name)
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: machine.Namespace,
		Name:      *machine.Spec.Bootstrap.DataSecretName,
	}, secret); err != nil {
		return nil, fmt.Errorf("get bootstrap secret %s: %w", *machine.Spec.Bootstrap.DataSecretName, err)
	}

	data, ok := secret.Data["value"]
	if !ok {
		return nil, fmt.Errorf("bootstrap secret %s has no 'value' key", *machine.Spec.Bootstrap.DataSecretName)
	}
	return data, nil
}

// getTalosSecretBundle reads and parses the Talos secret bundle from the cluster-level secret.
// Returns nil if TalosSecretRef is not configured.
func (r *HetznerRobotMachineReconciler) getTalosSecretBundle(ctx context.Context, hrc *infrav1.HetznerRobotCluster) (*talosSecretBundle, error) {
	if hrc.Spec.TalosSecretRef == nil {
		return nil, nil
	}

	secret := &corev1.Secret{}
	ns := hrc.Spec.TalosSecretRef.Namespace
	if ns == "" {
		ns = hrc.Namespace
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: hrc.Spec.TalosSecretRef.Name}, secret); err != nil {
		return nil, fmt.Errorf("get talos secret %s/%s: %w", ns, hrc.Spec.TalosSecretRef.Name, err)
	}

	bundle, ok := secret.Data["bundle"]
	if !ok {
		return nil, fmt.Errorf("talos secret %s has no 'bundle' key", hrc.Spec.TalosSecretRef.Name)
	}

	var bundleData talosSecretBundle
	if err := yaml.Unmarshal(bundle, &bundleData); err != nil {
		return nil, fmt.Errorf("parse talos secret bundle: %w", err)
	}

	return &bundleData, nil
}

// buildRobotClient creates a Robot API client from the HRC's secret.
func (r *HetznerRobotMachineReconciler) buildRobotClient(ctx context.Context, hrc *infrav1.HetznerRobotCluster) (*robot.Client, error) {
	return robot.NewFromCluster(ctx, r.Client, hrc)
}

// getSSHPrivateKey retrieves the SSH private key from the HRC's SSH secret.
func (r *HetznerRobotMachineReconciler) getSSHPrivateKey(ctx context.Context, hrc *infrav1.HetznerRobotCluster) ([]byte, error) {
	secret := &corev1.Secret{}
	ns := hrc.Spec.SSHSecretRef.Namespace
	if ns == "" {
		ns = hrc.Namespace
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: hrc.Spec.SSHSecretRef.Name}, secret); err != nil {
		return nil, fmt.Errorf("get SSH secret %s/%s: %w", ns, hrc.Spec.SSHSecretRef.Name, err)
	}
	key, ok := secret.Data["ssh-privatekey"]
	if !ok {
		return nil, fmt.Errorf("SSH secret %s has no 'ssh-privatekey' key", hrc.Spec.SSHSecretRef.Name)
	}
	return key, nil
}

// getSSHKeyFingerprint retrieves the SSH public key fingerprint from the HRC's SSH secret.
// Returns empty string if not available (auth falls back to password from rescue activation).
func (r *HetznerRobotMachineReconciler) getSSHKeyFingerprint(ctx context.Context, hrc *infrav1.HetznerRobotCluster) (string, error) {
	secret := &corev1.Secret{}
	ns := hrc.Spec.SSHSecretRef.Namespace
	if ns == "" {
		ns = hrc.Namespace
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: hrc.Spec.SSHSecretRef.Name}, secret); err != nil {
		return "", fmt.Errorf("get SSH secret: %w", err)
	}
	return string(secret.Data["ssh-fingerprint"]), nil
}

