package main

import (
	"strings"
	"testing"
)

func TestPGWireConfig(t *testing.T) {
	cases := []struct {
		name         string
		tlsCert      string
		tlsKey       string
		authUser     string
		authPassword string
		wantErr      string
	}{
		{
			name: "plain no auth",
		},
		{
			name:         "TLS with password auth",
			tlsCert:      "server.crt",
			tlsKey:       "server.key",
			authUser:     "alice",
			authPassword: "secret",
		},
		{
			name:         "auth without TLS",
			authUser:     "alice",
			authPassword: "secret",
			wantErr:      "requires TLS",
		},
		{
			name:    "cert without key",
			tlsCert: "server.crt",
			wantErr: "set together",
		},
		{
			name:    "key without cert",
			tlsKey:  "server.key",
			wantErr: "set together",
		},
		{
			name:     "user without password",
			tlsCert:  "server.crt",
			tlsKey:   "server.key",
			authUser: "alice",
			wantErr:  "set together",
		},
		{
			name:         "password without user",
			tlsCert:      "server.crt",
			tlsKey:       "server.key",
			authPassword: "secret",
			wantErr:      "set together",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := pgwireConfig(tc.tlsCert, tc.tlsKey, tc.authUser, tc.authPassword)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("pgwireConfig() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("pgwireConfig(): %v", err)
			}
			if cfg.TLSCertFile != tc.tlsCert || cfg.TLSKeyFile != tc.tlsKey {
				t.Fatalf("TLS files = %q/%q, want %q/%q", cfg.TLSCertFile, cfg.TLSKeyFile, tc.tlsCert, tc.tlsKey)
			}
			if cfg.AuthUser != tc.authUser || cfg.AuthPassword != tc.authPassword {
				t.Fatalf("auth = %q/%q, want configured credentials", cfg.AuthUser, cfg.AuthPassword)
			}
			if got, want := cfg.TLSEnabled(), tc.tlsCert != ""; got != want {
				t.Fatalf("TLSEnabled() = %v, want %v", got, want)
			}
			if got, want := cfg.AuthEnabled(), tc.authUser != ""; got != want {
				t.Fatalf("AuthEnabled() = %v, want %v", got, want)
			}
		})
	}
}
