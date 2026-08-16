package netx

// Built-in TLS, for direct connections over the public internet. `-tls auto` auto-generates and reuses a self-signed certificate;
// the client side verifies it via certificate fingerprint pinning (TOFU) — no CA or domain name required.

import (
	"botbureau/backend/internal/i18n"

	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// EnsureSelfSignedCert generates (or reuses) a self-signed certificate, returning the cert/key paths and the SHA-256 fingerprint (hex).
func EnsureSelfSignedCert(dataDir string) (certPath, keyPath, fingerprint string, err error) {
	dir := filepath.Join(dataDir, "tls")
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if fp, ferr := certFingerprint(certPath); ferr == nil {

		// Certificate already exists — reuse it (pinning is only meaningful if the fingerprint stays stable)
		return certPath, keyPath, fp, nil
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return
	}
	host, _ := os.Hostname()
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "botbureau@" + host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host, "localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return
	}
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return
	}
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return
	}
	sum := sha256.Sum256(der)
	return certPath, keyPath, hex.EncodeToString(sum[:]), nil
}

// certFingerprint reads a PEM certificate and returns the SHA-256 fingerprint (hex) of its DER bytes.
func certFingerprint(certPath string) (string, error) {
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf(i18n.T("%s is not a valid PEM certificate"), certPath)
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// ModernTLSConfig tightens the config to TLS 1.2+.
func ModernTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12}
}
