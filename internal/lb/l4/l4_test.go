package l4

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	lbmodel "github.com/abhrajyoti-01/rift/internal/lb/model"
)

// echoServer is a real TCP server that echoes bytes back.
func echoServer(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String(), func() { cancel(); wg.Wait() }
}

func snapshotFor(addrs ...string) *lbmodel.Snapshot {
	pool := &lbmodel.Pool{ID: "p", Picker: "round_robin"}
	for i, a := range addrs {
		b := &lbmodel.Backend{ID: fmt.Sprintf("b%d", i), Addr: a, Weight: 1}
		b.Health.Store(true)
		pool.Backends = append(pool.Backends, b)
	}
	return &lbmodel.Snapshot{
		Version:   1,
		Listeners: []lbmodel.ListenerSpec{{ID: "l", Proto: "tcp", Bind: "", Pool: "p"}},
		Pools:     map[lbmodel.PoolID]*lbmodel.Pool{"p": pool},
	}
}

// startProxy runs a Forwarder in front of the given backends and returns the
// proxy's address plus a stop function.
func startProxy(t *testing.T, cfg Config, addrs ...string) (string, func()) {
	t.Helper()
	snap := snapshotFor(addrs...)
	fwd := New(cfg, func() *lbmodel.Snapshot { return snap })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		fwd.Serve(ctx, ln)
	}()
	return ln.Addr().String(), func() {
		cancel()
		<-done
	}
}

func TestL4ByteEquivalence(t *testing.T) {
	// The core correctness property: every byte survives the proxy
	// unchanged, in both copy modes.
	for _, splice := range []bool{false, true} {
		t.Run(fmt.Sprintf("splice=%v", splice), func(t *testing.T) {
			backend, stopBackend := echoServer(t)
			defer stopBackend()

			proxyAddr, stopProxy := startProxy(t, Config{Splice: splice, MaxConns: 100}, backend)
			defer stopProxy()

			conn, err := net.Dial("tcp", proxyAddr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()

			payload := make([]byte, 1<<16) // 64 KiB
			rand.Read(payload)
			payload[0] = 0x00 // ensure a zero byte traverses
			payload[1] = 0xff

			go func() {
				conn.Write(payload)
				if cw, ok := conn.(interface{ CloseWrite() error }); ok {
					cw.CloseWrite()
				}
			}()

			got, err := io.ReadAll(conn)
			if err != nil && err != io.EOF {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("byte mismatch: got %d bytes, want %d", len(got), len(payload))
			}
		})
	}
}

func TestL4HalfCloseDrain(t *testing.T) {
	// Client finishes its request (half-close) and must still receive the
	// full response. Skipping the drain truncates exactly here.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	response := bytes.Repeat([]byte("R"), 128*1024)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(io.Discard, c) // read until client EOF
		c.Write(response)      // then answer
	}()

	proxyAddr, stopProxy := startProxy(t, Config{IdleTimeout: 5 * time.Second}, ln.Addr().String())
	defer stopProxy()

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("request"))
	conn.(interface{ CloseWrite() error }).CloseWrite()

	got, err := io.ReadAll(conn)
	if err != nil && err != io.EOF {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(response) {
		t.Fatalf("got %d response bytes, want %d (half-close drain failed)", len(got), len(response))
	}
}

func TestL4AdmissionRefusesOverMax(t *testing.T) {
	backend, stopBackend := echoServer(t)
	defer stopBackend()

	// MaxConns 1: hold one connection open, the second must be closed by
	// the proxy rather than queued.
	proxyAddr, stopProxy := startProxy(t, Config{MaxConns: 1, DialTimeout: 2 * time.Second}, backend)
	defer stopProxy()

	c1, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c1.Write([]byte("x"))
	time.Sleep(150 * time.Millisecond) // let admission register

	c2, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	// The proxy closes refused connections; a read should hit EOF quickly.
	c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := c2.Read(buf); err == nil {
		t.Fatal("second connection should have been refused, got data")
	}
}

func TestL4FailoverToHealthyBackend(t *testing.T) {
	// First backend refuses connections; the proxy must repick and succeed
	// against the second.
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := deadLn.Addr().String()
	deadLn.Close() // nothing listening now

	backend, stopBackend := echoServer(t)
	defer stopBackend()

	proxyAddr, stopProxy := startProxy(t, Config{MaxRetries: 2, DialTimeout: 500 * time.Millisecond}, deadAddr, backend)
	defer stopProxy()

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte("hello"))
	got := make([]byte, 5)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read after failover: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestL4NoBackendClosesConnection(t *testing.T) {
	snap := &lbmodel.Snapshot{
		Version: 1,
		Pools:   map[lbmodel.PoolID]*lbmodel.Pool{"p": {ID: "p", Backends: nil}},
	}
	fwd := New(Config{}, func() *lbmodel.Snapshot { return snap })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); fwd.Serve(ctx, ln) }()
	defer func() { cancel(); <-done }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("connection with no backends must be closed, not held open")
	}
}

func TestL4ServeStopsOnContextCancel(t *testing.T) {
	backend, stopBackend := echoServer(t)
	defer stopBackend()
	snap := snapshotFor(backend)
	fwd := New(Config{}, func() *lbmodel.Snapshot { return snap })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fwd.Serve(ctx, ln) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v on clean cancel, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

func TestL4ConnectionAccounting(t *testing.T) {
	backend, stopBackend := echoServer(t)
	defer stopBackend()
	snap := snapshotFor(backend)
	fwd := New(Config{}, func() *lbmodel.Snapshot { return snap })
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); fwd.Serve(ctx, ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	io.ReadFull(conn, buf)
	conn.Close()

	// Wait for teardown.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fwd.Metrics().ActiveConns.Load() != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if n := fwd.Metrics().ActiveConns.Load(); n != 0 {
		t.Errorf("ActiveConns = %d after close, want 0 (leak)", n)
	}
	if fwd.Metrics().Accepted.Load() == 0 {
		t.Error("Accepted counter not incremented")
	}
	cancel()
	<-done
}

func BenchmarkL4Forward64K(b *testing.B) {
	backend, stop := echoServerB(b)
	defer stop()
	snap := snapshotFor(backend)
	fwd := New(Config{Splice: false, MaxConns: 10}, func() *lbmodel.Snapshot { return snap })
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fwd.Serve(ctx, ln)

	payload := make([]byte, 64<<10)
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()

	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := conn.Write(payload); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(conn, payload); err != nil {
			b.Fatal(err)
		}
	}
}

func echoServerB(b *testing.B) (string, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}
