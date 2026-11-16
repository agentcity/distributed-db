// Package testutil generates throwaway X.509 certificate material for
// TLS/mTLS tests in the KV, Raft, meta and client packages.
//
// It is test support code: nothing in the production data path imports it. A
// generated set contains one CA, a server certificate valid for localhost and
// the loopback addresses, and a client certificate signed by the same CA.
package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Certs holds absolute paths to a generated CA, server and client key pair.
// Every file is PEM-encoded and written with mode 0o600.
//
// The server certificate carries both ServerAuth and ClientAuth extended key
// usages: a homogeneous cluster presents the node certificate when it dials its
// peers, so the same leaf must be acceptable in both directions. The client
// certificate is ClientAuth-only.
type Certs struct {
	// Dir is the directory holding every generated file.
	Dir string

	// CAFile is the PEM bundle that signed both leaf certificates.
	CAFile string
	// ServerCert and ServerKey are the server certificate and its key. The
	// certificate is valid for localhost, kv.test and the loopback addresses.
	ServerCert string
	ServerKey  string
	// ClientCert and ClientKey are the client certificate and its key.
	ClientCert string
	ClientKey  string
}

// Generate creates a CA, a server certificate and a client certificate in dir,
// which must already exist (typically a test temporary directory).
func Generate(dir string) (*Certs, error) {
	ca, err := newCA("testutil CA")
	if err != nil {
		return nil, err
	}
	server, err := ca.issue("kv.test",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		[]string{"localhost", "kv.test", "meta.test", "raft.test"},
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")})
	if err != nil {
		return nil, err
	}
	client, err := ca.issue("client.test",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil, nil)
	if err != nil {
		return nil, err
	}

	c := &Certs{Dir: dir}
	files := []struct {
		name string
		data []byte
		dst  *string
	}{
		{"ca.pem", ca.certPEM, &c.CAFile},
		{"server.crt", server.certPEM, &c.ServerCert},
		{"server.key", server.keyPEM, &c.ServerKey},
		{"client.crt", client.certPEM, &c.ClientCert},
		{"client.key", client.keyPEM, &c.ClientKey},
	}
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, f.data, 0o600); err != nil {
			return nil, fmt.Errorf("testutil: write %s: %w", path, err)
		}
		*f.dst = path
	}
	return c, nil
}

type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

type leafCert struct {
	certPEM []byte
	keyPEM  []byte
}

func newCA(cn string) (*testCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &testCA{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

func (ca *testCA) issue(cn string, usages []x509.ExtKeyUsage, dnsNames []string, ips []net.IP) (*leafCert, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           usages,
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &leafCert{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}
