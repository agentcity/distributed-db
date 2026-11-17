# SQL HTTP gateway security

`internal/sqlhttp` serves `POST /v1/sql`, the Prometheus/SLO endpoints, and the
liveness probes. It supports optional TLS and one authentication mechanism:
a Bearer token.

## Endpoint policy

| Endpoint          | Methods                 | Authentication        |
| ----------------- | ----------------------- | --------------------- |
| `/v1/sql`         | `POST`                  | Bearer required       |
| `/metrics`        | `GET`                   | Bearer required       |
| `/debug/slo`      | `GET`                   | Bearer required       |
| `/health`         | `GET`                   | public                |
| `/healthz`        | `GET`                   | public                |
| anything else     | any                     | Bearer required (401 when unauthenticated) |

`/health` and `/healthz` stay public so container, orchestrator, and load
balancer probes do not need the API secret. `/metrics` and `/debug/slo` use
the same token as SQL; configure the scrape job (for example a Prometheus
`authorization` block with `type: Bearer`) to send it. When `AuthToken` is
empty the historical development behavior is preserved: everything is public.

## Bearer authentication

Set `Config.AuthToken` and send:

```http
Authorization: Bearer <token>
```

The handler accepts exactly one `Authorization` header, compares the token in
constant time (both sides are hashed to a fixed-length SHA-256 digest before
`crypto/subtle.ConstantTimeCompare`), and never includes the configured token
in a response, error, or log line. A missing or invalid token receives:

```http
HTTP/1.1 401 Unauthorized
WWW-Authenticate: Bearer realm="sqlhttp"
```

## TLS

Enable TLS with either a certificate pair:

```go
sqlhttp.Config{
    Engine:      engine,
    TLSCertFile: "server.crt",
    TLSKeyFile:  "server.key",
    AuthToken:   token,
}
```

or a caller-supplied `TLSConfig` (it is cloned, never mutated). The two styles
may be combined; certificate files are appended to the cloned `TLSConfig`.
`NewServer` raises `MinVersion` to TLS 1.2 when the caller leaves it lower,
keeps TLS 1.3 available through negotiation, and rejects a config without a
server certificate.

`Config.Validate` and `NewServer` reject `AuthToken` when no TLS is configured.
`NewWithConfig` is the lower-level handler constructor: it enforces the token
but does not own a listener, so it can be used when TLS terminates in a trusted
in-process or sidecar proxy.

## `cmd/sqlsrv` flags

| Flag              | Meaning                                                          |
| ----------------- | ---------------------------------------------------------------- |
| `-tls-cert FILE`  | PEM server certificate for the SQL HTTP listener                 |
| `-tls-key FILE`   | PEM private key for `-tls-cert` (both or neither)                |
| `-auth-token TOK` | Bearer token for `/v1/sql`, `/metrics`, `/debug/slo`             |
| `-tls-ca FILE`    | CA bundle for meta/KV verification; enables cluster TLS          |
| `-tls-server-name`| Expected meta/KV server name; enables cluster TLS                |

`-tls-cert`/`-tls-key` configure the inbound SQL listener. They are also
presented as the client certificate on meta/KV dials when cluster TLS is
enabled through `-tls-ca` or `-tls-server-name`. Enabling TLS on the listener
alone keeps a plaintext cluster connection working.

Startup fails with a non-zero exit status when:

- only one of `-tls-cert`/`-tls-key` is set;
- the pair cannot be loaded (unreadable, malformed, or mismatched);
- `-auth-token` is set without `-tls-cert`/`-tls-key` (tokens are never sent
  over plaintext).

Without `-tls-cert`/`-tls-key` and `-auth-token`, the server starts in the
development mode (plaintext, no auth) and logs warnings for both omissions.
