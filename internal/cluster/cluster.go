package cluster

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// GossipMessage represents a cluster gossip message exchanged between nodes.
type GossipMessage struct {
	Version    string            `json:"version"`               // protocol version
	NodeID     string            `json:"node_id"`               // sender identity
	Addr       string            `json:"addr,omitempty"`        // sender's gossip listen address
	Term       uint64            `json:"term"`                  // monotonic term for last-writer-wins
	ConfigHash string            `json:"config_hash,omitempty"` // sha256 of the sender's config file
	Health     map[string]Health `json:"health"`                // backend health state
}

// Health represents the health state of a single backend.
type Health struct {
	Healthy  bool   `json:"healthy"`
	Failures int    `json:"failures"`
	Term     uint64 `json:"term"`
}

// Peer represents a cluster peer node.
type Peer struct {
	ID   string
	Addr string
}

// Node is a cluster node that participates in gossip-based health sharing.
// Each node runs a UDP listener on the gossip port and exchanges health state
// with peers at the configured interval.
//
// Design: problem — each replica probes backends independently causing flapping
// backends to produce inconsistent routing; options — (a) external consensus
// (etcd/Consul), (b) gossip protocol, (c) leader-based sync; choice: (b) —
// gossip is sufficient for health hints (not source of truth), leaderless,
// no coordination needed, partition-tolerant.
// Failure: partitioned node degrades to local health only (never 503s healthy
// traffic due to split-brain); higher term always wins.
// Observability: haribon_cluster_peers, haribon_cluster_term, log level:warn
// on config hash mismatch.
//
// Field notes:
//
//   - own holds only this node's own findings, and is the only thing gossiped.
//     Relaying a peer's report would keep it alive on every other node after
//     that peer went away, which is what remoteAt and staleAfter exist to stop.
//   - origin records which node produced the current Health entry; "" means this
//     node. Without it, expiring a peer's finding could not tell whether the
//     entry it is about to forget was the peer's or a local probe result.
//   - staleAfter is how long a peer's finding stays authoritative without being
//     re-asserted. Without an expiry, a replica that dies mid-incident leaves
//     its "this backend is unhealthy" verdict in every peer's map forever, and
//     the backend stays out of rotation cluster-wide with nothing to clear it.
//   - seen records when each peer was last heard from, keyed by node ID. Gossip
//     is the only trustworthy liveness signal here: a UDP send to a peer address
//     succeeds whether or not anything is listening, so a send failure says
//     nothing about the peer, and a peer that dies keeps looking alive forever.
type Node struct {
	NodeID     string
	Addr       string // gossip address
	GossipAddr string // alias for Addr
	Peers      []Peer
	GossipSec  int
	Health     map[string]Health // the current finding per backend, local or remote
	own        map[string]Health // this node's own findings — the only thing it gossips
	origin     map[string]string // which node produced Health[backend]; "" means this node
	remoteAt   map[string]time.Time
	healthMu   sync.RWMutex
	staleAfter time.Duration
	term       uint64
	termMu     sync.RWMutex
	seen       map[string]time.Time
	seenMu     sync.Mutex
	configHash atomic.Value // string; sha256 of this node's config file
	mismatches atomic.Int64 // count of peer messages with a differing config hash
	listener   *net.UDPConn
	stopCh     chan struct{}
	stopOnce   sync.Once
	wg         sync.WaitGroup
}

// NewNode creates a cluster node.
// nodeID identifies this node in the cluster (typically ${HOSTNAME}).
// addr is the gossip UDP address (host:port).
// peers is the list of peer gossip addresses.
// gossipSec is the gossip interval in seconds.
func NewNode(nodeID, addr string, peers []string, gossipSec int) *Node {
	if gossipSec <= 0 {
		gossipSec = 5
	}
	stale := time.Duration(gossipSec*3) * time.Second
	if stale < 15*time.Second {
		stale = 15 * time.Second
	}
	n := &Node{
		NodeID:     nodeID,
		Addr:       addr,
		GossipAddr: addr,
		GossipSec:  gossipSec,
		Health:     make(map[string]Health),
		own:        make(map[string]Health),
		origin:     make(map[string]string),
		remoteAt:   make(map[string]time.Time),
		staleAfter: stale,
		seen:       make(map[string]time.Time),
		stopCh:     make(chan struct{}),
	}
	for _, p := range peers {
		n.Peers = append(n.Peers, Peer{ID: p, Addr: p})
	}
	return n
}

// SetHealth updates the local health state for a backend.
//
// The entry is stamped with a freshly incremented term, not the current one.
// Every local change must be strictly newer than anything a peer has seen,
// otherwise a backend marked unhealthy on a freshly-started node (term 0) would
// carry term 0 and every peer holding "no entry yet" (also term 0) would reject
// it as same-term — the change would never propagate at all.
//
// The finding is also recorded as this node's own, which is what gets gossiped.
// Only first-hand findings are sent: relaying a peer's report onward would keep
// it alive after that peer died, since every relay would refresh it.
func (n *Node) SetHealth(backend string, healthy bool) {
	term := n.IncrementTerm()

	n.healthMu.Lock()
	defer n.healthMu.Unlock()
	h := n.Health[backend]
	h.Healthy = healthy
	if !healthy {
		h.Failures++
	} else {
		h.Failures = 0
	}
	h.Term = term
	n.Health[backend] = h
	n.own[backend] = h
	// What this node just observed outranks whatever a peer last said, so the
	// entry is no longer a peer's and is no longer subject to expiry.
	n.origin[backend] = ""
	delete(n.remoteAt, backend)
}

// GetHealth returns the health state for a backend, falling back to
// localHealthy when nothing is known about it.
//
// A peer's finding that has not been re-asserted within staleAfter is ignored,
// so a replica that stopped reporting cannot keep suppressing a backend.
func (n *Node) GetHealth(backend string, localHealthy bool) bool {
	n.healthMu.RLock()
	defer n.healthMu.RUnlock()

	h, known := n.Health[backend]
	if !known {
		return localHealthy
	}
	if n.isStale(backend) {
		return localHealthy
	}
	return h.Healthy
}

// GetMergedHealth returns the health view merged from this node and its peers,
// excluding peer findings that have gone stale.
func (n *Node) GetMergedHealth() map[string]Health {
	n.healthMu.RLock()
	defer n.healthMu.RUnlock()
	out := make(map[string]Health, len(n.Health))
	for k, v := range n.Health {
		if n.isStale(k) {
			continue
		}
		out[k] = v
	}
	return out
}

// ownHealth returns this node's first-hand findings, for gossiping.
func (n *Node) ownHealth() map[string]Health {
	n.healthMu.RLock()
	defer n.healthMu.RUnlock()
	out := make(map[string]Health, len(n.own))
	for k, v := range n.own {
		out[k] = v
	}
	return out
}

// isStale reports whether the current entry for backend came from a peer that
// has stopped asserting it. Callers must hold healthMu (read or write).
func (n *Node) isStale(backend string) bool {
	seen, remote := n.remoteAt[backend]
	return remote && time.Since(seen) > n.staleAfter
}

// expireRemote drops peer findings that have gone stale, so a replica that went
// away stops influencing routing.
//
// Expiring a backend does not leave it unknown: this node's own finding for it,
// if there is one, is put back in place. Otherwise the local view would be
// thrown away along with the departed peer's, and a backend this node probes
// happily would go back to being reported only by the fallback.
func (n *Node) expireRemote() {
	n.healthMu.Lock()
	defer n.healthMu.Unlock()
	for backend := range n.remoteAt {
		if !n.isStale(backend) {
			continue
		}
		delete(n.remoteAt, backend)
		delete(n.origin, backend)
		if own, ok := n.own[backend]; ok {
			n.Health[backend] = own
		} else {
			delete(n.Health, backend)
		}
	}
}

// GetTerm returns the current monotonic term.
func (n *Node) GetTerm() uint64 {
	n.termMu.RLock()
	defer n.termMu.RUnlock()
	return n.term
}

// IncrementTerm bumps the term and returns the new value.
func (n *Node) IncrementTerm() uint64 {
	n.termMu.Lock()
	defer n.termMu.Unlock()
	n.term++
	return n.term
}

// Start begins the gossip listener and periodic gossip rounds.
//
// It returns an error when the gossip socket cannot be bound. Silently
// continuing would leave the node running with no listener at all: it would
// still send gossip but never receive any, so peers would appear alive while
// their health state never arrived.
func (n *Node) Start() error {
	addr, err := net.ResolveUDPAddr("udp", n.Addr)
	if err != nil {
		return fmt.Errorf("gossip address %s: %w", n.Addr, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("gossip listener on %s: %w", n.Addr, err)
	}
	n.listener = conn
	log.Printf("cluster: gossip listening on %s, %d peers", n.Addr, len(n.Peers))

	n.wg.Add(2)
	go n.listenLoop()
	go n.gossipLoop()
	return nil
}

// Stop stops the gossip listener and rounds. It is safe to call more than once.
func (n *Node) Stop() {
	n.stopOnce.Do(func() {
		close(n.stopCh)
		if n.listener != nil {
			n.listener.Close()
		}
		n.wg.Wait()
	})
}

// SetConfigHash records the sha256 of this node's config file so peers can
// detect drift. Empty disables the check on this node.
func (n *Node) SetConfigHash(h string) { n.configHash.Store(h) }

// ConfigHash returns the recorded config hash ("" when unset).
func (n *Node) ConfigHash() string {
	v, _ := n.configHash.Load().(string)
	return v
}

// ConfigMismatches returns how many peer messages carried a config hash that
// differs from this node's. Exposed as haribon_config_hash_mismatch_total.
func (n *Node) ConfigMismatches() int64 { return n.mismatches.Load() }

// LivePeerCount returns how many peers have sent a gossip message recently.
//
// This counts replicas actually heard from, not configured peer addresses: a
// configured peer that has never been heard from is not evidence of anything,
// and one that went away must stop being counted.
func (n *Node) LivePeerCount() int {
	n.seenMu.Lock()
	defer n.seenMu.Unlock()
	c := 0
	for _, at := range n.seen {
		if time.Since(at) <= n.staleAfter {
			c++
		}
	}
	return c
}

// markPeerSeen records that a gossip message arrived from a peer.
func (n *Node) markPeerSeen(nodeID string) {
	if nodeID == "" {
		return
	}
	n.seenMu.Lock()
	n.seen[nodeID] = time.Now()
	n.seenMu.Unlock()
}

func (n *Node) listenLoop() {
	defer n.wg.Done()
	buf := make([]byte, 4096)
	for {
		n.listener.SetReadDeadline(time.Now().Add(1 * time.Second))

		// Use the byte count ReadFrom returns. Unmarshalling the whole buffer
		// instead would include the trailing NUL bytes and json.Unmarshal
		// rejects trailing data — every message would be silently dropped.
		nRead, _, err := n.listener.ReadFrom(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}
		var msg GossipMessage
		if err := json.Unmarshal(buf[:nRead], &msg); err != nil {
			continue
		}
		n.MergeGossip(msg)
	}
}

func (n *Node) gossipLoop() {
	defer n.wg.Done()
	ticker := time.NewTicker(time.Duration(n.GossipSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-n.stopCh:
			return
		case <-ticker.C:
			// Drop findings from replicas that have gone quiet before sending, so
			// a departed replica stops being counted in the metrics too.
			n.expireRemote()
			n.sendGossip()
		}
	}
}

func (n *Node) sendGossip() {
	n.IncrementTerm()
	msg := GossipMessage{
		Version:    "1",
		NodeID:     n.NodeID,
		Addr:       n.Addr,
		Term:       n.GetTerm(),
		ConfigHash: n.ConfigHash(),
		// Only this node's own findings. Forwarding a peer's finding would keep
		// it alive on every other node after the peer that made it went away,
		// which is exactly what the expiry is there to prevent. The upshot is
		// that peers must be listed as a full mesh: every node talks to every
		// other node directly, and findings do not travel two hops.
		Health: n.ownHealth(),
	}
	data, _ := json.Marshal(msg)

	// A failed send says nothing about the peer, so every configured peer is
	// retried on every round rather than being written off. UDP has no
	// connection to refuse: a dial to a dead peer's address succeeds, and a
	// peer whose name momentarily fails to resolve is still a peer.
	for _, p := range n.Peers {
		conn, err := net.Dial("udp", p.Addr)
		if err != nil {
			log.Printf("cluster: cannot send to peer %s: %v", p.Addr, err)
			continue
		}
		_, _ = conn.Write(data)
		conn.Close()
	}
}

// MergeGossip merges a gossip message into local state.
// Higher term wins over local state.
func (n *Node) MergeGossip(msg GossipMessage) {
	// Ignore our own datagrams outright. The previous guard also required
	// msg.Term <= local term, so a node that received its own message back with
	// a higher term would adopt that term and the health map inside it.
	if msg.NodeID == n.NodeID {
		return
	}
	// Hearing from a peer is the only liveness signal available, so record it
	// before any of the filtering below can return early.
	n.markPeerSeen(msg.NodeID)
	// Config drift is counted before the term check so a peer running a
	// different config is still reported even when its term is stale.
	if msg.ConfigHash != "" {
		if mine := n.ConfigHash(); mine != "" && mine != msg.ConfigHash {
			if n.mismatches.Add(1)%30 == 1 { // log the first, then every 30th
				log.Printf("cluster: config hash mismatch with %s (local %s, peer %s) — "+
					"replicas may route to different backends",
					msg.NodeID, shortHash(mine), shortHash(msg.ConfigHash))
			}
		}
	}
	if msg.Term < n.GetTerm() {
		return
	}
	n.termMu.Lock()
	if msg.Term > n.term {
		n.term = msg.Term
	}
	n.termMu.Unlock()

	now := time.Now()
	n.healthMu.Lock()
	defer n.healthMu.Unlock()
	for backend, h := range msg.Health {
		existing, known := n.Health[backend]
		switch {
		// First contact wins outright. A backend we have never heard about has a
		// zero-value entry with Term 0, and `h.Term > 0` is false when the sender
		// is also at term 0 — the very first health report would be discarded.
		case !known, h.Term > existing.Term:
			n.Health[backend] = h
			n.origin[backend] = msg.NodeID
			n.remoteAt[backend] = now
		// The sender is still asserting this backend but has nothing newer to
		// say. If the entry we hold is theirs, restamp it so it does not expire
		// while they are alive and still reporting.
		case n.origin[backend] == msg.NodeID:
			n.remoteAt[backend] = now
		}
	}
}

// Discover resolves peer addresses from DNS or a static file.
func Discover(peers []string) ([]Peer, error) {
	var resolved []Peer
	for _, p := range peers {
		addrs, err := net.ResolveUDPAddr("udp", p)
		if err != nil {
			return nil, fmt.Errorf("resolve peer %q: %w", p, err)
		}
		resolved = append(resolved, Peer{ID: p, Addr: addrs.String()})
	}
	return resolved, nil
}

// shortHash truncates a hash for log output without panicking on short input.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
