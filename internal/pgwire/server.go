package pgwire

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"distributed-db/internal/sqlengine"
)

const (
	protocolVersion3 uint32 = 196608 // 3.0

	sslRequestCode    uint32 = 80877103
	gssEncRequestCode uint32 = 80877104
	cancelRequestCode uint32 = 80877102

	maxStartupPacket       = 1 << 20  // 1 MiB
	maxMessageSize   int32 = 16 << 20 // 16 MiB
	maxNegotiations        = 4
	startupTimeout         = 10 * time.Second
)

var (
	errBadMessage    = errors.New("pgwire: malformed message")
	errCancelRequest = errors.New("pgwire: cancel request")
)

// Server is a minimal PostgreSQL wire-protocol server backed by a SQL engine.
// A Server is safe for concurrent use: one goroutine serves each connection.
type Server struct {
	engine *sqlengine.Engine
	logger *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	ln     net.Listener
	addr   string
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup

	nextPID atomic.Int32
}

// New returns a server that executes SQL with engine. A nil logger uses
// slog.Default.
func New(engine *sqlengine.Engine, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		engine: engine,
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
		conns:  make(map[net.Conn]struct{}),
	}
	s.nextPID.Store(int32(os.Getpid()))
	return s
}

// Start listens on addr (for example ":5432" or "127.0.0.1:0") and returns the
// actual address. It starts accepting connections in the background.
func (s *Server) Start(addr string) (string, error) {
	if s.engine == nil {
		return "", errors.New("pgwire: nil engine")
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("pgwire: server is stopped")
	}
	if s.ln != nil {
		s.mu.Unlock()
		return "", errors.New("pgwire: server is already started")
	}
	s.mu.Unlock()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("pgwire: listen %q: %w", addr, err)
	}
	actual := ln.Addr().String()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ln.Close()
		return "", errors.New("pgwire: server is stopped")
	}
	if s.ln != nil {
		s.mu.Unlock()
		_ = ln.Close()
		return "", errors.New("pgwire: server is already started")
	}
	s.ln = ln
	s.addr = actual
	s.wg.Add(1)
	s.mu.Unlock()

	go s.acceptLoop(ln)
	return actual, nil
}

// Addr returns the address the server is listening on, or an empty string if
// Start has not been called.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Stop closes the listener and all active connections, then waits for their
// handler goroutines to finish. Stop is idempotent.
func (s *Server) Stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.wg.Wait()
		return
	}
	s.closed = true
	s.cancel()

	ln := s.ln
	s.ln = nil
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
}

func (s *Server) acceptLoop(ln net.Listener) {
	defer s.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return
			}
			s.logger.Warn("pgwire: accept failed", "err", err)
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()

		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
				_ = conn.Close()
			}()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	remote := conn.RemoteAddr().String()
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)

	params, err := s.startup(conn, br, bw)
	if err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) &&
			!errors.Is(err, errCancelRequest) {
			s.logger.Debug("pgwire: startup failed", "remote", remote, "err", err)
		}
		return
	}
	s.logger.Debug("pgwire: client connected",
		"remote", remote, "user", params["user"], "database", params["database"])

	for {
		typ, body, err := readMessage(br)
		if err != nil {
			if errors.Is(err, errBadMessage) {
				_ = writeError(bw, "08P01", err.Error())
				_ = bw.Flush()
			} else if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.logger.Debug("pgwire: read message failed", "remote", remote, "err", err)
			}
			return
		}

		switch typ {
		case msgQuery:
			if err := s.simpleQuery(bw, body); err != nil {
				s.logger.Debug("pgwire: write failed", "remote", remote, "err", err)
				return
			}
		case msgTerminate:
			return
		default:
			if err := writeError(bw, "08P01", fmt.Sprintf("pgwire: unsupported message type %q", typ)); err != nil {
				return
			}
			if err := writeReadyForQuery(bw, 'I'); err != nil {
				return
			}
			if err := bw.Flush(); err != nil {
				return
			}
		}
	}
}

// startup performs the v3 startup exchange. It returns the startup parameters
// (user, database, application_name, ...) after ReadyForQuery was sent.
func (s *Server) startup(conn net.Conn, br *bufio.Reader, bw *bufio.Writer) (map[string]string, error) {
	_ = conn.SetReadDeadline(time.Now().Add(startupTimeout))
	defer conn.SetReadDeadline(time.Time{})

	for i := 0; i < maxNegotiations; i++ {
		body, err := readStartupPacket(br)
		if err != nil {
			return nil, err
		}
		if len(body) < 4 {
			return nil, fmt.Errorf("%w: startup packet too short", errBadMessage)
		}
		code := binary.BigEndian.Uint32(body[:4])
		switch code {
		case sslRequestCode, gssEncRequestCode:
			// Decline encryption; the client then retries in plaintext.
			if _, err := bw.Write([]byte{'N'}); err != nil {
				return nil, err
			}
			if err := bw.Flush(); err != nil {
				return nil, err
			}
			continue
		case cancelRequestCode:
			return nil, errCancelRequest
		case protocolVersion3:
			params, err := parseStartupParams(body[4:])
			if err != nil {
				return nil, err
			}
			if err := s.sendStartupResponses(bw); err != nil {
				return nil, err
			}
			if err := bw.Flush(); err != nil {
				return nil, err
			}
			return params, nil
		default:
			err := fmt.Errorf("pgwire: unsupported protocol version %d", code)
			if werr := writeError(bw, "08P01", err.Error()); werr != nil {
				return nil, werr
			}
			if werr := bw.Flush(); werr != nil {
				return nil, werr
			}
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: too many startup negotiation packets", errBadMessage)
}

func (s *Server) sendStartupResponses(w io.Writer) error {
	var auth [4]byte
	if err := writeMessage(w, msgAuthenticationOk, auth[:]); err != nil {
		return err
	}
	for _, p := range [][2]string{
		{"server_version", "14.0"},
		{"client_encoding", "UTF8"},
		{"DateStyle", "ISO"},
	} {
		var body []byte
		body = appendCString(body, p[0])
		body = appendCString(body, p[1])
		if err := writeMessage(w, msgParameterStatus, body); err != nil {
			return err
		}
	}
	pid, secret := s.backendKey()
	var key [8]byte
	binary.BigEndian.PutUint32(key[0:4], uint32(pid))
	binary.BigEndian.PutUint32(key[4:8], uint32(secret))
	if err := writeMessage(w, msgBackendKeyData, key[:]); err != nil {
		return err
	}
	return writeReadyForQuery(w, 'I')
}

// simpleQuery executes one SQL statement and writes its full response,
// including ReadyForQuery, then flushes the buffer.
func (s *Server) simpleQuery(w *bufio.Writer, body []byte) error {
	query := strings.TrimSpace(strings.TrimRight(string(body), "\x00"))
	if query == "" {
		if err := writeMessage(w, msgEmptyQueryResponse, nil); err != nil {
			return err
		}
	} else {
		res, err := s.engine.Execute(s.ctx, query)
		switch {
		case err != nil:
			if werr := writeError(w, sqlState(err), err.Error()); werr != nil {
				return werr
			}
		case res == nil:
			if werr := writeError(w, "XX000", "pgwire: engine returned a nil result"); werr != nil {
				return werr
			}
		default:
			if werr := writeResult(w, query, res); werr != nil {
				return werr
			}
		}
	}
	if err := writeReadyForQuery(w, 'I'); err != nil {
		return err
	}
	return w.Flush()
}
