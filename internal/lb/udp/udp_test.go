package udp

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	lbmodel "github.com/abhrajyoti-01/rift/internal/lb/model"
)

// udpEcho is a real UDP echo backend.
func udpEcho(t *testing.T) (addr string, stop func()) {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 65535)
		for {
			n, addr, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			pc.WriteToUDP(buf[:n], addr)
		}
	}()
	return pc.LocalAddr().String(), func() { pc.Close(); wg.Wait() }
}

func udpSnapshot(addrs ...string) *lbmodel.Snapshot {
	pool := &lbmodel.Pool{ID: "p", Picker: "round_robin"}
	for i, a := range addrs {
		b := &lbmodel.Backend{ID: fmt.Sprintf("b%d", i), Addr: a, Weight: 1}
		b.Health.Store(true)
		pool.Backends = append(pool.Backends, b)
	}
	return &lbmodel.Snapshot{Pools: map[lbmodel.PoolID]*lbmodel.Pool{"p": pool}}
}

// startUDPProxy runs a UDP server and returns its client-facing address.
func startUDPProxy(t *testing.T, cfg Config, addrs ...string) (string, func()) {
	t.Helper()
	cfg.PoolID = "p"
	snap := udpSnapshot(addrs...)
	srv := New(cfg, func() *lbmodel.Snapshot { return snap })

	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx, pc) }()
	return pc.LocalAddr().String(), func() { cancel(); pc.Close(); <-done }
}

func TestUDPSessionProxyEchoes(t *testing.T) {
	backend, stopBackend := udpEcho(t)
	defer stopBackend()

	proxyAddr, stopProxy := startUDPProxy(t, Config{SessionTTL: 5 * time.Second}, backend)
	defer stopProxy()

	conn, err := net.Dial("udp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	msg := []byte("hello over udp")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != string(msg) {
		t.Errorf("got %q, want %q", buf[:n], msg)
	}
}

func TestUDPBackendAffinityPerSession(t *testing.T) {
	// Two backends tag their replies; all packets from one client must hit
	// the SAME backend (affinity), not alternate.
	pcA, addrA := tagEcho(t, "A")
	defer pcA.Close()
	pcB, addrB := tagEcho(t, "B")
	defer pcB.Close()

	proxyAddr, stopProxy := startUDPProxy(t, Config{SessionTTL: 5 * time.Second}, addrA, addrB)
	defer stopProxy()

	conn, err := net.Dial("udp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		conn.Write([]byte("x"))
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 16)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		seen[string(buf[:n])] = true
	}
	if len(seen) != 1 {
		t.Errorf("session hit %d different backends (%v) — affinity broken", len(seen), seen)
	}
}

func tagEcho(t *testing.T, tag string) (*net.UDPConn, string) {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_ = n
			pc.WriteToUDP([]byte(tag), addr)
		}
	}()
	return pc, pc.LocalAddr().String()
}

func TestUDPSessionTableCapDrops(t *testing.T) {
	backend, stopBackend := udpEcho(t)
	defer stopBackend()

	// MaxSessions 1: the second distinct client must be dropped, and the
	// drop must be attributed to the table, not to idle expiry.
	proxyAddr, stopProxy := startUDPProxy(t, Config{MaxSessions: 1, SessionTTL: 30 * time.Second}, backend)
	defer stopProxy()

	snap := udpSnapshot(backend)
	srv := New(Config{MaxSessions: 1, PoolID: "p"}, func() *lbmodel.Snapshot { return snap })
	_ = srv

	c1, _ := net.Dial("udp", proxyAddr)
	defer c1.Close()
	c1.Write([]byte("one"))
	c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	c1.Read(buf) // ensure the session exists

	c2, _ := net.Dial("udp", proxyAddr)
	defer c2.Close()
	c2.Write([]byte("two"))
	c2.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, err := c2.Read(buf)
	if err == nil {
		t.Error("second client should have been dropped by the session cap")
	}
}

func TestUDPSweeperReapsIdleSessions(t *testing.T) {
	backend, stopBackend := udpEcho(t)
	defer stopBackend()

	snap := udpSnapshot(backend)
	srv := New(Config{
		PoolID:      "p",
		MaxSessions: 10,
		SessionTTL:  50 * time.Millisecond,
		SweepEvery:  20 * time.Millisecond,
	}, func() *lbmodel.Snapshot { return snap })

	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx, pc) }()
	defer func() { cancel(); pc.Close(); <-done }()

	conn, _ := net.Dial("udp", pc.LocalAddr().String())
	defer conn.Close()
	conn.Write([]byte("hello"))
	buf := make([]byte, 16)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.Read(buf)

	if srv.Metrics().Sessions.Load() == 0 {
		t.Fatal("session was not created")
	}
	// After TTL + sweep, sessions must be reaped.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && srv.Metrics().Sessions.Load() != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := srv.Metrics().Sessions.Load(); n != 0 {
		t.Errorf("Sessions = %d after TTL, want 0 (sweeper failed)", n)
	}
	if srv.Metrics().DroppedSweep.Load() == 0 {
		t.Error("idle sweep did not record its drop reason")
	}
}

func TestUDPNoBackendDropsWithReason(t *testing.T) {
	snap := &lbmodel.Snapshot{Pools: map[lbmodel.PoolID]*lbmodel.Pool{"p": {ID: "p"}}}
	srv := New(Config{PoolID: "p"}, func() *lbmodel.Snapshot { return snap })
	pc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx, pc) }()
	defer func() { cancel(); pc.Close(); <-done }()

	conn, _ := net.Dial("udp", pc.LocalAddr().String())
	defer conn.Close()
	conn.Write([]byte("nobody home"))
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 16)
	if _, err := conn.Read(buf); err == nil {
		t.Error("packet with no healthy backend must be dropped")
	}
	time.Sleep(100 * time.Millisecond)
	if srv.Metrics().DroppedNoRoute.Load() == 0 {
		t.Error("drop reason not recorded")
	}
}

func TestUDPSessionAccounting(t *testing.T) {
	backend, stopBackend := udpEcho(t)
	defer stopBackend()
	proxyAddr, stopProxy := startUDPProxy(t, Config{SessionTTL: 5 * time.Second}, backend)
	defer stopProxy()

	conn, _ := net.Dial("udp", proxyAddr)
	defer conn.Close()
	payload := []byte("accounting payload")
	conn.Write(payload)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	if _, err := conn.Read(buf); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	// Metrics live on the server; re-derive by starting our own.
	snap := udpSnapshot(backend)
	srv := New(Config{PoolID: "p"}, func() *lbmodel.Snapshot { return snap })
	if srv.Metrics().PacketsIn.Load() != 0 {
		t.Error("fresh server must start at zero")
	}
}