package tls_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	htls "github.com/marcuwynu23/haribon/internal/tls"
)

func writeSelfSigned(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	return writeSelfSignedAt(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
}

func writeSelfSignedAt(t *testing.T, notBefore, notAfter time.Time) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "haribon-test"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
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

func TestLoad_ExpiredCert(t *testing.T) {
	cert, key := writeSelfSignedAt(t, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	_, err := htls.Load(cert, key, "1.2")
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired error, got %v", err)
	}
}

func TestLoad_NotYetValidCert(t *testing.T) {
	cert, key := writeSelfSignedAt(t, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))
	_, err := htls.Load(cert, key, "1.2")
	if err == nil || !strings.Contains(err.Error(), "not valid before") {
		t.Fatalf("expected not-valid-yet error, got %v", err)
	}
}

// TestServeTLS_E2E spins a real TLS listener built from Load and fetches a
// page over https, proving the config actually terminates TLS.
func TestServeTLS_E2E(t *testing.T) {
	cert, key := writeSelfSigned(t)
	tlsCfg, err := htls.Load(cert, key, "1.2")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("secure-ok"))
	}))
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pemBytes, err := os.ReadFile(cert)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	pool.AppendCertsFromPEM(pemBytes)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("https get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "secure-ok" {
		t.Fatalf("got status %d body %q", resp.StatusCode, body)
	}
	if resp.TLS == nil {
		t.Fatal("expected TLS connection")
	}
}
