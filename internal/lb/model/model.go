package lbmodel

import "sync/atomic"

// PoolID identifies a backend pool within a Snapshot.
type PoolID string

// String renders the pool id (metric labels use it; pools are a closed set
// from config, so cardinality is bounded).
func (p PoolID) String() string { return string(p) }

// ListenerID identifies a listener within a Snapshot.
type ListenerID string

// String renders the listener id.
func (l ListenerID) String() string { return string(l) }

// Backend is the unit of health and load accounting. Immutable identity
// (ID, Addr, Weight); mutable state is atomic only. Backends are shared
// across Snapshots when unchanged (copy-on-write on reload).
type Backend struct {
	ID     string
	Addr   string // host:port, validated at config time
	Weight int    // ≥ 1

	Conns  atomic.Int64 // in-flight; ± on connect/close
	Health atomic.Bool  // written only by the health system
}

// Pool is an ordered, immutable-per-snapshot backend set. Picker names the
// algorithm this pool uses (the closed set in lb/picker).
type Pool struct {
	ID       PoolID
	Picker   string
	Backends []*Backend
}

// TLSConfig terminates TLS on a listener when non-nil. Key material is a
// file path, never inline.
type TLSConfig struct {
	CertFile   string
	KeyFile    string
	MinVersion string // "1.2" | "1.3"
}

// ListenerSpec binds one data-plane socket to one pool.
type ListenerSpec struct {
	ID       ListenerID
	Proto    string // "tcp" | "udp" | "http"
	Bind     string
	Pool     PoolID
	MaxConns int
	TLS      *TLSConfig // nil = cleartext
}

// Snapshot is the immutable configuration the data plane reads through one
// atomic.Pointer owned by lb/control. Version increments per accepted
// reload.
type Snapshot struct {
	Version   uint64
	Listeners []ListenerSpec
	Pools     map[PoolID]*Pool
}
