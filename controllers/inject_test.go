package controllers

import (
	"testing"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
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

func TestInjectVLANConfig(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
cluster:
  clusterName: test
`)
	vlanCfg := &infrav1.VLANConfig{
		ID:           4000,
		Interface:    "enp193s0f0np0",
		PrefixLength: 24,
	}

	result, err := injectVLANConfig(input, vlanCfg, "10.10.0.1", "aa:bb:cc:dd:ee:ff", "138.199.242.217", "138.199.242.129")
	if err != nil {
		t.Fatalf("injectVLANConfig failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	machine := config["machine"].(map[string]interface{})
	network := machine["network"].(map[string]interface{})
	interfaces := network["interfaces"].([]interface{})

	if len(interfaces) != 1 {
		t.Fatalf("expected 1 interface, got %d", len(interfaces))
	}

	iface := interfaces[0].(map[string]interface{})
	// injectVLANConfig uses deviceSelector by MAC, not interface name
	selector := iface["deviceSelector"].(map[string]interface{})
	if selector["hardwareAddr"] != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("expected deviceSelector.hardwareAddr aa:bb:cc:dd:ee:ff, got %v", selector["hardwareAddr"])
	}

	// Static /32 address instead of DHCP
	if iface["dhcp"] != nil {
		t.Errorf("expected no dhcp field (static config), got %v", iface["dhcp"])
	}
	parentAddrs := iface["addresses"].([]interface{})
	if len(parentAddrs) != 1 || parentAddrs[0] != "138.199.242.217/32" {
		t.Errorf("expected parent address 138.199.242.217/32, got %v", parentAddrs)
	}

	// Static routes: default route + on-link route to gateway
	routes := iface["routes"].([]interface{})
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
	defaultRoute := routes[0].(map[string]interface{})
	if defaultRoute["network"] != "0.0.0.0/0" || defaultRoute["gateway"] != "138.199.242.129" {
		t.Errorf("expected default route via 138.199.242.129, got %v", defaultRoute)
	}
	onlinkRoute := routes[1].(map[string]interface{})
	if onlinkRoute["network"] != "138.199.242.129/32" {
		t.Errorf("expected on-link route 138.199.242.129/32, got %v", onlinkRoute)
	}
	if _, hasGW := onlinkRoute["gateway"]; hasGW {
		t.Errorf("on-link route should not have gateway field, got %v", onlinkRoute["gateway"])
	}

	vlans := iface["vlans"].([]interface{})
	if len(vlans) != 1 {
		t.Fatalf("expected 1 vlan, got %d", len(vlans))
	}

	vlan := vlans[0].(map[string]interface{})
	if vlan["vlanId"] != 4000 {
		t.Errorf("expected vlanId 4000, got %v", vlan["vlanId"])
	}

	addresses := vlan["addresses"].([]interface{})
	if len(addresses) != 1 || addresses[0] != "10.10.0.1/24" {
		t.Errorf("expected address 10.10.0.1/24, got %v", addresses)
	}
}

func TestInjectVLANConfig_NilConfig(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectVLANConfig(input, nil, "10.10.0.1", "aa:bb:cc:dd:ee:ff", "138.199.242.217", "138.199.242.129")
	if err != nil {
		t.Fatalf("injectVLANConfig failed: %v", err)
	}
	if string(result) != string(input) {
		t.Error("nil VLANConfig should return input unchanged")
	}
}

func TestInjectVLANConfig_EmptyInternalIP(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	vlanCfg := &infrav1.VLANConfig{ID: 4000, Interface: "eth0"}
	result, err := injectVLANConfig(input, vlanCfg, "", "aa:bb:cc:dd:ee:ff", "138.199.242.217", "138.199.242.129")
	if err != nil {
		t.Fatalf("injectVLANConfig failed: %v", err)
	}
	if string(result) != string(input) {
		t.Error("empty internalIP should return input unchanged")
	}
}

func TestInjectVLANConfig_MergeExistingInterface(t *testing.T) {
	// Existing config already has the interface matched by deviceSelector MAC
	input := []byte(`machine:
  network:
    interfaces:
      - deviceSelector:
          hardwareAddr: "aa:bb:cc:dd:ee:ff"
        mtu: 9000
`)
	vlanCfg := &infrav1.VLANConfig{
		ID:           4000,
		Interface:    "enp193s0f0np0",
		PrefixLength: 24,
	}

	result, err := injectVLANConfig(input, vlanCfg, "10.10.0.2", "aa:bb:cc:dd:ee:ff", "138.199.242.218", "138.199.242.129")
	if err != nil {
		t.Fatalf("injectVLANConfig failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	machine := config["machine"].(map[string]interface{})
	network := machine["network"].(map[string]interface{})
	interfaces := network["interfaces"].([]interface{})

	// Should still be 1 interface (merged, not duplicated)
	if len(interfaces) != 1 {
		t.Fatalf("expected 1 interface (merged), got %d", len(interfaces))
	}

	iface := interfaces[0].(map[string]interface{})
	// Original settings preserved
	if iface["mtu"] != 9000 {
		t.Errorf("expected mtu 9000 preserved, got %v", iface["mtu"])
	}
	// No DHCP — static /32 config
	if iface["dhcp"] != nil {
		t.Errorf("expected no dhcp field (static config), got %v", iface["dhcp"])
	}
	// Static /32 address
	parentAddrs := iface["addresses"].([]interface{})
	if len(parentAddrs) != 1 || parentAddrs[0] != "138.199.242.218/32" {
		t.Errorf("expected parent address 138.199.242.218/32, got %v", parentAddrs)
	}
	// Static routes
	routes := iface["routes"].([]interface{})
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
	// VLAN added
	vlans := iface["vlans"].([]interface{})
	if len(vlans) != 1 {
		t.Fatalf("expected 1 vlan, got %d", len(vlans))
	}
}

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

func TestInjectVLANConfig_DefaultPrefixLength(t *testing.T) {
	input := []byte(`machine: {}`)
	vlanCfg := &infrav1.VLANConfig{
		ID:        4000,
		Interface: "eth0",
		// PrefixLength not set — should default to 24
	}

	result, err := injectVLANConfig(input, vlanCfg, "10.10.0.3", "aa:bb:cc:dd:ee:ff", "1.2.3.4", "1.2.3.1")
	if err != nil {
		t.Fatalf("injectVLANConfig failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	machine := config["machine"].(map[string]interface{})
	network := machine["network"].(map[string]interface{})
	interfaces := network["interfaces"].([]interface{})
	iface := interfaces[0].(map[string]interface{})
	vlans := iface["vlans"].([]interface{})
	vlan := vlans[0].(map[string]interface{})
	addresses := vlan["addresses"].([]interface{})
	if addresses[0] != "10.10.0.3/24" {
		t.Errorf("expected /24 default prefix, got %v", addresses[0])
	}
}

// ─── injectIPv6Config ──────────────────────────────────────────────────────

func TestInjectIPv6Config(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectIPv6Config(input, "2a01:4f8:271:3b49::", "aa:bb:cc:dd:ee:ff", "10.10.0.1")
	if err != nil {
		t.Fatalf("injectIPv6Config failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	machine := config["machine"].(map[string]interface{})
	network := machine["network"].(map[string]interface{})
	interfaces := network["interfaces"].([]interface{})

	if len(interfaces) != 1 {
		t.Fatalf("expected 1 interface, got %d", len(interfaces))
	}

	iface := interfaces[0].(map[string]interface{})
	// Uses deviceSelector by MAC, not interface name
	deviceSelector := iface["deviceSelector"].(map[string]interface{})
	if deviceSelector == nil {
		t.Errorf("expected deviceSelector, got nil")
	}

	addrs := iface["addresses"].([]interface{})
	if len(addrs) != 1 || addrs[0] != "2a01:4f8:271:3b49::1/64" {
		t.Errorf("expected 2a01:4f8:271:3b49::1/64, got %v", addrs)
	}

	routes := iface["routes"].([]interface{})
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}
	route := routes[0].(map[string]interface{})
	if route["network"] != "::/0" || route["gateway"] != "fe80::1" {
		t.Errorf("expected ::/0 via fe80::1, got %v", route)
	}

	// Verify sysctl
	sysctls := machine["sysctls"].(map[string]interface{})
	if sysctls["net.ipv6.conf.all.forwarding"] != "1" {
		t.Errorf("expected ipv6 forwarding sysctl, got %v", sysctls)
	}
}

func TestInjectIPv6Config_Empty(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectIPv6Config(input, "", "aa:bb:cc:dd:ee:ff", "10.10.0.1")
	if err != nil {
		t.Fatalf("injectIPv6Config failed: %v", err)
	}
	if string(result) != string(input) {
		t.Error("empty ipv6Net should return input unchanged")
	}
}

func TestInjectIPv6Config_MergeExistingInterface(t *testing.T) {
	// Existing config uses deviceSelector with MAC — injectIPv6Config matches by hardwareAddr
	input := []byte(`machine:
  network:
    interfaces:
      - deviceSelector:
          hardwareAddr: aa:bb:cc:dd:ee:ff
        dhcp: true
        addresses:
          - 10.0.0.1/24
`)
	result, err := injectIPv6Config(input, "2a01:4f8::", "aa:bb:cc:dd:ee:ff", "10.10.0.1")
	if err != nil {
		t.Fatalf("injectIPv6Config failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	machine := config["machine"].(map[string]interface{})
	network := machine["network"].(map[string]interface{})
	interfaces := network["interfaces"].([]interface{})

	if len(interfaces) != 1 {
		t.Fatalf("expected 1 interface (merged), got %d", len(interfaces))
	}

	iface := interfaces[0].(map[string]interface{})
	addrs := iface["addresses"].([]interface{})
	// Should have both existing IPv4 + new IPv6
	if len(addrs) != 2 {
		t.Fatalf("expected 2 addresses (IPv4+IPv6), got %d: %v", len(addrs), addrs)
	}
	if addrs[0] != "10.0.0.1/24" {
		t.Errorf("expected existing IPv4 preserved, got %v", addrs[0])
	}
	if addrs[1] != "2a01:4f8::1/64" {
		t.Errorf("expected IPv6 appended, got %v", addrs[1])
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
