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
	if ipv6Net == "" || primaryMAC == "" {
		return configData, nil
	}

	return modifyFirstDocument(configData, func(config map[string]interface{}) error {
		machine := ensureMap(config, "machine")
		network := ensureMap(machine, "network")

		// ipv6Net from Hetzner API may include prefix length (e.g. "2a01:4f8:271:3b49::/64").
		// Strip it before constructing the host address.
		ipv6Prefix := strings.Split(ipv6Net, "/")[0] // "2a01:4f8:271:3b49::"
		ipv6Addr := ipv6Prefix + "1/64"              // "2a01:4f8:271:3b49::1/64"

		// Find existing interface by deviceSelector MAC or create new
		interfaces, _ := network["interfaces"].([]interface{})
		found := false
		for _, iface := range interfaces {
			ifMap, ok := iface.(map[string]interface{})
			if !ok {
				continue
			}
			// Match by deviceSelector.hardwareAddr or by legacy interface name
			selector, _ := ifMap["deviceSelector"].(map[string]interface{})
			if (selector != nil && selector["hardwareAddr"] == primaryMAC) || ifMap["interface"] == primaryMAC {
				addrs, _ := ifMap["addresses"].([]interface{})
				addrs = append(addrs, ipv6Addr)
				ifMap["addresses"] = addrs
				routes, _ := ifMap["routes"].([]interface{})
				routes = append(routes, map[string]interface{}{
					"network": "::/0",
					"gateway": "fe80::1",
				})
				ifMap["routes"] = routes
				found = true
				break
			}
		}

		if !found {
			newIface := map[string]interface{}{
				"deviceSelector": map[string]interface{}{
					"hardwareAddr": primaryMAC, "physical": true,
				},
				"addresses": []interface{}{ipv6Addr},
				"routes": []interface{}{
					map[string]interface{}{
						"network": "::/0",
						"gateway": "fe80::1",
					},
				},
			}
			interfaces = append(interfaces, newIface)
			network["interfaces"] = interfaces
		}

		// Set IPv6 forwarding sysctl (required for pod routing)
		sysctls := ensureMap(machine, "sysctls")
		sysctls["net.ipv6.conf.all.forwarding"] = "1"

		// Set kubelet nodeIP for dual-stack: IPv4 (VLAN) + IPv6.
		// Without this, kubelet only advertises the IPv4 address and K8s
		// doesn't know the node has IPv6 connectivity.
		kubelet := ensureMap(machine, "kubelet")
		extraArgs := ensureMap(kubelet, "extraArgs")
		// Kubelet dual-stack nodeIP: VLAN IPv4 + public IPv6.
		// Both are needed for K8s to advertise the node as dual-stack.
		nodeIPv6 := strings.TrimSuffix(ipv6Prefix, "::") + "::1" // e.g. 2a01:4f8:2210:1a2e::1
		if internalIP != "" {
			extraArgs["node-ip"] = internalIP + "," + nodeIPv6
		} else {
			extraArgs["node-ip"] = nodeIPv6
		}

		return nil
	})
}

// injectHostname sets machine.network.hostname in the Talos machineconfig.
//
// Format: <role>-<dc>-<serverID>
//   - role: from HetznerRobotHost label "role" (e.g. "compute", "storage")
//     Defaults to "compute" for control-plane and worker roles.
//   - dc: Hetzner datacenter (e.g. "fsn1") — from HetznerRobotCluster.Spec.DC
//   - serverID: Hetzner Robot server ID (immutable hardware identifier)
//
// Examples: "compute-fsn1-2938104", "storage-fsn1-2965124"
func injectHostname(configData []byte, dc string, serverID int, hostRole string) ([]byte, error) {
	if serverID == 0 {
		return configData, nil
	}

	if dc == "" {
		dc = "fsn1"
	}
	// Map host role label to hostname prefix
	prefix := "compute"
	if hostRole == "storage" {
		prefix = "storage"
	}
	hostname := fmt.Sprintf("%s-%s-%d", prefix, dc, serverID)

	return modifyFirstDocument(configData, func(config map[string]interface{}) error {
		machine := ensureMap(config, "machine")
		network := ensureMap(machine, "network")
		network["hostname"] = hostname
		return nil
	})
}

// injectVLANConfig adds a VLAN interface to the Talos machineconfig and configures
// static IPv4 routing on the parent NIC.
//
// Uses deviceSelector by MAC address for parent NIC identification.
// Configures static /32 address + explicit default route on the parent NIC instead
// of DHCP. Hetzner DHCP assigns /25 or /26 prefixes which create on-link routes for
// the entire subnet. When two servers share the same /25 (e.g., 138.199.242.217 and
// 138.199.242.218), the kernel tries direct ARP instead of routing through the gateway.
// Hetzner blocks direct L2 between servers, so SSH and all inter-node traffic fails.
// Static /32 + explicit gateway forces all traffic through the router.
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

		// Build the VLAN entry.
		// MTU 1400 is required by Hetzner vSwitch — default 1500 + 4-byte VLAN tag
		// header = 1504, exceeding the vSwitch underlying MTU. Large packets would
		// be silently dropped (TCP small packets work, large transfers break).
		vlanEntry := map[string]interface{}{
			"vlanId": vlanCfg.ID,
			"mtu":    1400,
			"addresses": []interface{}{
				fmt.Sprintf("%s/%d", internalIP, prefixLen),
			},
		}

		// Build static routes for the parent NIC.
		// With a /32 address, the kernel has no on-link route for the gateway itself.
		// The on-link route (gatewayIP/32 with no gateway field) tells the kernel the
		// gateway is directly reachable on this interface. Without it, the default route
		// fails with "network unreachable" because there's no path to the next hop.
		parentRoutes := []interface{}{
			map[string]interface{}{
				"network": "0.0.0.0/0",
				"gateway": gatewayIP,
			},
			map[string]interface{}{
				"network": gatewayIP + "/32",
			},
		}

		// Build the interface entry with deviceSelector + static IP + routes + VLAN.
		// No DHCP — static /32 avoids Hetzner's L2 isolation issue with /25 on-link routes.
		ifaceEntry := map[string]interface{}{
			"deviceSelector": map[string]interface{}{
				"hardwareAddr": primaryMAC, "physical": true,
			},
			"addresses": []interface{}{serverIP + "/32"},
			"routes":    parentRoutes,
			"vlans":     []interface{}{vlanEntry},
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
				// Set static IP config (replace any existing dhcp/addresses)
				delete(ifMap, "dhcp")
				ifMap["addresses"] = []interface{}{serverIP + "/32"}
				ifMap["routes"] = parentRoutes
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

// injectSecretboxEncryptionSecret replaces cluster.secretboxEncryptionSecret in the
// machineconfig YAML. CAPT may generate a different encryption key per Machine, but all
// CP nodes must use the same key to decrypt secrets in shared etcd. This function ensures
// the correct cluster-wide key is used.
func injectSecretboxEncryptionSecret(configData []byte, secret string) ([]byte, error) {
	if secret == "" {
		return configData, nil
	}

	return modifyFirstDocument(configData, func(config map[string]interface{}) error {
		cluster := ensureMap(config, "cluster")
		cluster["secretboxEncryptionSecret"] = secret
		return nil
	})
}

// injectServiceAccountKey overrides cluster.serviceAccount.key in the Talos machineconfig.
// CABPT generates a unique SA key per Machine, but all CP nodes sharing etcd must use the
// same key — otherwise API servers can't validate tokens signed by other CP nodes.
// Workers are unaffected (they don't run kube-apiserver), but injecting consistently
// ensures correctness if a worker is later promoted.
func injectServiceAccountKey(configData []byte, saKey string) ([]byte, error) {
	if saKey == "" {
		return configData, nil
	}

	return modifyFirstDocument(configData, func(config map[string]interface{}) error {
		cluster := ensureMap(config, "cluster")
		sa := ensureMap(cluster, "serviceAccount")
		sa["key"] = saKey
		return nil
	})
}

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
