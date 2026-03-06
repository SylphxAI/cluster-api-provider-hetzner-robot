/*
Package talos provides utilities for interacting with the Talos Linux API.
Uses native Go gRPC — no subprocess, no talosctl binary dependency.
*/
package talos

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"gopkg.in/yaml.v3"
)

// ─── Machineconfig CA extraction ─────────────────────────────────────────────

// talosV1Alpha1Config is a minimal subset of the Talos v1alpha1 machineconfig,
// enough to extract machine.ca.crt and machine.ca.key.
type talosV1Alpha1Config struct {
	Machine struct {
		CA struct {
			Crt string `yaml:"crt"` // base64(PEM) of the machine CA cert
			Key string `yaml:"key"` // base64(PEM) of the machine CA key
		} `yaml:"ca"`
	} `yaml:"machine"`
}

// MachineCAFromMachineConfig parses the Talos machineconfig YAML (from the CABT
// bootstrap-data secret) and returns the machine.ca certificate and private key.
//
// This is the CORRECT source for the machine CA — not the CABT bundle's certs.os,
// which is a separate key pair that CABT generates independently.
// The Talos API server uses machine.ca to sign its TLS server certificate.
func MachineCAFromMachineConfig(machineConfigYAML []byte) (*x509.Certificate, ed25519.PrivateKey, error) {
	// Machineconfig can be multi-document YAML (machineconfig + hostname config).
	// Use only the first document.
	dec := yaml.NewDecoder(bytes.NewReader(machineConfigYAML))
	var mc talosV1Alpha1Config
	if err := dec.Decode(&mc); err != nil {
		return nil, nil, fmt.Errorf("parse machineconfig YAML: %w", err)
	}

	if mc.Machine.CA.Crt == "" || mc.Machine.CA.Key == "" {
		return nil, nil, fmt.Errorf("machineconfig missing machine.ca.crt or machine.ca.key")
	}

	return decodeCAPKCS8Ed25519(mc.Machine.CA.Crt, mc.Machine.CA.Key)
}

// ─── Common cert parsing ──────────────────────────────────────────────────────

// decodeCAPKCS8Ed25519 decodes a base64(PEM) cert and a base64(PEM) PKCS#8 Ed25519 key.
// Handles both standard "PRIVATE KEY" and Talos non-standard "ED25519 PRIVATE KEY" PEM headers.
func decodeCAPKCS8Ed25519(crtB64, keyB64 string) (*x509.Certificate, ed25519.PrivateKey, error) {
	// Cert: base64(PEM) → PEM → DER → *x509.Certificate
	caCertPEM, err := base64.StdEncoding.DecodeString(crtB64)
	if err != nil {
		return nil, nil, fmt.Errorf("decode ca cert base64: %w", err)
	}
	caCertBlock, _ := pem.Decode(caCertPEM)
	if caCertBlock == nil {
		return nil, nil, fmt.Errorf("ca cert: no PEM block found")
	}
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	// Key: base64(PEM) → PEM → DER → ed25519.PrivateKey
	// Talos uses non-standard PEM header "ED25519 PRIVATE KEY" for PKCS#8.
	// x509.ParsePKCS8PrivateKey operates on DER bytes (ignores PEM header).
	caKeyPEM, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, nil, fmt.Errorf("decode ca key base64: %w", err)
	}
	caKeyBlock, _ := pem.Decode(caKeyPEM)
	if caKeyBlock == nil {
		return nil, nil, fmt.Errorf("ca key: no PEM block found")
	}
	caKeyRaw, err := x509.ParsePKCS8PrivateKey(caKeyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA private key (PKCS#8): %w", err)
	}
	caKey, ok := caKeyRaw.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("CA key is not Ed25519 (got %T)", caKeyRaw)
	}

	return caCert, caKey, nil
}

// ─── TLS config generation ────────────────────────────────────────────────────

// AdminTLSConfig generates an authenticated *tls.Config for the Talos machine API.
// It creates a short-lived Ed25519 admin client certificate signed by the machine CA
// from the Talos machineconfig (bootstrap-data secret), enabling mTLS on port 50000.
//
// machineConfigYAML must be the raw machineconfig YAML from the CABT bootstrap-data secret.
// This is the same config that was applied to the node via ApplyConfig.
func AdminTLSConfig(machineConfigYAML []byte) (*tls.Config, error) {
	caCert, caKey, err := MachineCAFromMachineConfig(machineConfigYAML)
	if err != nil {
		return nil, fmt.Errorf("read machine CA from machineconfig: %w", err)
	}
	return adminTLSConfigFromCA(caCert, caKey)
}

// adminTLSConfigFromCA builds a *tls.Config with a generated admin client cert.
func adminTLSConfigFromCA(caCert *x509.Certificate, caKey ed25519.PrivateKey) (*tls.Config, error) {
	// Generate ephemeral Ed25519 client key pair
	clientPub, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate client Ed25519 key: %w", err)
	}

	// Create admin client cert signed by the machine CA.
	// Talos v1.4+ uses "os:admin" role (renamed from "talos:admin").
	// See: github.com/siderolabs/talos/pkg/machinery/role/role.go
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"os:admin"},
		},
		NotBefore:   time.Now().Add(-5 * time.Minute),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientCertDER, err := x509.CreateCertificate(rand.Reader, template, caCert, clientPub, caKey)
	if err != nil {
		return nil, fmt.Errorf("sign client certificate: %w", err)
	}

	clientCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientCertDER})
	clientKeyDER, err := x509.MarshalPKCS8PrivateKey(clientPriv)
	if err != nil {
		return nil, fmt.Errorf("marshal client key: %w", err)
	}
	clientKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: clientKeyDER})

	tlsCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("build TLS key pair: %w", err)
	}

	// Build RootCAs pool from the actual machine CA.
	// This is the CA the Talos API server uses to sign its own TLS server cert.
	caPool := x509.NewCertPool()
	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("failed to add machine CA to cert pool")
	}

	return &tls.Config{
		RootCAs:      caPool,
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}
