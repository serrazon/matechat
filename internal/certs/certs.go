// Package certs handles family CA management and TLS configuration.
//
// All certificates use ECDSA P-256. The CA is valid for 20 years,
// device/server certificates for 10 years. Revocation is a simple
// text file (revoked.txt) checked post-handshake.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// InitCA generates a new self-signed CA key pair in dir.
// Creates family-ca.crt and family-ca.key. Fails if they already exist.
func InitCA(dir string) error {
	certPath := filepath.Join(dir, "family-ca.crt")
	keyPath := filepath.Join(dir, "family-ca.key")

	if fileExists(certPath) || fileExists(keyPath) {
		return fmt.Errorf("CA files already exist in %s", dir)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "matechat Family CA",
			Organization: []string{"matechat"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(20 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	if err := writePEM(certPath, "CERTIFICATE", certDER); err != nil {
		return err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER); err != nil {
		return err
	}

	return nil
}

// IssueCert signs a new certificate using the CA in caDir.
// Writes <name>.crt and <name>.key to outDir.
func IssueCert(caDir, outDir, name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}

	caCert, caKey, err := loadCA(caDir)
	if err != nil {
		return fmt.Errorf("load CA: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   name,
			Organization: []string{"matechat"},
		},
		NotBefore: time.Now().Add(-1 * time.Hour),
		NotAfter:  time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
			x509.ExtKeyUsageServerAuth,
		},
	}

	// Always add name as a SAN (Go 1.15+ rejects certs with CN only).
	// Add localhost SANs for local development convenience.
	if ip := net.ParseIP(name); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{name, "localhost"}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	if err := os.MkdirAll(outDir, 0700); err != nil {
		return err
	}

	certPath := filepath.Join(outDir, name+".crt")
	keyPath := filepath.Join(outDir, name+".key")

	if err := writePEM(certPath, "CERTIFICATE", certDER); err != nil {
		return err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER); err != nil {
		return err
	}

	return nil
}

// RevokeCert appends name to the revoked.txt file in caDir.
func RevokeCert(caDir, name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	revokedPath := filepath.Join(caDir, "revoked.txt")

	// Check if already revoked
	existing, _ := os.ReadFile(revokedPath)
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == name {
			return fmt.Errorf("%q is already revoked", name)
		}
	}

	f, err := os.OpenFile(revokedPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, name)
	return err
}

// LoadRevokedNames reads the revoked.txt file and returns a set of revoked names.
func LoadRevokedNames(caDir string) (map[string]bool, error) {
	revokedPath := filepath.Join(caDir, "revoked.txt")
	data, err := os.ReadFile(revokedPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	names := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names[line] = true
		}
	}
	return names, nil
}

// LoadServerTLS builds a tls.Config for the broker.
// Requires client certificates signed by the family CA.
func LoadServerTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load server cert: %w", err)
	}

	caPool, err := loadCAPool(caFile)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// LoadClientTLS builds a tls.Config for a client dialing the broker or peers.
func LoadClientTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}

	caPool, err := loadCAPool(caFile)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// PeerName returns the CN from the first peer certificate of an established TLS connection.
func PeerName(conn *tls.Conn) (string, error) {
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return "", fmt.Errorf("no peer certificates")
	}
	cn := state.PeerCertificates[0].Subject.CommonName
	if cn == "" {
		return "", fmt.Errorf("peer certificate has no CN")
	}
	return cn, nil
}

// --- helpers ---

func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "family-ca.crt"))
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("failed to decode CA cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}

	keyPEM, err := os.ReadFile(filepath.Join(dir, "family-ca.key"))
	if err != nil {
		return nil, nil, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("failed to decode CA key PEM")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}

	return cert, key, nil
}

func loadCAPool(caFile string) (*x509.CertPool, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}
	return pool, nil
}

func writePEM(path, pemType string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: pemType, Bytes: data})
}

func randomSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	return serial, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
