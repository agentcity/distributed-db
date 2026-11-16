package coordinator

import (
	"errors"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// connPool caches one *grpc.ClientConn per node address. Connections are
// dialed lazily and reused across requests, so a coordinator request never
// creates a connection of its own.
type connPool struct {
	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
	creds credentials.TransportCredentials
	// closed is set by closeAll; get refuses to dial afterwards.
	closed bool
}

func newConnPool() *connPool {
	return &connPool{
		conns: make(map[string]*grpc.ClientConn),
		creds: insecure.NewCredentials(),
	}
}

// setCredentials replaces the transport credentials used for new connections.
// It must be called before the first dial (Start calls it with the TLS
// configuration); existing connections are not re-dialed.
func (p *connPool) setCredentials(creds credentials.TransportCredentials) {
	if creds == nil {
		return
	}
	p.mu.Lock()
	p.creds = creds
	p.mu.Unlock()
}

// get returns the cached connection for addr, dialing it on first use.
// grpc.NewClient is lazy, so a successful return does not imply that the node
// is reachable; the first RPC reports connection errors.
func (p *connPool) get(addr string) (*grpc.ClientConn, error) {
	if addr == "" {
		return nil, errors.New("coordinator: empty node address")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("coordinator: connection pool is closed")
	}
	if cc, ok := p.conns[addr]; ok {
		return cc, nil
	}
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(p.creds))
	if err != nil {
		return nil, fmt.Errorf("coordinator: dial %s: %w", addr, err)
	}
	p.conns[addr] = cc
	return cc, nil
}

// closeAll closes every pooled connection. It is safe to call more than once.
func (p *connPool) closeAll() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	conns := p.conns
	p.conns = make(map[string]*grpc.ClientConn)
	p.mu.Unlock()

	var errs []error
	for addr, cc := range conns {
		if err := cc.Close(); err != nil {
			errs = append(errs, fmt.Errorf("coordinator: close connection to %s: %w", addr, err))
		}
	}
	return errors.Join(errs...)
}
