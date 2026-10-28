package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func writeKeypair(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestStartWebhookServer(t *testing.T) {
	oldCert, oldKey, oldPort := tlsCertFile, tlsKeyFile, webhookPort
	t.Cleanup(func() { tlsCertFile, tlsKeyFile, webhookPort = oldCert, oldKey, oldPort })
	webhookPort = freePort(t)

	tlsCertFile, tlsKeyFile = "/nonexistent/tls.crt", "/nonexistent/tls.key"
	if _, err := startWebhookServer(context.Background(), fake.NewSimpleClientset(), "driver", nil); err == nil {
		t.Fatal("expected error for missing keypair")
	}

	tlsCertFile, tlsKeyFile = writeKeypair(t, t.TempDir())
	srv, err := startWebhookServer(context.Background(), fake.NewSimpleClientset(), "driver", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = srv.Shutdown(context.Background())
}
