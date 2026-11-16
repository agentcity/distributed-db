package test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// This file implements a crash-consistency harness for a single-node kvserver.
//
// TestCrashConsistency starts the test binary as a child process. The child
// commits N writes through CommitSingle, reads each one back, prints
// "READY <n>", and then blocks. The parent kills the child with SIGKILL,
// restarts a kvserver on the same DataDir, and verifies that every
// acknowledged key is still readable.
//
// The "fast" subtest runs with DurabilityMode="fast" and
// SyncStateMachine=false. That mode relies on the durable Raft log instead of
// fsyncing every Pebble state-machine batch, so acknowledged writes must still
// survive. The "sync" subtest forces DurabilityMode="sync" (and
// SyncStateMachine=true) as the control that proves the harness itself can
// recover a cleanly persisted state machine.

const (
	crashChildEnv   = "CRASH_CONSISTENCY_CHILD"
	crashDataDirEnv = "CRASH_CONSISTENCY_DATA_DIR"
	crashAddrEnv    = "CRASH_CONSISTENCY_ADDR"
	crashNEnv       = "CRASH_CONSISTENCY_N"
	crashSyncEnv    = "CRASH_CONSISTENCY_SYNC"

	// crashDefaultN is deliberately modest so the test is usable in CI.
	crashDefaultN = 32

	// crashTSBase is an explicit startTS base. The test intentionally bypasses
	// the meta timestamp allocator: CommitSingle only requires startTS to be
	// positive, monotonic, and smaller than commitTS.
	crashTSBase uint64 = 1 << 40
)

type crashMode struct {
	name       string
	durability string
	forceSync  bool
}

// TestCrashConsistency is the opt-in crash test. It is skipped under -short.
func TestCrashConsistency(t *testing.T) {
	if testing.Short() {
		t.Skip("crash-consistency test launches and kills child processes; skipped in -short mode")
	}
	n := crashWriteCount(t)
	for _, mode := range []crashMode{
		{name: "fast", durability: kvserver.DurabilityFast, forceSync: false},
		{name: "sync", durability: kvserver.DurabilitySync, forceSync: true},
	} {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			crashRound(t, mode, n)
		})
	}
}

// TestCrashConsistencyChild is not a normal test. TestCrashConsistency starts
// it with -test.run and CRASH_CONSISTENCY_CHILD=1, waits for READY, then kills
// it. Without that environment it skips, so a package-wide test run is
// unaffected.
func TestCrashConsistencyChild(t *testing.T) {
	if os.Getenv(crashChildEnv) != "1" {
		t.Skip("crash-consistency child helper; TestCrashConsistency starts it")
	}
	dataDir := os.Getenv(crashDataDirEnv)
	addr := os.Getenv(crashAddrEnv)
	if dataDir == "" || addr == "" {
		t.Fatalf("%s and %s must be set in child mode", crashDataDirEnv, crashAddrEnv)
	}
	n, err := strconv.Atoi(os.Getenv(crashNEnv))
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q must be a positive integer", crashNEnv, os.Getenv(crashNEnv))
	}
	mode := crashMode{name: "fast", durability: kvserver.DurabilityFast, forceSync: false}
	if os.Getenv(crashSyncEnv) == "1" {
		mode = crashMode{name: "sync", durability: kvserver.DurabilitySync, forceSync: true}
	}

	srv := crashStartServer(t, mode, dataDir, addr)
	crashWaitLeader(t, srv, 30*time.Second)

	client, conn := crashDial(t, addr)
	t.Cleanup(func() { _ = conn.Close() })

	for i := 0; i < n; i++ {
		key := crashKey(mode.name, i)
		value := crashValue(i)
		startTS, commitTS := crashTS(i)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := client.CommitSingle(ctx, &pbv2.CommitSingleRequest{
			StartTs:   startTS,
			CommitTs:  commitTS,
			Primary:   key,
			Mutations: []*pbv2.Mutation{{Key: key, Value: value}},
		})
		cancel()
		if err != nil {
			t.Fatalf("%s: CommitSingle key %q: %v", mode.name, key, err)
		}
		if !resp.GetSuccess() {
			t.Fatalf("%s: CommitSingle key %q was not acknowledged: error=%q leader=%q",
				mode.name, key, resp.GetError(), resp.GetLeader())
		}

		// Acknowledged writes must be visible before the crash. This also
		// proves the child reached READY with fully applied state.
		readCtx, readCancel := context.WithTimeout(context.Background(), 10*time.Second)
		got, err := client.Get(readCtx, &pbv2.GetRequest{Key: key, Ts: commitTS})
		readCancel()
		if err != nil || got.GetError() != "" || !got.GetFound() || !bytes.Equal(got.GetValue(), value) {
			t.Fatalf("%s: read-back of acknowledged key %q: err=%v resp=%+v",
				mode.name, key, err, got)
		}
	}

	fmt.Printf("READY %d\n", n)
	_ = os.Stdout.Sync()

	// Block until the parent kills this process. Stdin EOF is a second line of
	// defence: if the parent dies, the pipe closes and the child exits instead
	// of becoming an orphan.
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// crashRound performs one kill-and-restart cycle for a storage mode.
func crashRound(t *testing.T, mode crashMode, n int) {
	t.Helper()
	dataDir := t.TempDir()
	addr := crashFreeAddr(t)

	child := crashStartChild(t, mode, dataDir, addr, n)
	defer child.cleanup()

	child.awaitReady(t, n, 60*time.Second)

	// Simulate an abrupt node failure. Do not stop the child's server and do
	// not let the child run any cleanup before it dies.
	waitErr := child.killAndWait(t, 10*time.Second)
	_ = child.stdin.Close()
	if waitErr == nil {
		t.Fatalf("%s: child exited cleanly after SIGKILL; the crash test did not exercise a crash", mode.name)
	}
	t.Logf("%s: child was killed after READY %d (wait error: %v)", mode.name, n, waitErr)

	// Restart on the exact same DataDir and address, this time in the parent.
	srv := crashStartServer(t, mode, dataDir, addr)
	crashWaitLeader(t, srv, 30*time.Second)
	t.Logf("%s: restarted server became leader on %s", mode.name, addr)

	client, conn := crashDial(t, addr)
	defer func() { _ = conn.Close() }()

	lost, wrong := crashReadAcknowledged(t, client, mode, n)
	if len(lost) != 0 || len(wrong) != 0 {
		t.Fatalf("%s mode (DurabilityMode=%q, SyncStateMachine=%t): acknowledged writes did not survive SIGKILL + restart: %d/%d keys missing, %d with wrong values\nmissing keys: [%s]\nwrong values: [%s]",
			mode.name, mode.durability, mode.forceSync, len(lost), n, len(wrong),
			strings.Join(lost, ", "), strings.Join(wrong, ", "))
	}
	t.Logf("%s: all %d acknowledged CommitSingle writes survived SIGKILL and restart", mode.name, n)
}

func crashWriteCount(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("CRASH_N")
	if raw == "" {
		return crashDefaultN
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		t.Fatalf("CRASH_N=%q must be a positive integer", raw)
	}
	return n
}

func crashServerConfig(mode crashMode, dataDir, addr string) kvserver.Config {
	return kvserver.Config{
		ID:       addr,
		RaftAddr: addr,
		DataDir:  dataDir,
		// Legacy single-range configuration: one range, no peers, i.e. a
		// single-node Raft group covering the whole keyspace.
		RangeID:            1,
		ElectionTimeoutMin: 80 * time.Millisecond,
		ElectionTimeoutMax: 160 * time.Millisecond,
		HeartbeatInterval:  20 * time.Millisecond,
		CacheSize:          8 << 20,
		MemTableSize:       4 << 20,
		Logger:             crashLogger(),
		DurabilityMode:     mode.durability,
		SyncStateMachine:   mode.forceSync,
	}
}

func crashStartServer(t *testing.T, mode crashMode, dataDir, addr string) *kvserver.Server {
	t.Helper()
	srv, err := kvserver.New(crashServerConfig(mode, dataDir, addr))
	if err != nil {
		t.Fatalf("%s: open kvserver on %s: %v", mode.name, dataDir, err)
	}
	if err := srv.Start(context.Background()); err != nil {
		_ = srv.Stop(context.Background())
		t.Fatalf("%s: start kvserver on %s: %v", mode.name, addr, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	return srv
}

func crashWaitLeader(t *testing.T, srv *kvserver.Server, timeout time.Duration) {
	t.Helper()
	node := srv.Raft()
	if node == nil {
		t.Fatal("kvserver has no default raft group")
	}
	deadline := time.Now().Add(timeout)
	for {
		status := node.Status()
		if status.Role == "leader" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no leader within %s (role=%q leader=%q term=%d)",
				timeout, status.Role, status.Leader, status.Term)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func crashDial(t *testing.T, addr string) (pbv2.KVServiceClient, *grpc.ClientConn) {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial crash test server %s: %v", addr, err)
	}
	return pbv2.NewKVServiceClient(conn), conn
}

// crashReadAcknowledged reads every key the child acknowledged. Transient RPC
// errors are retried; a successful "not found" response is treated as data
// loss, never as a reason to wait longer.
func crashReadAcknowledged(t *testing.T, client pbv2.KVServiceClient, mode crashMode, n int) (lost, wrong []string) {
	t.Helper()
	for i := 0; i < n; i++ {
		key := crashKey(mode.name, i)
		want := crashValue(i)
		_, commitTS := crashTS(i)

		resp, err := crashGet(t, client, key, commitTS)
		if err != nil {
			lost = append(lost, fmt.Sprintf("%q (read error: %v)", key, err))
			continue
		}
		if !resp.GetFound() {
			lost = append(lost, fmt.Sprintf("%q", key))
			continue
		}
		if !bytes.Equal(resp.GetValue(), want) {
			wrong = append(wrong, fmt.Sprintf("%q: got %q want %q", key, resp.GetValue(), want))
		}
	}
	return lost, wrong
}

func crashGet(t *testing.T, client pbv2.KVServiceClient, key []byte, ts uint64) (*pbv2.GetResponse, error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := client.Get(ctx, &pbv2.GetRequest{Key: key, Ts: ts})
		cancel()
		if err == nil {
			if msg := resp.GetError(); msg != "" {
				lastErr = fmt.Errorf("%s", msg)
			} else {
				return resp, nil
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// crashOutput is a concurrency-safe writer for the child's stdout and stderr.
// os/exec copies those streams from separate goroutines, while the parent polls
// the accumulated text while waiting for READY.
type crashOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *crashOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *crashOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

type crashChild struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	out     *crashOutput
	waitCh  chan error
	waited  bool
	waitErr error
}

func crashStartChild(t *testing.T, mode crashMode, dataDir, addr string, n int) *crashChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashConsistencyChild$")
	out := &crashOutput{}
	cmd.Stdout = out
	cmd.Stderr = out
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("%s: create crash child stdin: %v", mode.name, err)
	}
	cmd.Env = append(os.Environ(),
		crashChildEnv+"=1",
		crashDataDirEnv+"="+dataDir,
		crashAddrEnv+"="+addr,
		crashNEnv+"="+strconv.Itoa(n),
		crashSyncEnv+"="+crashBool(mode.forceSync),
	)
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		t.Fatalf("%s: start crash child: %v", mode.name, err)
	}
	child := &crashChild{
		cmd:    cmd,
		stdin:  stdin,
		out:    out,
		waitCh: make(chan error, 1),
	}
	go func() { child.waitCh <- cmd.Wait() }()
	return child
}

func crashBool(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// awaitReady waits for the child's READY line. It fails fast if the child
// exits early and fails with a deadline otherwise, so a broken child never
// leaves the parent waiting forever.
func (c *crashChild) awaitReady(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-c.waitCh:
			c.waited = true
			c.waitErr = err
			t.Fatalf("crash child exited before READY (wait error: %v)\nchild output:\n%s",
				err, c.out.String())
		case <-ticker.C:
			if crashReadyReported(c.out.String(), n) {
				return
			}
		case <-timer.C:
			t.Fatalf("timed out after %s waiting for crash child READY\nchild output:\n%s",
				timeout, c.out.String())
		}
	}
}

func crashReadyReported(out string, n int) bool {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || fields[0] != "READY" {
			continue
		}
		got, err := strconv.Atoi(fields[1])
		if err == nil && got == n {
			return true
		}
	}
	return false
}

// killAndWait sends SIGKILL (os.Process.Kill) and reaps the child. It is only
// called after READY, so the child has already acknowledged all writes.
func (c *crashChild) killAndWait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	if c.waited {
		return c.waitErr
	}
	if err := c.cmd.Process.Kill(); err != nil {
		t.Logf("kill crash child pid %d: %v", c.cmd.Process.Pid, err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-c.waitCh:
		c.waited = true
		c.waitErr = err
		return err
	case <-timer.C:
		t.Fatalf("crash child pid %d did not exit within %s after SIGKILL", c.cmd.Process.Pid, timeout)
		return nil
	}
}

// cleanup guarantees that a failed test does not leave the child process
// behind. It is a no-op after killAndWait has reaped the child.
func (c *crashChild) cleanup() {
	if !c.waited && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	if !c.waited {
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-c.waitCh:
			c.waited = true
		case <-timer.C:
		}
		timer.Stop()
	}
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
}

func crashKey(mode string, i int) []byte {
	return []byte(fmt.Sprintf("crash-consistency/%s/key-%05d", mode, i))
}

func crashValue(i int) []byte {
	return []byte(fmt.Sprintf("acknowledged-%05d", i))
}

func crashTS(i int) (startTS, commitTS uint64) {
	startTS = crashTSBase + uint64(i)*2
	return startTS, startTS + 1
}

func crashFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve crash test address: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release crash test address: %v", err)
	}
	return addr
}

func crashLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
