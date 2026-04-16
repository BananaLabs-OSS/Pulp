package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnableTLS_Succeeds(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t)
	s := NewHTTPServer(":0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.EnableTLS(certPath, keyPath); err != nil {
		t.Fatalf("EnableTLS: %v", err)
	}
	if s.certPath != certPath || s.keyPath != keyPath {
		t.Error("cert/key paths not stored")
	}
}

func TestEnableTLS_RejectsMissingPath(t *testing.T) {
	s := NewHTTPServer(":0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.EnableTLS("", ""); err == nil {
		t.Error("expected error for empty paths")
	}
	if err := s.EnableTLS("/does/not/exist.pem", "/also/missing.key"); err == nil {
		t.Error("expected error for missing files")
	}
}

func TestEnableTLS_RejectsMismatchedPair(t *testing.T) {
	certA, _ := writeSelfSignedCert(t)
	_, keyB := writeSelfSignedCert(t)
	s := NewHTTPServer(":0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.EnableTLS(certA, keyB); err == nil {
		t.Error("expected error for mismatched cert/key pair")
	}
}

func writeSelfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "pulp-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certFile, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}
	certFile.Close()

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyFile, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	keyFile.Close()

	return certPath, keyPath
}
