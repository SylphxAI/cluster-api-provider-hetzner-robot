package controllers

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInjectInstallDisk(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
cluster:
  clusterName: test
`)
	result, err := injectInstallDisk(input, "/dev/nvme0n1")
	if err != nil {
		t.Fatalf("injectInstallDisk failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	machine := config["machine"].(map[string]interface{})
	install := machine["install"].(map[string]interface{})
	if install["disk"] != "/dev/nvme0n1" {
		t.Errorf("expected /dev/nvme0n1, got %v", install["disk"])
	}
}

func TestInjectInstallDisk_DoesNotOverride(t *testing.T) {
	input := []byte(`machine:
  install:
    disk: /dev/sda
`)
	result, err := injectInstallDisk(input, "/dev/nvme0n1")
	if err != nil {
		t.Fatalf("injectInstallDisk failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	machine := config["machine"].(map[string]interface{})
	install := machine["install"].(map[string]interface{})
	if install["disk"] != "/dev/sda" {
		t.Errorf("should not override existing disk, got %v", install["disk"])
	}
}

// ─── injectPendingConfigTaint ──────────────────────────────────────────────

func TestInjectPendingConfigTaint(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectPendingConfigTaint(input)
	if err != nil {
		t.Fatalf("injectPendingConfigTaint failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	machine := config["machine"].(map[string]interface{})
	kubelet := machine["kubelet"].(map[string]interface{})
	extraArgs := kubelet["extraArgs"].(map[string]interface{})
	expected := "fleet.talos.dev/pending-config=true:NoSchedule"
	if extraArgs["register-with-taints"] != expected {
		t.Errorf("expected register-with-taints %q, got %v", expected, extraArgs["register-with-taints"])
	}
}

func TestInjectPendingConfigTaint_PreservesExistingConfig(t *testing.T) {
	input := []byte(`machine:
  kubelet:
    extraArgs:
      rotate-server-certificates: "true"
`)
	result, err := injectPendingConfigTaint(input)
	if err != nil {
		t.Fatalf("injectPendingConfigTaint failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	machine := config["machine"].(map[string]interface{})
	kubelet := machine["kubelet"].(map[string]interface{})
	extraArgs := kubelet["extraArgs"].(map[string]interface{})
	if extraArgs["rotate-server-certificates"] != "true" {
		t.Errorf("existing kubelet extraArgs should be preserved, got %v", extraArgs)
	}
	expected := "fleet.talos.dev/pending-config=true:NoSchedule"
	if extraArgs["register-with-taints"] != expected {
		t.Errorf("expected register-with-taints %q, got %v", expected, extraArgs["register-with-taints"])
	}
}

// ─── injectSecretboxEncryptionSecret ───────────────────────────────────────

func TestInjectSecretboxEncryptionSecret(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
cluster:
  clusterName: test
  secretboxEncryptionSecret: WRONG_KEY_FROM_CAPT
`)
	result, err := injectSecretboxEncryptionSecret(input, "CORRECT_CLUSTER_KEY")
	if err != nil {
		t.Fatalf("injectSecretboxEncryptionSecret failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	cluster := config["cluster"].(map[string]interface{})
	if cluster["secretboxEncryptionSecret"] != "CORRECT_CLUSTER_KEY" {
		t.Errorf("expected CORRECT_CLUSTER_KEY, got %v", cluster["secretboxEncryptionSecret"])
	}
	// Verify other cluster fields are preserved
	if cluster["clusterName"] != "test" {
		t.Errorf("clusterName was not preserved, got %v", cluster["clusterName"])
	}
}

func TestInjectSecretboxEncryptionSecret_Empty(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
cluster:
  clusterName: test
`)
	result, err := injectSecretboxEncryptionSecret(input, "")
	if err != nil {
		t.Fatalf("injectSecretboxEncryptionSecret failed: %v", err)
	}
	if string(result) != string(input) {
		t.Error("empty secret should return input unchanged")
	}
}

func TestInjectSecretboxEncryptionSecret_NoCluster(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectSecretboxEncryptionSecret(input, "SOME_KEY")
	if err != nil {
		t.Fatalf("injectSecretboxEncryptionSecret failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	cluster := config["cluster"].(map[string]interface{})
	if cluster["secretboxEncryptionSecret"] != "SOME_KEY" {
		t.Errorf("expected SOME_KEY, got %v", cluster["secretboxEncryptionSecret"])
	}
}

// ─── injectServiceAccountKey ───────────────────────────────────────────────

func TestInjectServiceAccountKey(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
cluster:
  clusterName: test
  serviceAccount:
    key: WRONG_KEY_FROM_CAPT
`)
	result, err := injectServiceAccountKey(input, "CORRECT_CLUSTER_SA_KEY")
	if err != nil {
		t.Fatalf("injectServiceAccountKey failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	cluster := config["cluster"].(map[string]interface{})
	sa := cluster["serviceAccount"].(map[string]interface{})
	if sa["key"] != "CORRECT_CLUSTER_SA_KEY" {
		t.Errorf("expected CORRECT_CLUSTER_SA_KEY, got %v", sa["key"])
	}
	// Verify other cluster fields are preserved
	if cluster["clusterName"] != "test" {
		t.Errorf("clusterName was not preserved, got %v", cluster["clusterName"])
	}
}

func TestInjectServiceAccountKey_Empty(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
cluster:
  clusterName: test
`)
	result, err := injectServiceAccountKey(input, "")
	if err != nil {
		t.Fatalf("injectServiceAccountKey failed: %v", err)
	}
	if string(result) != string(input) {
		t.Error("empty SA key should return input unchanged")
	}
}

func TestInjectServiceAccountKey_NoCluster(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectServiceAccountKey(input, "SOME_SA_KEY")
	if err != nil {
		t.Fatalf("injectServiceAccountKey failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	cluster := config["cluster"].(map[string]interface{})
	sa := cluster["serviceAccount"].(map[string]interface{})
	if sa["key"] != "SOME_SA_KEY" {
		t.Errorf("expected SOME_SA_KEY, got %v", sa["key"])
	}
}

func TestInjectServiceAccountKey_NoServiceAccount(t *testing.T) {
	input := []byte(`cluster:
  clusterName: test
`)
	result, err := injectServiceAccountKey(input, "SA_KEY_123")
	if err != nil {
		t.Fatalf("injectServiceAccountKey failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	cluster := config["cluster"].(map[string]interface{})
	sa := cluster["serviceAccount"].(map[string]interface{})
	if sa["key"] != "SA_KEY_123" {
		t.Errorf("expected SA_KEY_123, got %v", sa["key"])
	}
}

// ─── injectProviderID ──────────────────────────────────────────────────────

func TestInjectProviderID(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectProviderID(input, "hetzner-robot://2920324")
	if err != nil {
		t.Fatalf("injectProviderID failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	machine := config["machine"].(map[string]interface{})
	kubelet := machine["kubelet"].(map[string]interface{})
	extraArgs := kubelet["extraArgs"].(map[string]interface{})
	if extraArgs["provider-id"] != "hetzner-robot://2920324" {
		t.Errorf("expected provider-id hetzner-robot://2920324, got %v", extraArgs["provider-id"])
	}
}

func TestInjectProviderID_Empty(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectProviderID(input, "")
	if err != nil {
		t.Fatalf("injectProviderID failed: %v", err)
	}
	if string(result) != string(input) {
		t.Error("empty providerID should return input unchanged")
	}
}

func TestInjectProviderID_PreservesExistingKubeletConfig(t *testing.T) {
	input := []byte(`machine:
  kubelet:
    extraArgs:
      rotate-server-certificates: "true"
`)
	result, err := injectProviderID(input, "hetzner-robot://123")
	if err != nil {
		t.Fatalf("injectProviderID failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	machine := config["machine"].(map[string]interface{})
	kubelet := machine["kubelet"].(map[string]interface{})
	extraArgs := kubelet["extraArgs"].(map[string]interface{})
	if extraArgs["rotate-server-certificates"] != "true" {
		t.Errorf("existing kubelet extraArgs should be preserved, got %v", extraArgs)
	}
	if extraArgs["provider-id"] != "hetzner-robot://123" {
		t.Errorf("provider-id should be injected, got %v", extraArgs["provider-id"])
	}
}
