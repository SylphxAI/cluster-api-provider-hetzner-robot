package controllers

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
)

func modifyFirstDocument(configData []byte, fn func(config map[string]interface{}) error) ([]byte, error) {
	docs := splitYAMLDocuments(configData)
	if len(docs) == 0 {
		return nil, fmt.Errorf("empty machineconfig")
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(docs[0], &config); err != nil {
		return nil, fmt.Errorf("unmarshal first document: %w", err)
	}

	if err := fn(config); err != nil {
		return nil, err
	}

	firstDoc, err := yaml.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("marshal first document: %w", err)
	}

	result := firstDoc
	for _, doc := range docs[1:] {
		result = append(result, []byte("\n---\n")...)
		result = append(result, doc...)
	}

	return result, nil
}

// splitYAMLDocuments splits multi-document YAML by "---" separators,
// returning each document as a trimmed byte slice.
func splitYAMLDocuments(data []byte) [][]byte {
	parts := bytes.Split(data, []byte("\n---\n"))
	var docs [][]byte
	for _, p := range parts {
		p = bytes.TrimSpace(p)
		if len(p) > 0 && !bytes.Equal(p, []byte("---")) {
			p = bytes.TrimPrefix(p, []byte("---\n"))
			docs = append(docs, p)
		}
	}
	return docs
}

// ensureMap returns the child map at key, creating it if absent.
func ensureMap(parent map[string]interface{}, key string) map[string]interface{} {
	if m, ok := parent[key].(map[string]interface{}); ok {
		return m
	}
	m := make(map[string]interface{})
	parent[key] = m
	return m
}

// injectInstallDisk ensures machine.install.disk is set in the Talos machineconfig YAML.
// CAPT generates configs without install disk — CAPHR must inject it from the HRM spec
// before applying, otherwise Talos rejects the config with "install disk or diskSelector should be defined".
func injectInstallDisk(configData []byte, installDisk string) ([]byte, error) {
	if installDisk == "" {
		installDisk = "/dev/nvme0n1"
	}
	return modifyFirstDocument(configData, func(config map[string]interface{}) error {
		machine := ensureMap(config, "machine")
		install := ensureMap(machine, "install")
		if _, exists := install["disk"]; !exists {
			install["disk"] = installDisk
		}
		return nil
	})
}

// injectIPv6Config adds a global IPv6 address and default route to the primary NIC.
// Uses deviceSelector by MAC address — works regardless of OS NIC naming
// (rescue uses eth0, Talos uses enp193s0f0np0, etc.).
// Also sets kubelet dual-stack nodeIP and IPv6 forwarding sysctl.
func injectIPv6Config(configData []byte, ipv6Net string, primaryMAC string, internalIP string) ([]byte, error) {
	if ipv6Net == "" {
		return configData, nil
	}

	return modifyFirstDocument(configData, func(config map[string]interface{}) error {
		machine := ensureMap(config, "machine")
		network := ensureMap(machine, "network")

		ipv6Prefix := strings.Split(ipv6Net, "/")[0]
		ipv6Addr := ipv6Prefix + "1/64"

		// IPv6 goes in a SEPARATE interface entry (deviceSelector: physical only, no MAC).
		// Must NOT merge into the DHCP+VLAN entry — combining dhcp:true with
		// static addresses on the same interface causes Talos networking failures.
		// This matches the proven working config on existing nodes.
		newIface := map[string]interface{}{
			"deviceSelector": map[string]interface{}{
				"physical": true,
			},
			"addresses": []interface{}{ipv6Addr},
			"routes": []interface{}{
				map[string]interface{}{
					"network": "::/0",
					"gateway": "fe80::1",
				},
			},
		}

		interfaces, _ := network["interfaces"].([]interface{})
		interfaces = append(interfaces, newIface)
		network["interfaces"] = interfaces

		// Set IPv6 forwarding sysctl (required for pod routing)
		sysctls := ensureMap(machine, "sysctls")
		sysctls["net.ipv6.conf.all.forwarding"] = "1"

		return nil
	})
}

// injectHostname removed — hostname is managed by CABPT via HostnameConfig
// document (auto: stable). CAPHR injecting machine.network.hostname conflicted
// with HostnameConfig, causing "static hostname is already set" errors.

// injectKubeletNodeIP sets machine.kubelet.extraArgs["node-ip"] so kubelet
// advertises the correct address(es) to the Kubernetes API server.
//
// Without this, kubelet uses the default route's IP — which on Hetzner is the
// public IPv4. Internal VLAN traffic would route over the public internet instead
// of the private vSwitch network. Same problem as providerID: bare metal has no
// CCM to auto-detect the right IPs; CABPT templates don't know per-server IPs.
//
// Cases:
//   - VLAN + IPv6: node-ip = internalIP,ipv6 (dual-stack)
//   - VLAN only:   node-ip = internalIP (single-stack, private network)
//   - IPv6 only:   node-ip = ipv6 (single-stack IPv6)
//   - Neither:     no injection (kubelet auto-detects)
func injectKubeletNodeIP(configData []byte, internalIP string, ipv6Net string) ([]byte, error) {
	// Derive IPv6 node address from the /64 subnet.
	var nodeIPv6 string
	if ipv6Net != "" {
		ipv6Prefix := strings.Split(ipv6Net, "/")[0]
		nodeIPv6 = strings.TrimSuffix(ipv6Prefix, "::") + "::1"
	}

	// Build the node-ip value.
	var nodeIP string
	switch {
	case internalIP != "" && nodeIPv6 != "":
		nodeIP = internalIP + "," + nodeIPv6 // dual-stack
	case internalIP != "":
		nodeIP = internalIP // VLAN only
	case nodeIPv6 != "":
		nodeIP = nodeIPv6 // IPv6 only
	default:
		return configData, nil // no injection needed
	}

	return modifyFirstDocument(configData, func(config map[string]interface{}) error {
		machine := ensureMap(config, "machine")
		kubelet := ensureMap(machine, "kubelet")
		extraArgs := ensureMap(kubelet, "extraArgs")
		extraArgs["node-ip"] = nodeIP
		return nil
	})
}

// injectVLANConfig adds a VLAN interface to the Talos machineconfig.
//
// Uses deviceSelector by MAC address for parent NIC identification.
// Parent NIC uses DHCP for public IP (matches proven working config on existing nodes).
// VLAN gets a static address from HetznerRobotHost.Spec.InternalIP.
func injectVLANConfig(configData []byte, vlanCfg *infrav1.VLANConfig, internalIP string, primaryMAC string, serverIP string, gatewayIP string) ([]byte, error) {
	if vlanCfg == nil || internalIP == "" {
		return configData, nil
	}

	prefixLen := vlanCfg.PrefixLength
	if prefixLen == 0 {
		prefixLen = 24
	}

	return modifyFirstDocument(configData, func(config map[string]interface{}) error {
		machine := ensureMap(config, "machine")
		network := ensureMap(machine, "network")

		// VLAN entry with static internal IP.
		// No MTU override — Hetzner vSwitch handles MTU negotiation.
		vlanEntry := map[string]interface{}{
			"vlanId": vlanCfg.ID,
			"addresses": []interface{}{
				fmt.Sprintf("%s/%d", internalIP, prefixLen),
			},
		}

		// Parent NIC: DHCP for public IP + VLAN overlay.
		// DHCP is proven working on all existing nodes. Static /32 caused
		// networking failures on some NIC types (Broadcom vs others).
		ifaceEntry := map[string]interface{}{
			"deviceSelector": map[string]interface{}{
				"hardwareAddr": primaryMAC, "physical": true,
			},
			"dhcp":  true,
			"vlans": []interface{}{vlanEntry},
		}

		// Find or create the interfaces list
		interfaces, ok := network["interfaces"].([]interface{})
		if !ok {
			interfaces = []interface{}{}
		}

		// Check if an entry for this NIC already exists (by deviceSelector MAC) — merge VLAN into it
		found := false
		for i, iface := range interfaces {
			ifMap, ok := iface.(map[string]interface{})
			if !ok {
				continue
			}
			selector, _ := ifMap["deviceSelector"].(map[string]interface{})
			if (selector != nil && selector["hardwareAddr"] == primaryMAC) || ifMap["interface"] == primaryMAC {
				// Enable DHCP (remove any static config)
				ifMap["dhcp"] = true
				delete(ifMap, "addresses")
				delete(ifMap, "routes")
				// Add VLAN to existing vlans list (or create new)
				existingVlans, _ := ifMap["vlans"].([]interface{})
				ifMap["vlans"] = append(existingVlans, vlanEntry)
				interfaces[i] = ifMap
				found = true
				break
			}
		}

		if !found {
			interfaces = append(interfaces, ifaceEntry)
		}

		network["interfaces"] = interfaces
		return nil
	})
}

// injectSecretboxEncryptionSecret + injectServiceAccountKey removed.
// Verified: CABPT/CACPPT already shares the same keys across all CP nodes.
// CAPHR's previous injection was redundant (overwriting identical values).

// injectProviderID sets machine.kubelet.extraArgs["provider-id"] in the Talos
// machineconfig. This causes kubelet to register the Node with the correct providerID,
// allowing CAPI to match Machine → Node. Without this, CAPI can't find the Node
// and the Machine stays in Failed phase.
func injectProviderID(configData []byte, providerID string) ([]byte, error) {
	if providerID == "" {
		return configData, nil
	}

	return modifyFirstDocument(configData, func(config map[string]interface{}) error {
		machine := ensureMap(config, "machine")
		kubelet := ensureMap(machine, "kubelet")
		extraArgs := ensureMap(kubelet, "extraArgs")
		extraArgs["provider-id"] = providerID
		return nil
	})
}
