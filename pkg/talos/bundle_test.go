package talos

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

// ─── Test helpers ────────────────────────────────────────────────────────────

// generateTestCA creates a self-signed Ed25519 CA certificate and returns the
// parsed cert, private key, and both as base64(PEM)-encoded strings suitable
// for embedding in a Talos machineconfig.
func generateTestCA(t *testing.T) (*x509.Certificate, ed25519.PrivateKey, string, string) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Ed25519 key: %v", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "test-machine-ca",
			Organization: []string{"talos-test"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	// Encode cert to base64(PEM)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	crtB64 := base64.StdEncoding.EncodeToString(certPEM)

	// Encode key to base64(PEM) using standard PKCS#8 "PRIVATE KEY" header
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal PKCS8 key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	keyB64 := base64.StdEncoding.EncodeToString(keyPEM)

	return cert, priv, crtB64, keyB64
}

// buildMachineconfig constructs a minimal Talos v1alpha1 machineconfig YAML
// with the given base64(PEM) CA cert and key.
func buildMachineconfig(crtB64, keyB64 string) []byte {
	return []byte(fmt.Sprintf(`version: v1alpha1
machine:
  ca:
    crt: %s
    key: %s
cluster:
  secret: dGVzdA==
`, crtB64, keyB64))
}

// ─── MachineCAFromMachineConfig tests ────────────────────────────────────────

func TestMachineCAFromMachineConfig_Success(t *testing.T) {
	origCert, origKey, crtB64, keyB64 := generateTestCA(t)
	mc := buildMachineconfig(crtB64, keyB64)

	cert, key, err := MachineCAFromMachineConfig(mc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the parsed cert matches the original
	if cert.Subject.CommonName != origCert.Subject.CommonName {
		t.Errorf("cert subject CN = %q, want %q", cert.Subject.CommonName, origCert.Subject.CommonName)
	}
	if !cert.IsCA {
		t.Error("parsed cert should be a CA")
	}

	// Verify the private key matches: the public half of the returned key
	// must equal the original public key
	if !origKey.Public().(ed25519.PublicKey).Equal(key.Public()) {
		t.Error("returned private key does not match original")
	}

	// Verify the returned key can sign and the returned cert can verify
	msg := []byte("test-message")
	sig := ed25519.Sign(key, msg)
	if !ed25519.Verify(cert.PublicKey.(ed25519.PublicKey), msg, sig) {
		t.Error("cert public key cannot verify signature from returned private key")
	}
}

func TestMachineCAFromMachineConfig_MultiDocumentYAML(t *testing.T) {
	_, _, crtB64, keyB64 := generateTestCA(t)

	// Multi-document YAML: machineconfig + a second document (hostname override)
	multiDoc := fmt.Sprintf(`version: v1alpha1
machine:
  ca:
    crt: %s
    key: %s
---
version: v1alpha1
machine:
  network:
    hostname: worker-01
`, crtB64, keyB64)

	cert, key, err := MachineCAFromMachineConfig([]byte(multiDoc))
	if err != nil {
		t.Fatalf("unexpected error on multi-document YAML: %v", err)
	}
	if cert == nil {
		t.Fatal("cert is nil")
	}
	if key == nil {
		t.Fatal("key is nil")
	}
	if cert.Subject.CommonName != "test-machine-ca" {
		t.Errorf("cert CN = %q, want %q", cert.Subject.CommonName, "test-machine-ca")
	}
}

func TestMachineCAFromMachineConfig_MissingCACert(t *testing.T) {
	_, _, _, keyB64 := generateTestCA(t)

	// crt is empty string
	mc := buildMachineconfig("", keyB64)
	_, _, err := MachineCAFromMachineConfig(mc)
	if err == nil {
		t.Fatal("expected error for missing CA cert, got nil")
	}
	if !strings.Contains(err.Error(), "missing machine.ca.crt") {
		t.Errorf("error = %q, want it to mention missing machine.ca.crt", err.Error())
	}
}

func TestMachineCAFromMachineConfig_MissingCAKey(t *testing.T) {
	_, _, crtB64, _ := generateTestCA(t)

	// key is empty string
	mc := buildMachineconfig(crtB64, "")
	_, _, err := MachineCAFromMachineConfig(mc)
	if err == nil {
		t.Fatal("expected error for missing CA key, got nil")
	}
	if !strings.Contains(err.Error(), "missing machine.ca") {
		t.Errorf("error = %q, want it to mention missing machine.ca", err.Error())
	}
}

func TestMachineCAFromMachineConfig_InvalidYAML(t *testing.T) {
	_, _, err := MachineCAFromMachineConfig([]byte(`{{{not: valid: yaml: [[[`))
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	if !strings.Contains(err.Error(), "parse machineconfig YAML") {
		t.Errorf("error = %q, want it to mention YAML parse failure", err.Error())
	}
}

func TestMachineCAFromMachineConfig_InvalidBase64(t *testing.T) {
	// Use strings that survive YAML parsing (no special YAML chars like !)
	// but are not valid base64 (contain characters outside the base64 alphabet)
	mc := buildMachineconfig("not_valid_base64_@#$", "not_valid_base64_@#$")
	_, _, err := MachineCAFromMachineConfig(mc)
	if err == nil {
		t.Fatal("expected error for invalid base64, got nil")
	}
	if !strings.Contains(err.Error(), "base64") {
		t.Errorf("error = %q, want it to mention base64 decoding", err.Error())
	}
}

func TestMachineCAFromMachineConfig_NonEd25519Key(t *testing.T) {
	// Generate an Ed25519 CA cert (for the cert part) but pair it with an RSA key
	_, _, crtB64, _ := generateTestCA(t)

	// Generate an RSA key and encode it as base64(PEM(PKCS#8))
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	rsaKeyDER, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatalf("marshal RSA PKCS8: %v", err)
	}
	rsaKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaKeyDER})
	rsaKeyB64 := base64.StdEncoding.EncodeToString(rsaKeyPEM)

	mc := buildMachineconfig(crtB64, rsaKeyB64)
	_, _, err = MachineCAFromMachineConfig(mc)
	if err == nil {
		t.Fatal("expected error for non-Ed25519 key, got nil")
	}
	if !strings.Contains(err.Error(), "not Ed25519") {
		t.Errorf("error = %q, want it to mention 'not Ed25519'", err.Error())
	}
}

// ─── AdminTLSConfig tests ───────────────────────────────────────────────────

func TestAdminTLSConfig_Success(t *testing.T) {
	origCert, _, crtB64, keyB64 := generateTestCA(t)
	mc := buildMachineconfig(crtB64, keyB64)

	tlsCfg, err := AdminTLSConfig(mc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// RootCAs must be set
	if tlsCfg.RootCAs == nil {
		t.Fatal("RootCAs is nil")
	}

	// RootCAs should trust the CA cert: verify by checking that a cert
	// signed by the CA can be verified against the pool
	verifyOpts := x509.VerifyOptions{
		Roots: tlsCfg.RootCAs,
	}
	// The CA cert itself should be in the pool (self-signed CA verifies against itself)
	_, err = origCert.Verify(verifyOpts)
	if err != nil {
		t.Errorf("CA cert not trusted by RootCAs pool: %v", err)
	}

	// Exactly one client certificate
	if len(tlsCfg.Certificates) != 1 {
		t.Fatalf("Certificates length = %d, want 1", len(tlsCfg.Certificates))
	}

	// Parse the client cert and verify properties
	clientCert := tlsCfg.Certificates[0]
	if len(clientCert.Certificate) == 0 {
		t.Fatal("client cert has no certificate data")
	}
	parsedClient, err := x509.ParseCertificate(clientCert.Certificate[0])
	if err != nil {
		t.Fatalf("parse client cert: %v", err)
	}

	// Organization must be "os:admin"
	if len(parsedClient.Subject.Organization) != 1 || parsedClient.Subject.Organization[0] != "os:admin" {
		t.Errorf("client cert org = %v, want [os:admin]", parsedClient.Subject.Organization)
	}

	// Client cert must be signed by the CA: verify against the CA
	clientVerifyOpts := x509.VerifyOptions{
		Roots:     tlsCfg.RootCAs,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if _, err := parsedClient.Verify(clientVerifyOpts); err != nil {
		t.Errorf("client cert not signed by CA: %v", err)
	}

	// Client cert should use Ed25519
	if _, ok := parsedClient.PublicKey.(ed25519.PublicKey); !ok {
		t.Errorf("client cert public key type = %T, want ed25519.PublicKey", parsedClient.PublicKey)
	}

	// MinVersion must be TLS 1.2
	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = 0x%04x, want 0x%04x (TLS 1.2)", tlsCfg.MinVersion, tls.VersionTLS12)
	}

	// Client cert validity window: NotBefore should be ~5min in the past,
	// NotAfter should be ~24h in the future
	now := time.Now()
	if parsedClient.NotBefore.After(now) {
		t.Errorf("client cert NotBefore (%v) is in the future", parsedClient.NotBefore)
	}
	if parsedClient.NotAfter.Before(now) {
		t.Errorf("client cert NotAfter (%v) is in the past", parsedClient.NotAfter)
	}
	if parsedClient.NotAfter.Before(now.Add(23 * time.Hour)) {
		t.Errorf("client cert NotAfter (%v) is less than 23h from now", parsedClient.NotAfter)
	}
}

func TestAdminTLSConfig_ErrorPropagation(t *testing.T) {
	// Invalid machineconfig should propagate the error
	_, err := AdminTLSConfig([]byte(`garbage`))
	if err == nil {
		t.Fatal("expected error for invalid machineconfig, got nil")
	}
	if !strings.Contains(err.Error(), "machine CA") || !strings.Contains(err.Error(), "machineconfig") {
		t.Errorf("error = %q, want it to reference machine CA from machineconfig", err.Error())
	}
}

// ─── decodeCAPKCS8Ed25519 tests ─────────────────────────────────────────────

func TestDecodeCAPKCS8Ed25519_StandardPEMHeader(t *testing.T) {
	// Standard "PRIVATE KEY" header — the default from MarshalPKCS8PrivateKey
	_, _, crtB64, keyB64 := generateTestCA(t)

	cert, key, err := decodeCAPKCS8Ed25519(crtB64, keyB64)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cert == nil {
		t.Fatal("cert is nil")
	}
	if key == nil {
		t.Fatal("key is nil")
	}
	if cert.Subject.CommonName != "test-machine-ca" {
		t.Errorf("cert CN = %q, want %q", cert.Subject.CommonName, "test-machine-ca")
	}
}

func TestDecodeCAPKCS8Ed25519_TalosPEMHeader(t *testing.T) {
	// Talos uses non-standard "ED25519 PRIVATE KEY" PEM header for PKCS#8 DER.
	// The underlying DER bytes are identical — only the PEM header differs.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// Create a self-signed cert
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "talos-header-test"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	// Encode cert normally
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	crtB64 := base64.StdEncoding.EncodeToString(certPEM)

	// Encode key with Talos non-standard "ED25519 PRIVATE KEY" header
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal PKCS8: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "ED25519 PRIVATE KEY", Bytes: keyDER})
	keyB64 := base64.StdEncoding.EncodeToString(keyPEM)

	cert, key, err := decodeCAPKCS8Ed25519(crtB64, keyB64)
	if err != nil {
		t.Fatalf("unexpected error with ED25519 PRIVATE KEY header: %v", err)
	}
	if cert.Subject.CommonName != "talos-header-test" {
		t.Errorf("cert CN = %q, want %q", cert.Subject.CommonName, "talos-header-test")
	}

	// Verify key roundtrip: sign with returned key, verify with cert's public key
	msg := []byte("talos-header-verification")
	sig := ed25519.Sign(key, msg)
	if !ed25519.Verify(cert.PublicKey.(ed25519.PublicKey), msg, sig) {
		t.Error("signature verification failed after roundtrip with Talos PEM header")
	}
}

func TestDecodeCAPKCS8Ed25519_InvalidCertBase64(t *testing.T) {
	_, _, _, keyB64 := generateTestCA(t)
	_, _, err := decodeCAPKCS8Ed25519("not-valid-base64!!", keyB64)
	if err == nil {
		t.Fatal("expected error for invalid cert base64")
	}
	if !strings.Contains(err.Error(), "decode ca cert base64") {
		t.Errorf("error = %q, want it to mention cert base64 decoding", err.Error())
	}
}

func TestDecodeCAPKCS8Ed25519_InvalidKeyBase64(t *testing.T) {
	_, _, crtB64, _ := generateTestCA(t)
	_, _, err := decodeCAPKCS8Ed25519(crtB64, "not-valid-base64!!")
	if err == nil {
		t.Fatal("expected error for invalid key base64")
	}
	if !strings.Contains(err.Error(), "decode ca key base64") {
		t.Errorf("error = %q, want it to mention key base64 decoding", err.Error())
	}
}

func TestDecodeCAPKCS8Ed25519_NoPEMBlockInCert(t *testing.T) {
	_, _, _, keyB64 := generateTestCA(t)
	// Valid base64 but not PEM
	notPEM := base64.StdEncoding.EncodeToString([]byte("this is not PEM data"))
	_, _, err := decodeCAPKCS8Ed25519(notPEM, keyB64)
	if err == nil {
		t.Fatal("expected error for non-PEM cert data")
	}
	if !strings.Contains(err.Error(), "no PEM block") {
		t.Errorf("error = %q, want it to mention no PEM block", err.Error())
	}
}

func TestDecodeCAPKCS8Ed25519_NoPEMBlockInKey(t *testing.T) {
	_, _, crtB64, _ := generateTestCA(t)
	notPEM := base64.StdEncoding.EncodeToString([]byte("this is not PEM data"))
	_, _, err := decodeCAPKCS8Ed25519(crtB64, notPEM)
	if err == nil {
		t.Fatal("expected error for non-PEM key data")
	}
	if !strings.Contains(err.Error(), "no PEM block") {
		t.Errorf("error = %q, want it to mention no PEM block", err.Error())
	}
}
