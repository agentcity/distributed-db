package grpctls_test

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/grpctls"
)

func TestHandshakeTLS13(t *testing.T) {
	f := newTestFiles(t)
	serverCfg := mustServerConfig(t, grpctls.ServerOptions{
		CertFile: f.serverCert,
		KeyFile:  f.serverKey,
	})
	clientCfg := mustClientConfig(t, grpctls.ClientOptions{
		CAFile:     f.caFile,
		ServerName: "localhost",
	})

	clientErr, serverErr := runHandshake(t, serverCfg, clientCfg)
	if clientErr != nil || serverErr != nil {
		t.Fatalf("TLS 1.3 handshake failed: client=%v server=%v", clientErr, serverErr)
	}
}

func TestHandshakeMutualTLS(t *testing.T) {
	f := newTestFiles(t)
	serverCfg := mustServerConfig(t, grpctls.ServerOptions{
		CertFile:     f.serverCert,
		KeyFile:      f.serverKey,
		ClientCAFile: f.caFile,
	})
	clientCfg := mustClientConfig(t, grpctls.ClientOptions{
		CAFile:     f.caFile,
		CertFile:   f.clientCert,
		KeyFile:    f.clientKey,
		ServerName: "localhost",
	})

	clientErr, serverErr := runHandshake(t, serverCfg, clientCfg)
	if clientErr != nil || serverErr != nil {
		t.Fatalf("mTLS handshake failed: client=%v server=%v", clientErr, serverErr)
	}
}

func TestHandshakeMTLSWithoutClientCert(t *testing.T) {
	f := newTestFiles(t)
	serverCfg := mustServerConfig(t, grpctls.ServerOptions{
		CertFile:     f.serverCert,
		KeyFile:      f.serverKey,
		ClientCAFile: f.caFile,
	})
	clientCfg := mustClientConfig(t, grpctls.ClientOptions{
		CAFile:     f.caFile,
		ServerName: "localhost",
	})

	clientErr, serverErr := runHandshake(t, serverCfg, clientCfg)
	if serverErr == nil {
		t.Fatalf("server accepted a client without a certificate (client error: %v)", clientErr)
	}
	if !strings.Contains(strings.ToLower(serverErr.Error()), "certificate") {
		t.Fatalf("server error = %v, want a certificate rejection", serverErr)
	}
}

func TestHandshakeWrongCA(t *testing.T) {
	f := newTestFiles(t)
	otherCA := newTestCA(t)
	otherCAFile := writePEM(t, t.TempDir(), "other-ca.pem", otherCA.certPEM)

	serverCfg := mustServerConfig(t, grpctls.ServerOptions{
		CertFile: f.serverCert,
		KeyFile:  f.serverKey,
	})
	clientCfg := mustClientConfig(t, grpctls.ClientOptions{
		CAFile:     otherCAFile,
		ServerName: "localhost",
	})

	clientErr, serverErr := runHandshake(t, serverCfg, clientCfg)
	if clientErr == nil {
		t.Fatalf("client trusted the wrong CA and completed the handshake (server error: %v)", serverErr)
	}
	var unknownAuthority x509.UnknownAuthorityError
	if !errors.As(clientErr, &unknownAuthority) {
		t.Fatalf("client error = %v, want x509.UnknownAuthorityError", clientErr)
	}
}

func TestHandshakeRejectsTLS12WhenMinTLS13(t *testing.T) {
	f := newTestFiles(t)
	serverCfg := mustServerConfig(t, grpctls.ServerOptions{
		CertFile: f.serverCert,
		KeyFile:  f.serverKey,
	})
	clientCfg := mustClientConfig(t, grpctls.ClientOptions{
		CAFile:     f.caFile,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	})
	// Force the client to offer TLS 1.2 only so the server's TLS 1.3
	// minimum is exercised.
	clientCfg.MaxVersion = tls.VersionTLS12

	clientErr, serverErr := runHandshake(t, serverCfg, clientCfg)
	if clientErr == nil {
		t.Fatal("TLS 1.2 client completed a handshake against a TLS 1.3 minimum server")
	}
	if serverErr == nil {
		t.Fatal("TLS 1.3 minimum server did not report a protocol version error")
	}
	combined := strings.ToLower(clientErr.Error() + " " + serverErr.Error())
	if !strings.Contains(combined, "protocol") && !strings.Contains(combined, "version") {
		t.Fatalf("errors do not mention a protocol version: client=%v server=%v", clientErr, serverErr)
	}
}

func TestHandshakeAllowsTLS12WhenConfigured(t *testing.T) {
	f := newTestFiles(t)
	serverCfg := mustServerConfig(t, grpctls.ServerOptions{
		CertFile:   f.serverCert,
		KeyFile:    f.serverKey,
		MinVersion: tls.VersionTLS12,
	})
	clientCfg := mustClientConfig(t, grpctls.ClientOptions{
		CAFile:     f.caFile,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	})
	clientCfg.MaxVersion = tls.VersionTLS12

	clientErr, serverErr := runHandshake(t, serverCfg, clientCfg)
	if clientErr != nil || serverErr != nil {
		t.Fatalf("explicit TLS 1.2 handshake failed: client=%v server=%v", clientErr, serverErr)
	}
}

func mustServerConfig(t *testing.T, opts grpctls.ServerOptions) *tls.Config {
	t.Helper()
	cfg, err := grpctls.ServerConfig(opts)
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	return cfg
}

func mustClientConfig(t *testing.T, opts grpctls.ClientOptions) *tls.Config {
	t.Helper()
	cfg, err := grpctls.ClientConfig(opts)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	return cfg
}

type handshakeResult struct {
	client bool
	err    error
}

// runHandshake performs a TLS handshake over net.Pipe. It returns the client
// and server handshake errors; a successful handshake returns nil for both.
//
// net.Pipe is unbuffered, while TLS 1.3 may write a final alert or session
// ticket that the peer will not read before returning from Handshake. Wrapping
// each end in asyncConn decouples those writes so negative tests observe the
// real handshake error instead of both sides blocking until the deadline.
func runHandshake(t *testing.T, serverCfg, clientCfg *tls.Config) (clientErr, serverErr error) {
	t.Helper()
	rawClient, rawServer := net.Pipe()
	clientConn := newAsyncConn(rawClient)
	serverConn := newAsyncConn(rawServer)
	defer clientConn.Close()
	defer serverConn.Close()

	deadline := time.Now().Add(5 * time.Second)
	if err := clientConn.SetDeadline(deadline); err != nil {
		t.Fatalf("set client deadline: %v", err)
	}
	if err := serverConn.SetDeadline(deadline); err != nil {
		t.Fatalf("set server deadline: %v", err)
	}

	results := make(chan handshakeResult, 2)
	go func() {
		results <- handshakeResult{client: true, err: tls.Client(clientConn, clientCfg).Handshake()}
	}()
	go func() {
		results <- handshakeResult{client: false, err: tls.Server(serverConn, serverCfg).Handshake()}
	}()
	for i := 0; i < 2; i++ {
		r := <-results
		if r.client {
			clientErr = r.err
		} else {
			serverErr = r.err
		}
	}
	return clientErr, serverErr
}

// asyncConn buffers writes and flushes them from a dedicated goroutine. The
// underlying net.Pipe is synchronous, so a direct Write blocks until the peer
// reads; TLS can legitimately write its final flight after the peer has
// already returned from Handshake.
type asyncConn struct {
	net.Conn
	writes chan []byte
	done   chan struct{}
	once   sync.Once
}

func newAsyncConn(conn net.Conn) *asyncConn {
	c := &asyncConn{
		Conn:   conn,
		writes: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
	go func() {
		for {
			select {
			case b := <-c.writes:
				if _, err := conn.Write(b); err != nil {
					return
				}
			case <-c.done:
				return
			}
		}
	}()
	return c
}

func (c *asyncConn) Write(b []byte) (int, error) {
	buf := append([]byte(nil), b...)
	select {
	case c.writes <- buf:
		return len(b), nil
	case <-c.done:
		return 0, net.ErrClosed
	}
}

func (c *asyncConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		_ = c.Conn.Close()
	})
	return nil
}
