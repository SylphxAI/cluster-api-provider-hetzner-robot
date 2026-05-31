package v1alpha1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestHetznerRobotHostCRDHasPhysicalIdentityImmutability(t *testing.T) {
	validations := crdSpecValidations(t, "infrastructure.cluster.x-k8s.io_hetznerrobothosts.yaml")

	requireValidationRule(t, validations, "serverID is immutable", "self.serverID == oldSelf.serverID")
	requireValidationRule(t, validations, "serverIP is immutable", "oldSelf.serverIP")
	requireValidationRule(t, validations, "serverIPv6Net is immutable", "oldSelf.serverIPv6Net")
	requireValidationRule(t, validations, "internalIP is immutable", "oldSelf.internalIP")
	requireValidationRule(t, validations, "installDisk is immutable", "oldSelf.installDisk")
}

func TestHetznerRobotMachineCRDHasBindingImmutability(t *testing.T) {
	validations := crdSpecValidations(t, "infrastructure.cluster.x-k8s.io_hetznerrobotmachines.yaml")

	requireValidationRule(t, validations, "hostRef and hostSelector are mutually exclusive", "has(self.hostRef)")
	requireValidationRule(t, validations, "providerID is immutable", "oldSelf.providerID")
	requireValidationRule(t, validations, "hostRef is immutable", "oldSelf.hostRef")
	requireValidationRule(t, validations, "hostSelector is immutable", "oldSelf.hostSelector")
}

func crdSpecValidations(t *testing.T, fileName string) []map[string]any {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", fileName))
	if err != nil {
		t.Fatalf("read CRD %s: %v", fileName, err)
	}

	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse CRD %s: %v", fileName, err)
	}

	specSchema := nestedMap(
		t,
		document,
		"spec",
		"versions",
		"0",
		"schema",
		"openAPIV3Schema",
		"properties",
		"spec",
	)
	rawValidations, ok := specSchema["x-kubernetes-validations"].([]any)
	if !ok || len(rawValidations) == 0 {
		t.Fatalf("%s spec schema has no x-kubernetes-validations", fileName)
	}

	validations := make([]map[string]any, 0, len(rawValidations))
	for _, raw := range rawValidations {
		validation, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("%s validation entry has unexpected type %T", fileName, raw)
		}
		validations = append(validations, validation)
	}
	return validations
}

func requireValidationRule(t *testing.T, validations []map[string]any, messagePart, rulePart string) {
	t.Helper()
	for _, validation := range validations {
		message, _ := validation["message"].(string)
		rule, _ := validation["rule"].(string)
		if strings.Contains(message, messagePart) && strings.Contains(rule, rulePart) {
			return
		}
	}
	t.Fatalf("missing validation rule with message containing %q and rule containing %q", messagePart, rulePart)
}

func nestedMap(t *testing.T, value any, path ...string) map[string]any {
	t.Helper()

	current := value
	for _, key := range path {
		switch typed := current.(type) {
		case map[string]any:
			current = typed[key]
		case []any:
			index := 0
			if key != "0" {
				t.Fatalf("unsupported non-zero list index %q in path %v", key, path)
			}
			if len(typed) == 0 {
				t.Fatalf("empty list at %q in path %v", key, path)
			}
			current = typed[index]
		default:
			t.Fatalf("unexpected %T at %q in path %v", current, key, path)
		}
	}

	result, ok := current.(map[string]any)
	if !ok {
		t.Fatalf("path %v resolved to %T, want map", path, current)
	}
	return result
}
