package controllers

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
	"gopkg.in/yaml.v3"
)

// ─── modifyFirstDocument / splitYAMLDocuments ──────────────────────────────

func TestModifyFirstDocument_SingleDoc(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	result, err := modifyFirstDocument(input, func(config map[string]interface{}) error {
		machine := ensureMap(config, "machine")
		machine["hostname"] = "test-node"
		return nil
	})
	if err != nil {
		t.Fatalf("modifyFirstDocument failed: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	machine := config["machine"].(map[string]interface{})
	if machine["hostname"] != "test-node" {
		t.Errorf("expected hostname test-node, got %v", machine["hostname"])
	}
}

func TestModifyFirstDocument_MultiDoc_PreservesSubsequent(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
cluster:
  clusterName: test
---
apiVersion: v1alpha1
kind: VolumeConfig
name: EPHEMERAL
provisioning:
  maxSize: 100GB
---
apiVersion: v1alpha1
kind: RawVolumeConfig
name: osd-data
provisioning:
  diskSelector:
    match: system_disk
`)
	result, err := modifyFirstDocument(input, func(config map[string]interface{}) error {
		machine := ensureMap(config, "machine")
		install := ensureMap(machine, "install")
		install["disk"] = "/dev/nvme0n1"
		return nil
	})
	if err != nil {
		t.Fatalf("modifyFirstDocument failed: %v", err)
	}

	resultStr := string(result)

	// First document should have the modification
	if !strings.Contains(resultStr, "disk: /dev/nvme0n1") {
		t.Error("expected install disk in first document")
	}

	// VolumeConfig document must survive
	if !strings.Contains(resultStr, "kind: VolumeConfig") {
		t.Error("VolumeConfig document was dropped")
	}
	if !strings.Contains(resultStr, "maxSize: 100GB") {
		t.Error("VolumeConfig maxSize was dropped")
	}

	// RawVolumeConfig document must survive
	if !strings.Contains(resultStr, "kind: RawVolumeConfig") {
		t.Error("RawVolumeConfig document was dropped")
	}
	if !strings.Contains(resultStr, "name: osd-data") {
		t.Error("RawVolumeConfig name was dropped")
	}

	// Documents should be separated by ---
	if !strings.Contains(resultStr, "\n---\n") {
		t.Error("expected YAML document separator between documents")
	}
}

func TestModifyFirstDocument_Empty(t *testing.T) {
	_, err := modifyFirstDocument([]byte(""), func(config map[string]interface{}) error {
		return nil
	})
	if err == nil {
		t.Error("expected error for empty input")
	}
}

func TestSplitYAMLDocuments_Single(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	docs := splitYAMLDocuments(input)
	if len(docs) != 1 {
		t.Fatalf("expected 1 document, got %d", len(docs))
	}
}

func TestSplitYAMLDocuments_Multi(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
---
apiVersion: v1alpha1
kind: VolumeConfig
name: EPHEMERAL
---
apiVersion: v1alpha1
kind: RawVolumeConfig
name: osd-data
`)
	docs := splitYAMLDocuments(input)
	if len(docs) != 3 {
		t.Fatalf("expected 3 documents, got %d", len(docs))
	}
	if !bytes.Contains(docs[0], []byte("controlplane")) {
		t.Error("first doc should contain machineconfig")
	}
	if !bytes.Contains(docs[1], []byte("VolumeConfig")) {
		t.Error("second doc should contain VolumeConfig")
	}
	if !bytes.Contains(docs[2], []byte("RawVolumeConfig")) {
		t.Error("third doc should contain RawVolumeConfig")
	}
}

func TestSplitYAMLDocuments_LeadingSeparator(t *testing.T) {
	input := []byte(`---
machine:
  type: controlplane
---
apiVersion: v1alpha1
kind: VolumeConfig
`)
	docs := splitYAMLDocuments(input)
	if len(docs) != 2 {
		t.Fatalf("expected 2 documents, got %d", len(docs))
	}
}

// ─── Inject pipeline preserves multi-document YAML ─────────────────────────

func TestInjectPipeline_PreservesVolumeConfig(t *testing.T) {
	// Simulates a full inject pipeline on multi-document YAML from CABPT
	input := []byte(`machine:
  type: controlplane
cluster:
  clusterName: test
---
apiVersion: v1alpha1
kind: VolumeConfig
name: EPHEMERAL
provisioning:
  maxSize: 100GB
`)

	// Run through the inject pipeline (same order as stateApplyConfig)
	data, err := injectInstallDisk(input, "/dev/nvme0n1")
	if err != nil {
		t.Fatalf("injectInstallDisk: %v", err)
	}
	// injectHostname removed — hostname managed by CABPT HostnameConfig
	data, err = injectProviderID(data, "hetzner-robot://2938104")
	if err != nil {
		t.Fatalf("injectProviderID: %v", err)
	}

	resultStr := string(data)

	// First document should have all injections
	if !strings.Contains(resultStr, "disk: /dev/nvme0n1") {
		t.Error("install disk missing from first document")
	}
	// hostname no longer injected by CAPHR — managed by CABPT HostnameConfig
	if !strings.Contains(resultStr, "provider-id: hetzner-robot://2938104") {
		t.Error("provider-id missing from first document")
	}

	// VolumeConfig document must survive the entire pipeline
	if !strings.Contains(resultStr, "kind: VolumeConfig") {
		t.Error("VolumeConfig document was dropped during inject pipeline")
	}
	if !strings.Contains(resultStr, "maxSize: 100GB") {
		t.Error("VolumeConfig maxSize was dropped during inject pipeline")
	}
}

// ─── ensureMap ─────────────────────────────────────────────────────────────

func TestEnsureMap_ExistingKey(t *testing.T) {
	parent := map[string]interface{}{
		"machine": map[string]interface{}{"type": "controlplane"},
	}
	machine := ensureMap(parent, "machine")
	if machine["type"] != "controlplane" {
		t.Errorf("expected existing value preserved, got %v", machine["type"])
	}
}

func TestEnsureMap_MissingKey(t *testing.T) {
	parent := map[string]interface{}{}
	machine := ensureMap(parent, "machine")
	if machine == nil {
		t.Fatal("expected non-nil map")
	}
	machine["type"] = "worker"
	if parent["machine"].(map[string]interface{})["type"] != "worker" {
		t.Error("expected parent to reference the same map")
	}
}

// ─── injectInstallDisk ─────────────────────────────────────────────────────

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

	// injectVLANConfig now creates a DEDICATED interface entry with
	// deviceSelector{physical:true, hardwareAddr:MAC} and vlans.
	// No addresses/routes on parent — just MAC selector + VLAN sub-entry.
	if len(interfaces) != 1 {
		t.Fatalf("expected 1 interface (dedicated VLAN entry), got %d", len(interfaces))
	}

	iface := interfaces[0].(map[string]interface{})
	selector := iface["deviceSelector"].(map[string]interface{})
	if selector["hardwareAddr"] != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("expected deviceSelector.hardwareAddr aa:bb:cc:dd:ee:ff, got %v", selector["hardwareAddr"])
	}
	if selector["physical"] != true {
		t.Errorf("expected deviceSelector.physical true, got %v", selector["physical"])
	}

	// No addresses or routes on the parent interface — VLAN only.
	if iface["addresses"] != nil {
		t.Errorf("expected no addresses on parent interface, got %v", iface["addresses"])
	}
	if iface["routes"] != nil {
		t.Errorf("expected no routes on parent interface, got %v", iface["routes"])
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
	vlanCfg := &infrav1.VLANConfig{ID: 4000}
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

	// Current behavior: existing interface has vlans stripped, new dedicated VLAN entry appended.
	// 2 interfaces: original (vlans stripped) + dedicated VLAN entry.
	if len(interfaces) != 2 {
		t.Fatalf("expected 2 interfaces (original + dedicated VLAN), got %d", len(interfaces))
	}

	// First interface: original with vlans stripped, mtu preserved.
	origIface := interfaces[0].(map[string]interface{})
	if origIface["mtu"] != 9000 {
		t.Errorf("expected mtu 9000 preserved on original interface, got %v", origIface["mtu"])
	}
	if origIface["vlans"] != nil {
		t.Errorf("expected vlans stripped from original interface, got %v", origIface["vlans"])
	}

	// Second interface: dedicated VLAN entry with MAC + physical selector.
	vlanIface := interfaces[1].(map[string]interface{})
	selector := vlanIface["deviceSelector"].(map[string]interface{})
	if selector["hardwareAddr"] != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("expected deviceSelector.hardwareAddr aa:bb:cc:dd:ee:ff, got %v", selector["hardwareAddr"])
	}
	vlans := vlanIface["vlans"].([]interface{})
	if len(vlans) != 1 {
		t.Fatalf("expected 1 vlan on dedicated entry, got %d", len(vlans))
	}
}

func TestInjectVLANConfig_DefaultPrefixLength(t *testing.T) {
	input := []byte(`machine: {}`)
	vlanCfg := &infrav1.VLANConfig{
		ID: 4000,
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

func TestInjectIPv6Config_SeparateInterface(t *testing.T) {
	// injectIPv6Config creates a SEPARATE interface entry (not merged into existing).
	// Combining dhcp:true with static addresses on the same interface causes Talos failures.
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

	// 2 interfaces: original (unchanged) + new IPv6 entry.
	if len(interfaces) != 2 {
		t.Fatalf("expected 2 interfaces (original + IPv6), got %d", len(interfaces))
	}

	// Original interface unchanged.
	origIface := interfaces[0].(map[string]interface{})
	origAddrs := origIface["addresses"].([]interface{})
	if len(origAddrs) != 1 || origAddrs[0] != "10.0.0.1/24" {
		t.Errorf("expected original interface unchanged, got %v", origAddrs)
	}

	// New IPv6 interface with physical selector.
	ipv6Iface := interfaces[1].(map[string]interface{})
	ipv6Addrs := ipv6Iface["addresses"].([]interface{})
	if len(ipv6Addrs) != 1 || ipv6Addrs[0] != "2a01:4f8::1/64" {
		t.Errorf("expected IPv6 address 2a01:4f8::1/64, got %v", ipv6Addrs)
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

// ─── injectInstallDisk edge cases ──────────────────────────────────────────

func TestInjectInstallDisk_ExistingDiskPreserved(t *testing.T) {
	// When config already has a disk set, injectInstallDisk must NOT override it.
	// This verifies the exact preserved value (not just "not the default").
	input := []byte(`machine:
  install:
    disk: /dev/disk/by-id/nvme-SAMSUNG_MZQL21T9HCJR-00A07_S64GNE0W405037
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
	want := "/dev/disk/by-id/nvme-SAMSUNG_MZQL21T9HCJR-00A07_S64GNE0W405037"
	if install["disk"] != want {
		t.Errorf("existing disk should be preserved: got %v, want %v", install["disk"], want)
	}
}

func TestInjectInstallDisk_EmptyParamDefaultsToNvme(t *testing.T) {
	// When installDisk param is empty string, the function should default to /dev/nvme0n1.
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectInstallDisk(input, "")
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
		t.Errorf("empty installDisk param should default to /dev/nvme0n1, got %v", install["disk"])
	}
}

func TestInjectInstallDisk_MultiDocOnlyFirstModified(t *testing.T) {
	// In multi-document YAML, only the first document should get the disk injected.
	// Subsequent documents must remain untouched.
	input := []byte(`machine:
  type: controlplane
---
apiVersion: v1alpha1
kind: VolumeConfig
name: EPHEMERAL
`)
	result, err := injectInstallDisk(input, "/dev/sda")
	if err != nil {
		t.Fatalf("injectInstallDisk failed: %v", err)
	}

	resultStr := string(result)
	// First doc should have the install disk
	if !strings.Contains(resultStr, "disk: /dev/sda") {
		t.Error("expected install disk in first document")
	}
	// Second doc must survive intact
	if !strings.Contains(resultStr, "kind: VolumeConfig") {
		t.Error("VolumeConfig document was dropped")
	}
	if !strings.Contains(resultStr, "name: EPHEMERAL") {
		t.Error("VolumeConfig name was dropped")
	}

	// Verify the second document does NOT have an install disk —
	// split on separator and check the second part
	parts := strings.SplitN(resultStr, "---", 2)
	if len(parts) != 2 {
		t.Fatalf("expected 2 document parts, got %d", len(parts))
	}
	if strings.Contains(parts[1], "disk:") {
		t.Error("disk should NOT appear in the second document")
	}
}

// ─── injectHostname edge cases ─────────────────────────────────────────────

// injectHostname tests removed — hostname managed by CABPT HostnameConfig

// ─── injectVLANConfig edge cases ───────────────────────────────────────────

func TestInjectVLANConfig_NilConfig_NoOp(t *testing.T) {
	input := []byte(`machine:
  type: worker
  network:
    hostname: test-node
`)
	result, err := injectVLANConfig(input, nil, "10.10.0.5", "aa:bb:cc:dd:ee:ff", "1.2.3.4", "1.2.3.1")
	if err != nil {
		t.Fatalf("injectVLANConfig failed: %v", err)
	}
	if string(result) != string(input) {
		t.Error("nil vlanCfg should return input unchanged")
	}
}

func TestInjectVLANConfig_EmptyInternalIP_NoOp(t *testing.T) {
	input := []byte(`machine:
  type: worker
  network:
    hostname: test-node
`)
	vlanCfg := &infrav1.VLANConfig{ID: 4000, PrefixLength: 24}
	result, err := injectVLANConfig(input, vlanCfg, "", "aa:bb:cc:dd:ee:ff", "1.2.3.4", "1.2.3.1")
	if err != nil {
		t.Fatalf("injectVLANConfig failed: %v", err)
	}
	if string(result) != string(input) {
		t.Error("empty internalIP should return input unchanged")
	}
}

func TestInjectVLANConfig_DedicatedEntryWithMAC(t *testing.T) {
	// Existing interface with MAC + extra settings.
	// injectVLANConfig strips vlans from existing interfaces and creates a dedicated entry.
	input := []byte(`machine:
  network:
    interfaces:
      - deviceSelector:
          hardwareAddr: "11:22:33:44:55:66"
        mtu: 1500
        addresses:
          - 10.0.0.1/24
`)
	vlanCfg := &infrav1.VLANConfig{
		ID:           4001,
		PrefixLength: 28,
	}

	result, err := injectVLANConfig(input, vlanCfg, "10.10.0.10", "11:22:33:44:55:66", "5.6.7.8", "5.6.7.1")
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

	// 2 interfaces: original (vlans stripped) + dedicated VLAN entry.
	if len(interfaces) != 2 {
		t.Fatalf("expected 2 interfaces (original + dedicated VLAN), got %d", len(interfaces))
	}

	// Original interface: mtu preserved, vlans stripped.
	origIface := interfaces[0].(map[string]interface{})
	if origIface["mtu"] != 1500 {
		t.Errorf("expected mtu 1500 preserved, got %v", origIface["mtu"])
	}

	// Dedicated VLAN entry with MAC + physical selector.
	vlanIface := interfaces[1].(map[string]interface{})
	selector := vlanIface["deviceSelector"].(map[string]interface{})
	if selector["hardwareAddr"] != "11:22:33:44:55:66" {
		t.Errorf("expected hardwareAddr 11:22:33:44:55:66, got %v", selector["hardwareAddr"])
	}
	vlans := vlanIface["vlans"].([]interface{})
	if len(vlans) != 1 {
		t.Fatalf("expected 1 vlan, got %d", len(vlans))
	}
	vlan := vlans[0].(map[string]interface{})
	addresses := vlan["addresses"].([]interface{})
	if addresses[0] != "10.10.0.10/28" {
		t.Errorf("expected 10.10.0.10/28, got %v", addresses[0])
	}
}

func TestInjectVLANConfig_PrefixLengthZero_DefaultsTo24(t *testing.T) {
	input := []byte(`machine: {}`)
	vlanCfg := &infrav1.VLANConfig{
		ID:           4000,
				PrefixLength: 0, // explicitly zero
	}

	result, err := injectVLANConfig(input, vlanCfg, "10.10.0.99", "aa:bb:cc:dd:ee:ff", "1.2.3.4", "1.2.3.1")
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
	if addresses[0] != "10.10.0.99/24" {
		t.Errorf("PrefixLength=0 should default to /24, got %v", addresses[0])
	}
}

// ─── injectIPv6Config edge cases ───────────────────────────────────────────

func TestInjectIPv6Config_EmptyIPv6Net_NoOp(t *testing.T) {
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

func TestInjectIPv6Config_EmptyPrimaryMAC_StillInjects(t *testing.T) {
	// injectIPv6Config uses deviceSelector: {physical: true} — no MAC needed.
	// Empty primaryMAC does NOT prevent IPv6 injection (unlike VLAN which requires MAC).
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectIPv6Config(input, "2a01:4f8:271:3b49::", "", "10.10.0.1")
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
		t.Fatalf("expected 1 interface (IPv6), got %d", len(interfaces))
	}
	iface := interfaces[0].(map[string]interface{})
	addrs := iface["addresses"].([]interface{})
	if len(addrs) != 1 || addrs[0] != "2a01:4f8:271:3b49::1/64" {
		t.Errorf("expected IPv6 address, got %v", addrs)
	}
}

func TestInjectIPv6Config_TrailingDoubleColon(t *testing.T) {
	// IPv6 prefix with trailing "::" (most common from Hetzner API)
	// Should build "2a01:4f8:271:3b49::1/64"
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
	iface := interfaces[0].(map[string]interface{})
	addrs := iface["addresses"].([]interface{})
	if addrs[0] != "2a01:4f8:271:3b49::1/64" {
		t.Errorf("expected 2a01:4f8:271:3b49::1/64, got %v", addrs[0])
	}
}

func TestInjectIPv6Config_WithSlash64Suffix(t *testing.T) {
	// IPv6 prefix with "/64" suffix — the function should strip the prefix length before building the address.
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectIPv6Config(input, "2a01:4f8:2210:1a2e::/64", "aa:bb:cc:dd:ee:ff", "10.10.0.1")
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
	iface := interfaces[0].(map[string]interface{})
	addrs := iface["addresses"].([]interface{})
	// Must be ::1/64 — not ::/64::1/64 or other malformed address
	if addrs[0] != "2a01:4f8:2210:1a2e::1/64" {
		t.Errorf("expected 2a01:4f8:2210:1a2e::1/64, got %v", addrs[0])
	}
}

// ─── injectKubeletNodeIP ───────────────────────────────────────────────────

func TestInjectKubeletNodeIP_DualStack(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectKubeletNodeIP(input, "10.10.0.5", "2a01:4f8:271:3b49::/64")
	if err != nil {
		t.Fatalf("injectKubeletNodeIP failed: %v", err)
	}
	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	machine := config["machine"].(map[string]interface{})
	kubelet := machine["kubelet"].(map[string]interface{})
	extraArgs := kubelet["extraArgs"].(map[string]interface{})
	nodeIP := extraArgs["node-ip"].(string)
	if nodeIP != "10.10.0.5,2a01:4f8:271:3b49::1" {
		t.Errorf("expected dual-stack '10.10.0.5,2a01:4f8:271:3b49::1', got %q", nodeIP)
	}
}

func TestInjectKubeletNodeIP_VLANOnly(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectKubeletNodeIP(input, "10.10.0.5", "")
	if err != nil {
		t.Fatalf("injectKubeletNodeIP failed: %v", err)
	}
	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	machine := config["machine"].(map[string]interface{})
	kubelet := machine["kubelet"].(map[string]interface{})
	extraArgs := kubelet["extraArgs"].(map[string]interface{})
	nodeIP := extraArgs["node-ip"].(string)
	if nodeIP != "10.10.0.5" {
		t.Errorf("expected VLAN-only '10.10.0.5', got %q", nodeIP)
	}
}

func TestInjectKubeletNodeIP_IPv6Only(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectKubeletNodeIP(input, "", "2a01:4f8:271:3b49::/64")
	if err != nil {
		t.Fatalf("injectKubeletNodeIP failed: %v", err)
	}
	var config map[string]interface{}
	if err := yaml.Unmarshal(result, &config); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	machine := config["machine"].(map[string]interface{})
	kubelet := machine["kubelet"].(map[string]interface{})
	extraArgs := kubelet["extraArgs"].(map[string]interface{})
	nodeIP := extraArgs["node-ip"].(string)
	if nodeIP != "2a01:4f8:271:3b49::1" {
		t.Errorf("expected IPv6-only '2a01:4f8:271:3b49::1', got %q", nodeIP)
	}
}

func TestInjectKubeletNodeIP_Neither_NoOp(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	result, err := injectKubeletNodeIP(input, "", "")
	if err != nil {
		t.Fatalf("injectKubeletNodeIP failed: %v", err)
	}
	if string(result) != string(input) {
		t.Error("expected no-op when neither internalIP nor IPv6 is set")
	}
}

// ─── modifyFirstDocument edge cases ────────────────────────────────────────

func TestModifyFirstDocument_FnReturnsError(t *testing.T) {
	input := []byte(`machine:
  type: controlplane
`)
	_, err := modifyFirstDocument(input, func(config map[string]interface{}) error {
		return fmt.Errorf("injected failure")
	})
	if err == nil {
		t.Fatal("expected error from fn to be propagated, got nil")
	}
	if !strings.Contains(err.Error(), "injected failure") {
		t.Errorf("expected error to contain 'injected failure', got %q", err.Error())
	}
}

func TestModifyFirstDocument_InvalidYAML(t *testing.T) {
	input := []byte(`{{{invalid yaml not valid at all`)
	_, err := modifyFirstDocument(input, func(config map[string]interface{}) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}
