package main

import (
	"strings"
	"testing"
)

func TestClientTLSOptionsEmptyIsPlaintext(t *testing.T) {
	opts, err := clientTLSOptions("", "", "", "")
	if err != nil {
		t.Fatalf("clientTLSOptions: %v", err)
	}
	if opts != nil {
		t.Fatalf("empty flags = %+v, want nil (plaintext)", opts)
	}
}

func TestClientTLSOptionsRejectsCertWithoutKey(t *testing.T) {
	_, err := clientTLSOptions("", "client.pem", "", "")
	if err == nil || !strings.Contains(err.Error(), "both CertFile and KeyFile") {
		t.Fatalf("error = %v, want a certificate/key pair error", err)
	}
}

func TestClientTLSOptionsBuildsClientTLS(t *testing.T) {
	opts, err := clientTLSOptions("ca.pem", "client.pem", "client.key", "meta.internal")
	if err != nil {
		t.Fatalf("clientTLSOptions: %v", err)
	}
	if opts == nil || opts.CAFile != "ca.pem" || opts.CertFile != "client.pem" ||
		opts.KeyFile != "client.key" || opts.ServerName != "meta.internal" {
		t.Fatalf("clientTLSOptions = %+v", opts)
	}
}

func TestClientTLSOptionsServerNameOnlyEnablesTLS(t *testing.T) {
	opts, err := clientTLSOptions("", "", "", "meta.internal")
	if err != nil {
		t.Fatalf("clientTLSOptions: %v", err)
	}
	if opts == nil || opts.ServerName != "meta.internal" {
		t.Fatalf("server-name-only options = %+v, want TLS with system roots", opts)
	}
}
