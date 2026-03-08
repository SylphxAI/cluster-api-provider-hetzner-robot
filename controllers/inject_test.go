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

	result, err := injectVLANConfig(input, vlanCfg, "10.10.0.1")
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
	if iface["interface"] != "enp193s0f0np0" {
		t.Errorf("expected interface enp193s0f0np0, got %v", iface["interface"])
	}
	if iface["dhcp"] != true {
		t.Errorf("expected dhcp true, got %v", iface["dhcp"])
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
	result, err := injectVLANConfig(input, nil, "10.10.0.1")
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
	result, err := injectVLANConfig(input, vlanCfg, "")
	if err != nil {
		t.Fatalf("injectVLANConfig failed: %v", err)
	}
	if string(result) != string(input) {
		t.Error("empty internalIP should return input unchanged")
	}
}

func TestInjectVLANConfig_MergeExistingInterface(t *testing.T) {
	// Existing config already has the interface with some settings
	input := []byte(`machine:
  network:
    interfaces:
      - interface: enp193s0f0np0
        mtu: 9000
`)
	vlanCfg := &infrav1.VLANConfig{
		ID:           4000,
		Interface:    "enp193s0f0np0",
		PrefixLength: 24,
	}

	result, err := injectVLANConfig(input, vlanCfg, "10.10.0.2")
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
	// dhcp: true injected
	if iface["dhcp"] != true {
		t.Errorf("expected dhcp true injected, got %v", iface["dhcp"])
	}
	// VLAN added
	vlans := iface["vlans"].([]interface{})
	if len(vlans) != 1 {
		t.Fatalf("expected 1 vlan, got %d", len(vlans))
	}
}

func TestInjectVLANConfig_DefaultPrefixLength(t *testing.T) {
	input := []byte(`machine: {}`)
	vlanCfg := &infrav1.VLANConfig{
		ID:        4000,
		Interface: "eth0",
		// PrefixLength not set — should default to 24
	}

	result, err := injectVLANConfig(input, vlanCfg, "10.10.0.3")
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
