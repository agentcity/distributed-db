package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"distributed-db/internal/audit"
	"distributed-db/internal/grpctls"
	"distributed-db/internal/rbac"
)

// Environment variable names for the admin RBAC and audit flags. They are read
// only when the corresponding flag was not given on the command line, so an
// explicit flag always wins (including an explicitly empty value). None of the
// variables are used as flag defaults, which keeps a token out of -help and
// out of flag parse errors.
const (
	adminTokenEnv          = "KV2_ADMIN_TOKEN"
	adminTokenRolesEnv     = "KV2_ADMIN_TOKEN_ROLES"
	adminTokenRolesFileEnv = "KV2_ADMIN_TOKEN_ROLES_FILE"
	adminMTLSRolesEnv      = "KV2_ADMIN_MTLS_ROLES"
	auditLogEnv            = "KV2_AUDIT_LOG"
	auditLogSyncEnv        = "KV2_AUDIT_LOG_SYNC"
	auditLogMaxBytesEnv    = "KV2_AUDIT_LOG_MAX_BYTES"
	auditLogMaxBackupsEnv  = "KV2_AUDIT_LOG_MAX_BACKUPS"
	auditRequiredEnv       = "KV2_AUDIT_REQUIRED"
)

// adminFlags holds the admin-auth/RBAC and audit flags for both kv2 modes. The
// same set protects the node's KV admin RPCs and the meta server's meta admin
// RPCs; use -mode=node or -mode=meta to choose which server consumes it.
type adminFlags struct {
	token           *string
	tokenRoles      *string
	tokenRolesFile  *string
	mtlsRoles       *string
	auditLog        *string
	auditLogSync    *bool
	auditMaxBytes   *int64
	auditMaxBackups *int
	auditRequired   *bool
}

// adminConfig is the validated admin configuration assembled from flags and the
// environment. It is a plain value: the parsed token map is keyed by the token
// (as the server config requires), but no method here prints or logs it.
type adminConfig struct {
	token           string
	tokenRoles      map[string]string // token -> role
	mtlsRoles       map[string]string // CN -> role
	auditLog        string
	auditSync       bool
	auditMaxBytes   int64
	auditMaxBackups int
	auditRequired   bool
}

// registerAdminFlags registers the RBAC/audit flags. The -admin-token flag is
// registered separately by registerAdminTokenFlag and attached here.
func registerAdminFlags(fs *flag.FlagSet) *adminFlags {
	f := &adminFlags{token: registerAdminTokenFlag(fs)}
	f.tokenRoles = fs.String("admin-token-roles", "",
		"comma-separated role=token pairs for RBAC-mapped bearer tokens "+
			"(env "+adminTokenRolesEnv+"); roles: admin, operator, read-only; "+
			"the token value is never printed")
	f.tokenRolesFile = fs.String("admin-token-roles-file", "",
		"file with one role=token pair per line, # comments allowed (env "+adminTokenRolesFileEnv+")")
	f.mtlsRoles = fs.String("admin-mtls-roles", "",
		"comma-separated CN=role pairs for verified mTLS clients (env "+adminMTLSRolesEnv+"); "+
			"requires -tls-client-ca")
	f.auditLog = fs.String("audit-log", "",
		"append-only JSONL admin audit path (env "+auditLogEnv+"); requires an admin credential")
	f.auditLogSync = fs.Bool("audit-log-sync", false,
		"fsync the audit file after every record (env "+auditLogSyncEnv+")")
	f.auditMaxBytes = fs.Int64("audit-log-max-bytes", audit.DefaultMaxBytes,
		"rotate the audit file at this size in bytes; 0 disables built-in rotation (env "+auditLogMaxBytesEnv+")")
	f.auditMaxBackups = fs.Int("audit-log-max-backups", audit.DefaultMaxBackups,
		"number of rotated audit files to keep; 0 discards rotated data (env "+auditLogMaxBackupsEnv+")")
	f.auditRequired = fs.Bool("audit-required", false,
		"reject protected admin RPCs when the audit sink is broken or cannot record (env "+auditRequiredEnv+")")
	return f
}

// resolveEnv fills unset flags from the environment. It must be called after
// flag.Parse. Invalid booleans/numbers are returned as errors rather than
// silently falling back to a default, and an error never includes the value
// for the token-bearing variables.
func (f *adminFlags) resolveEnv(fs *flag.FlagSet) error {
	fillString(fs, "admin-token", f.token, adminTokenEnv)
	fillString(fs, "admin-token-roles", f.tokenRoles, adminTokenRolesEnv)
	fillString(fs, "admin-token-roles-file", f.tokenRolesFile, adminTokenRolesFileEnv)
	fillString(fs, "admin-mtls-roles", f.mtlsRoles, adminMTLSRolesEnv)
	fillString(fs, "audit-log", f.auditLog, auditLogEnv)
	if !flagWasSet(fs, "audit-log-sync") {
		if raw, ok := os.LookupEnv(auditLogSyncEnv); ok && strings.TrimSpace(raw) != "" {
			v, err := strconv.ParseBool(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf("%s must be a boolean (true/false)", auditLogSyncEnv)
			}
			*f.auditLogSync = v
		}
	}
	if !flagWasSet(fs, "audit-log-max-bytes") {
		if raw, ok := os.LookupEnv(auditLogMaxBytesEnv); ok && strings.TrimSpace(raw) != "" {
			v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
			if err != nil || v < 0 {
				return fmt.Errorf("%s must be a non-negative integer", auditLogMaxBytesEnv)
			}
			*f.auditMaxBytes = v
		}
	}
	if !flagWasSet(fs, "audit-log-max-backups") {
		if raw, ok := os.LookupEnv(auditLogMaxBackupsEnv); ok && strings.TrimSpace(raw) != "" {
			v, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil || v < 0 {
				return fmt.Errorf("%s must be a non-negative integer", auditLogMaxBackupsEnv)
			}
			*f.auditMaxBackups = v
		}
	}
	if !flagWasSet(fs, "audit-required") {
		if raw, ok := os.LookupEnv(auditRequiredEnv); ok && strings.TrimSpace(raw) != "" {
			v, err := strconv.ParseBool(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf("%s must be a boolean (true/false)", auditRequiredEnv)
			}
			*f.auditRequired = v
		}
	}
	return nil
}

// build parses and validates the RBAC and audit configuration. It is called
// before any server is constructed so a bad role, a confused rotation setting,
// or mTLS roles without a client CA fail at startup instead of silently
// disabling enforcement. Errors never contain a token.
func (f *adminFlags) build(tlsCfg grpctls.Options) (adminConfig, error) {
	adm := adminConfig{
		token:           *f.token,
		auditLog:        strings.TrimSpace(*f.auditLog),
		auditSync:       *f.auditLogSync,
		auditMaxBytes:   *f.auditMaxBytes,
		auditMaxBackups: *f.auditMaxBackups,
		auditRequired:   *f.auditRequired,
	}
	inline, err := parseRoleTokenPairs(*f.tokenRoles)
	if err != nil {
		return adminConfig{}, err
	}
	fromFile := map[string]string{}
	if path := strings.TrimSpace(*f.tokenRolesFile); path != "" {
		fromFile, err = loadRoleTokenFile(path)
		if err != nil {
			return adminConfig{}, err
		}
	}
	adm.tokenRoles, err = mergeTokenRoles(inline, fromFile)
	if err != nil {
		return adminConfig{}, err
	}
	adm.mtlsRoles, err = parseMTLSRolePairs(*f.mtlsRoles)
	if err != nil {
		return adminConfig{}, err
	}
	if adm.auditMaxBytes == 0 && adm.auditMaxBackups > 0 {
		// Rotation disabled: keeping a positive backup count would be
		// meaningless, so normalize it instead of failing a --audit-log-max-bytes=0
		// invocation.
		adm.auditMaxBackups = 0
	}
	if len(adm.mtlsRoles) > 0 && tlsCfg.Server.ClientCAFile == "" {
		return adminConfig{}, errors.New("-admin-mtls-roles requires -tls-client-ca: a Common Name is only trusted when the listener verified the client certificate")
	}
	switch {
	case adm.auditMaxBytes < 0:
		return adminConfig{}, errors.New("-audit-log-max-bytes must not be negative")
	case adm.auditMaxBackups < 0:
		return adminConfig{}, errors.New("-audit-log-max-backups must not be negative")
	case adm.auditRequired && adm.auditLog == "":
		return adminConfig{}, errors.New("-audit-required requires -audit-log")
	case adm.auditLog != "" && !adm.authEnabled():
		return adminConfig{}, errors.New("-audit-log requires at least one admin credential (-admin-token, -admin-token-roles or -admin-mtls-roles)")
	}
	return adm, nil
}

// authEnabled reports whether any admin credential source is configured.
func (a adminConfig) authEnabled() bool {
	return a.token != "" || len(a.tokenRoles) > 0 || len(a.mtlsRoles) > 0
}

// parseRoleTokenPairs parses "role=token,role=token". Role is on the left so a
// token may itself contain '='. Entries are rejected when the role is unknown
// or the pair is malformed; error messages never echo the token.
func parseRoleTokenPairs(raw string) (map[string]string, error) {
	out := make(map[string]string)
	for i, entry := range splitList(raw) {
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 || eq == len(entry)-1 {
			return nil, fmt.Errorf("admin-token-roles entry %d is not role=token", i+1)
		}
		roleRaw := strings.TrimSpace(entry[:eq])
		token := strings.TrimSpace(entry[eq+1:])
		role, ok := rbac.ParseRole(roleRaw)
		if !ok || token == "" {
			if !ok {
				return nil, fmt.Errorf("admin-token-roles entry %d has unknown role %q", i+1, roleRaw)
			}
			return nil, fmt.Errorf("admin-token-roles entry %d has an empty token", i+1)
		}
		if prev, exists := out[token]; exists && prev != string(role) {
			return nil, fmt.Errorf("admin-token-roles entry %d maps a token to conflicting roles", i+1)
		}
		out[token] = string(role)
	}
	return out, nil
}

// parseMTLSRolePairs parses "CN=role,CN=role". The role is the suffix after the
// last '=', so a CN may contain '='. A CN mapped to two different roles is an
// error rather than a silent last-wins.
func parseMTLSRolePairs(raw string) (map[string]string, error) {
	out := make(map[string]string)
	for i, entry := range splitList(raw) {
		eq := strings.LastIndexByte(entry, '=')
		if eq <= 0 || eq == len(entry)-1 {
			return nil, fmt.Errorf("admin-mtls-roles entry %d is not CN=role", i+1)
		}
		cn := strings.TrimSpace(entry[:eq])
		roleRaw := strings.TrimSpace(entry[eq+1:])
		role, ok := rbac.ParseRole(roleRaw)
		if !ok {
			return nil, fmt.Errorf("admin-mtls-roles entry %d has unknown role %q", i+1, roleRaw)
		}
		if cn == "" {
			return nil, fmt.Errorf("admin-mtls-roles entry %d has an empty Common Name", i+1)
		}
		if prev, exists := out[cn]; exists && prev != string(role) {
			return nil, fmt.Errorf("admin-mtls-roles maps Common Name %q to conflicting roles", cn)
		}
		out[cn] = string(role)
	}
	return out, nil
}

// loadRoleTokenFile reads role=token lines. Blank lines and lines whose first
// non-space character is '#' are ignored. Error messages include the line
// number and file path, never the line contents (which contain tokens).
func loadRoleTokenFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read -admin-token-roles-file: %w", err)
	}
	out := make(map[string]string)
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 || eq == len(line)-1 {
			return nil, fmt.Errorf("-admin-token-roles-file %s line %d is not role=token", path, i+1)
		}
		roleRaw := strings.TrimSpace(line[:eq])
		token := strings.TrimSpace(line[eq+1:])
		role, ok := rbac.ParseRole(roleRaw)
		if !ok {
			return nil, fmt.Errorf("-admin-token-roles-file %s line %d has unknown role %q", path, i+1, roleRaw)
		}
		if token == "" {
			return nil, fmt.Errorf("-admin-token-roles-file %s line %d has an empty token", path, i+1)
		}
		if prev, exists := out[token]; exists && prev != string(role) {
			return nil, fmt.Errorf("-admin-token-roles-file %s line %d maps a token to conflicting roles", path, i+1)
		}
		out[token] = string(role)
	}
	return out, nil
}

// mergeTokenRoles combines inline and file credentials. A token present in both
// with different roles is an error and the message does not echo the token.
func mergeTokenRoles(inline, fromFile map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(inline)+len(fromFile))
	for token, role := range inline {
		out[token] = role
	}
	for token, role := range fromFile {
		if prev, exists := out[token]; exists && prev != role {
			return nil, errors.New("a token is configured with different roles via -admin-token-roles and -admin-token-roles-file")
		}
		out[token] = role
	}
	return out, nil
}

// fillString applies an environment fallback for one string flag unless the flag
// was set explicitly on the command line.
func fillString(fs *flag.FlagSet, flagName string, target *string, envName string) {
	if flagWasSet(fs, flagName) {
		return
	}
	if v, ok := os.LookupEnv(envName); ok {
		*target = v
	}
}

// flagWasSet reports whether name appeared on the command line.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == name {
			found = true
		}
	})
	return found
}
