package node

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"

	"distributed-db/internal/pb"
)

// loadReporter periodically reports this node's operation count and its recent
// operation rate to the coordinator. Reporting is strictly best effort:
// failures are logged at debug level and never affect request handling.
type loadReporter struct {
	node   *Node
	client pb.CoordinatorServiceClient
	conn   *grpc.ClientConn
	log    *slog.Logger

	// mu guards the previous sample used to compute ops/sec. reportLoad is
	// currently called from a single goroutine, but the mutex documents and
	// enforces that the sample is read and updated atomically.
	mu         sync.Mutex
	lastOps    uint64
	lastReport time.Time

	closeOnce sync.Once
}

func newLoadReporter(cfg Config, n *Node) (*loadReporter, error) {
	creds, err := clientTransportCredentials(cfg)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(cfg.CoordinatorAddr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}
	return &loadReporter{
		node:   n,
		client: pb.NewCoordinatorServiceClient(conn),
		conn:   conn,
		log:    cfg.Logger,
		// The first report covers the interval since the reporter was created,
		// so load is meaningful immediately instead of always starting at zero.
		lastReport: time.Now(),
	}, nil
}

func (n *Node) reportLoop(ctx context.Context) {
	defer n.wg.Done()

	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.reportLoad(ctx)
		}
	}
}

func (n *Node) reportLoad(ctx context.Context) {
	ops := n.ops.Load()
	load := n.reporter.observe(ops, time.Now())

	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	_, err := n.reporter.client.ReportLoad(reqCtx, &pb.LoadReportRequest{
		NodeId: n.cfg.ID,
		Shard:  n.cfg.Shard,
		Load:   load,
		Ops:    ops,
	})
	if err != nil {
		// Debug level on purpose: the coordinator may not be up yet.
		n.reporter.log.Debug("load report failed", "coordinator", n.cfg.CoordinatorAddr, "error", err)
	}
}

// observe records a counter sample and returns the operations-per-second rate
// since the previous sample. A reporter with no previous timestamp reports
// zero for its first sample.
func (r *loadReporter) observe(ops uint64, now time.Time) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	load := opsPerSecond(r.lastOps, r.lastReport, ops, now)
	r.lastOps = ops
	r.lastReport = now
	return load
}

// opsPerSecond computes a rate between two counter samples. It returns zero
// for the first sample, a non-positive interval, or a counter reset.
func opsPerSecond(prevOps uint64, prevTime time.Time, ops uint64, now time.Time) float64 {
	if prevTime.IsZero() {
		return 0
	}
	elapsed := now.Sub(prevTime).Seconds()
	if elapsed <= 0 {
		return 0
	}
	if ops < prevOps {
		// Counter went backwards (reset or wrap): do not report a negative rate.
		return 0
	}
	return float64(ops-prevOps) / elapsed
}

// Close releases the coordinator connection. It is safe to call more than once.
func (r *loadReporter) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		if r.conn != nil {
			_ = r.conn.Close()
		}
	})
}
