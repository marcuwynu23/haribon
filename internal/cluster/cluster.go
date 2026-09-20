package cluster

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// GossipMessage represents a cluster gossip message exchanged between nodes.
type GossipMessage struct {
	Version string            `json:"version"` // protocol version
	NodeID  string            `json:"node_id"`
	Term    uint64            `json:"term"`   // monotonic term for last-writer-wins
	Health  map[string]Health `json:"health"` // backend health state
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
	Live bool
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
type Node struct {
	NodeID     string
	Addr       string // gossip address
	GossipAddr string // alias for Addr
	Peers      []Peer
	GossipSec  int
	Health     map[string]Health // local backend health
	healthMu   sync.RWMutex
	term       uint64
	termMu     sync.RWMutex
	listener   *net.UDPConn
	stopCh     chan struct{}
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
	n := &Node{
		NodeID:     nodeID,
		Addr:       addr,
		GossipAddr: addr,
		GossipSec:  gossipSec,
		Health:     make(map[string]Health),
		stopCh:     make(chan struct{}),
	}
	for _, p := range peers {
		n.Peers = append(n.Peers, Peer{ID: p, Addr: p, Live: true})
	}
	return n
}

// SetHealth updates the local health state for a backend.
func (n *Node) SetHealth(backend string, healthy bool) {
	n.healthMu.Lock()
	defer n.healthMu.Unlock()
	h := n.Health[backend]
	h.Healthy = healthy
	if !healthy {
		h.Failures++
	} else {
		h.Failures = 0
	}
	h.Term = n.GetTerm()
	n.Health[backend] = h
}

// GetHealth returns the merged health state for a backend.
// If the local health map has a recorded state for this backend,
// that state takes precedence (after term-based merging).
// Otherwise, falls back to localHealthy.
func (n *Node) GetHealth(backend string, localHealthy bool) bool {
	n.healthMu.RLock()
	defer n.healthMu.RUnlock()

	h, known := n.Health[backend]
	if known {
		return h.Healthy
	}
	return localHealthy
}

// GetMergedHealth returns the health map merged from all peers.
func (n *Node) GetMergedHealth() map[string]Health {
	n.healthMu.RLock()
	defer n.healthMu.RUnlock()
	out := make(map[string]Health, len(n.Health))
	for k, v := range n.Health {
		out[k] = v
	}
	return out
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
func (n *Node) Start() {
	ln, err := net.ListenPacket("udp", n.Addr)
	if err != nil {
		log.Printf("cluster: gossip listener failed on %s: %v", n.Addr, err)
		return
	}
	n.listener = ln.(*net.UDPConn)
	log.Printf("cluster: gossip listening on %s, %d peers", n.Addr, len(n.Peers))

	n.wg.Add(2)
	go n.listenLoop()
	go n.gossipLoop()
}

// Stop stops the gossip listener and rounds.
func (n *Node) Stop() {
	if n.listener != nil {
		n.listener.Close()
	}
	close(n.stopCh)
	n.wg.Wait()
}

func (n *Node) listenLoop() {
	defer n.wg.Done()
	buf := make([]byte, 4096)
	for {
		n.listener.SetReadDeadline(time.Now().Add(1 * time.Second))
		_, _, err := n.listener.ReadFrom(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}
		var msg GossipMessage
		if err := json.Unmarshal(buf[:len(buf)], &msg); err != nil {
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
			n.sendGossip()
		}
	}
}

func (n *Node) sendGossip() {
	n.IncrementTerm()
	msg := GossipMessage{
		Version: "1",
		NodeID:  n.NodeID,
		Term:    n.GetTerm(),
		Health:  n.GetMergedHealth(),
	}
	data, _ := json.Marshal(msg)

	for _, peer := range n.Peers {
		if !peer.Live {
			continue
		}
		conn, err := net.Dial("udp", peer.Addr)
		if err != nil {
			peer.Live = false
			continue
		}
		_, _ = conn.Write(data)
		conn.Close()
	}
}

// MergeGossip merges a gossip message into local state.
// Higher term wins over local state.
func (n *Node) MergeGossip(msg GossipMessage) {
	if msg.Term <= n.GetTerm() && msg.NodeID == n.NodeID {
		return
	}
	if msg.Term < n.GetTerm() {
		return
	}
	n.termMu.Lock()
	if msg.Term > n.term {
		n.term = msg.Term
	}
	n.termMu.Unlock()

	n.healthMu.Lock()
	for backend, h := range msg.Health {
		existing := n.Health[backend]
		if h.Term > existing.Term {
			n.Health[backend] = h
		}
	}
	n.healthMu.Unlock()
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

// ConfigHash represents a config hash for gossip consistency check.
type ConfigHash struct {
	NodeID string `json:"node_id"`
	Hash   string `json:"hash"`
	Term   uint64 `json:"term"`
}

// ClusterConfig holds the cluster configuration.
type ClusterConfig struct {
	Enabled    bool     `yaml:"enabled"`
	NodeID     string   `yaml:"node_id"`
	Peers      []string `yaml:"peers"`
	GossipSec  int      `yaml:"gossip_interval_sec"`
	GossipAddr string   `yaml:"gossip_addr"`
}

// DefaultClusterConfig returns a cluster config with safe defaults.
func DefaultClusterConfig() ClusterConfig {
	return ClusterConfig{
		Enabled:    false,
		GossipSec:  5,
		GossipAddr: "0.0.0.0:7946",
	}
}
