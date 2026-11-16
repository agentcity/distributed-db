// Package pgwire implements a deliberately small PostgreSQL v3 wire-protocol
// front end for the P0 SQL engine.
//
// Supported:
//   - StartupMessage v3.0; SSLRequest and GSSENCRequest are declined with 'N'
//   - ParameterStatus for server_version, client_encoding, and DateStyle
//   - Simple Query ('Q'): one SQL statement per message
//   - Text-format RowDescription/DataRow for INT (OID 20) and TEXT (OID 25)
//   - CommandComplete tags for SELECT/INSERT/UPDATE/DELETE, "OK" otherwise
//   - ErrorResponse with SQLSTATE 42601 (syntax/unsupported) or XX000
//   - Terminate ('X'); unknown messages get ErrorResponse 08P01 + ReadyForQuery
//
// Not supported:
//   - TLS/SSL (requests are refused), GSSAPI encryption, authentication/passwords
//   - extended query protocol (Parse/Bind/Describe/Execute), prepared statements,
//     portals, COPY, notices, notifications, and cancel requests
//   - transactions/session state, SET, binary formats, and type inference beyond
//     int64 -> INT8 and string -> TEXT
//   - system catalogs (pg_catalog), so psql meta-commands such as \dt, \l, and
//     \d fail; only ordinary SQL statements are understood
//
// psql can connect and run simple SQL through the simple-query protocol. Most
// PostgreSQL drivers (JDBC, pgx in default mode, etc.) rely on the extended
// protocol and are not supported.
//
// The engine executes exactly one statement per Query message; multi-statement
// strings are rejected by the parser. RowDescription types are inferred from
// the first non-NULL value of each column, so an all-NULL column is reported as
// TEXT. A NULL value is encoded with length -1.
package pgwire
