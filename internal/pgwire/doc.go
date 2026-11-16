// Package pgwire implements a deliberately small PostgreSQL v3 wire-protocol
// front end for the P0 SQL engine.
//
// Supported:
//   - Optional TLS via tls.NewListener (Config.TLSConfig or
//     Config.TLSCertFile/TLSKeyFile), with TLS 1.2 as the minimum version and
//     TLS 1.3 negotiated when both peers support it
//   - Optional PostgreSQL cleartext password authentication over TLS
//     (Config.AuthUser/AuthPassword or Config.PasswordAuth); failures are
//     reported as SQLSTATE 28P01
//   - StartupMessage v3.0; SSLRequest and GSSENCRequest are declined with 'N'
//   - ParameterStatus for server_version, client_encoding, and DateStyle
//   - Simple Query ('Q'): one SQL statement per message
//   - Extended query protocol: Parse ('P'), Bind ('B'), Describe ('D'),
//     Execute ('E'), Sync ('S'), Close ('C'), and Flush ('H')
//   - Named and unnamed prepared statements, parameter placeholders ($1, $2,
//     ...), and multiple portals
//   - ParameterDescription, RowDescription, NoData, DataRow, CommandComplete,
//     PortalSuspended, ParseComplete, BindComplete, and CloseComplete
//   - Text and binary result formats for INT8 (OID 20) and TEXT (OID 25);
//     text and binary parameter values for the same types
//   - Execute row limits with portal suspension and resumption (portals are
//     dropped at Sync, matching the implicit transaction)
//   - ErrorResponse with SQLSTATE (42601, 42P01, 42P05, 42P03, 26000, 34000,
//     08P01, 0A000, 22P02, 22P03, 23505, 42804, ...) and Sync-based recovery
//   - Terminate ('X'); unknown messages get ErrorResponse 08P01 + ReadyForQuery
//
// Not supported:
//   - In-band TLS negotiation (SSLRequest is declined with 'N'): when TLS is
//     configured, the listener expects the TLS handshake immediately, so
//     clients must use direct TLS rather than libpq's default SSLRequest
//     upgrade (for libpq this means sslnegotiation=direct).
//   - MD5/SCRAM authentication, GSSAPI encryption, client certificates,
//     authorization/RBAC, and multi-user credential stores. Password auth is
//     cleartext inside TLS only; Config.Validate refuses it without TLS.
//   - transactions/session state, SET, COPY, notices, notifications, and
//     cancel requests
//   - system catalogs (pg_catalog), so psql meta-commands such as \dt, \l, and
//     \d fail; only ordinary SQL statements are understood
//   - NULL parameters (Bind reports 0A000), binary formats other than INT8/TEXT,
//     and parameter type OIDs other than the INT/TEXT families
//
// The zero Config (and the historical New constructor) keeps the development
// behavior: plain TCP, no authentication. That mode must not be exposed on an
// untrusted network.
//
// Parameter binding is implemented by substituting safely quoted literals into
// the SQL text because the P0 engine has no parameter API. Parameter types are
// taken from Parse when supplied, otherwise inferred from the statement shape
// and the table schema; unresolved types are reported as TEXT and may be
// rendered as either an integer or a quoted string at Bind time.
//
// Describe derives field metadata from the table schema when possible. Because
// the engine has no describe-only API, statements whose metadata cannot be
// inferred (for example JOINs) may be run with dummy parameter values to
// obtain column types; Describe never executes DML. Execute row limits are
// applied to the already-buffered engine result rather than streaming from
// storage.
//
// RowDescription types are inferred from the table schema when possible (so an
// empty result still reports INT8/TEXT correctly) and otherwise from the first
// non-NULL value of each column; an unknown column type is reported as TEXT. A
// NULL value is encoded with length -1. The engine executes exactly one
// statement per Query message; multi-statement strings are rejected by the
// parser.
package pgwire
