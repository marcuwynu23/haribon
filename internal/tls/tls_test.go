package tls_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	htls "github.com/marcuwynu23/haribon/internal/tls"
)

func writeSelfSigned(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "haribon-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	certOut := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = os.WriteFile(certFile, certOut, 0o644)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyOut := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	_ = os.WriteFile(keyFile, keyOut, 0o600)
	return certFile, keyFile
}

func TestLoad_Valid(t *testing.T) {
	cert, key := writeSelfSigned(t)
	cfg, err := htls.Load(cert, key, "1.2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("expected 1 certificate, got %d", len(cfg.Certificates))
	}
	if cfg.MinVersion != 0x0303 { // tls.VersionTLS12
		t.Fatalf("expected TLS1.2 min, got %#x", cfg.MinVersion)
	}
}

func TestLoad_MissingCert(t *testing.T) {
	_, key := writeSelfSigned(t)
	if _, err := htls.Load("no-such.crt", key, "1.2"); err == nil {
		t.Fatal("expected error for missing cert")
	}
}

func TestLoad_BadMinVersion(t *testing.T) {
	cert, key := writeSelfSigned(t)
	if _, err := htls.Load(cert, key, "1.0"); err == nil {
		t.Fatal("expected error for unsupported min_version")
	}
}
