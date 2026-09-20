package cluster_test

// cluster_wire_test.go — tests for the gossip transport itself.
//
// The original suite only exercised Node's in-memory maps by calling
// MergeGossip directly, so every defect in the UDP path (buffer handling,
// peer liveness, Start/Stop lifecycle) was invisible. These tests cover the
// wire, plus the term semantics that decide whether a change propagates at all.

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/marcuwynu23/haribon/internal/cluster"
)

// freeUDPPort returns a port that was free a moment ago. There is an inherent
// race between closing the probe socket and the node binding it; using a
// kernel-assigned port keeps the window tiny compared with a fixed port number
// that a previous test run or an unrelated process may already hold.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe udp port: %v", err)
	}
	defer func() { _ = c.Close() }()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// ---------- Term semantics ----------

// TestNode_SetHealthAdvancesTerm is the regression test for the bug that made
// gossip inert. SetHealth used to stamp the entry with the *current* term.
// On a freshly started node that term is 0, and a peer receiving the entry has
// no prior record for that backend either — also term 0 — so the comparison
// `h.Term > existing.Term` was `0 > 0` and the report was dropped. A backend
// discovered unhealthy on startup therefore never propagated anywhere.
func TestNode_SetHealthAdvancesTerm(t *testing.T) {
	n := cluster.NewNode("node1", "127.0.0.1:7946", nil, 5)

	n.SetHealth("http://backend1", true)
	first := n.GetTerm()

	n.SetHealth("http://backend1", false)
	second := n.GetTerm()

	if second <= first {
		t.Fatalf("each local health change must advance the term: %d then %d", first, second)
	}
}

// TestNode_FirstReportAcceptedByFreshPeer drives the merge path the way the
// gossip loop does: the sender stamps the message with its own term and ships
// its whole health map.
func TestNode_FirstReportAcceptedByFreshPeer(t *testing.T) {
	sender := cluster.NewNode("node-a", "127.0.0.1:7946", nil, 5)
	receiver := cluster.NewNode("node-b", "127.0.0.1:7947", nil, 5)

	sender.SetHealth("http://backend1", false)

	receiver.MergeGossip(cluster.GossipMessage{
		Version: "1",
		NodeID:  sender.NodeID,
		Term:    sender.GetTerm(),
		Health:  sender.GetMergedHealth(),
	})

	if receiver.GetHealth("http://backend1", true) {
		t.Fatal("a first-time health report must be accepted, not dropped as same-term")
	}
}

// ---------- Lifecycle ----------

func TestNode_StartReportsBindFailure(t *testing.T) {
	port := freeUDPPort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	first := cluster.NewNode("node-a", addr, nil, 5)
	if err := first.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer first.Stop()

	// A second node on the same address must fail loudly: a node that cannot
	// receive gossip still sends it, so peers see it as alive while every
	// health update they publish is silently ignored.
	second := cluster.NewNode("node-b", addr, nil, 5)
	if err := second.Start(); err == nil {
		second.Stop()
		t.Fatal("expected Start to fail when the gossip port is already bound")
	}
}

func TestNode_StopIsIdempotent(t *testing.T) {
	n := cluster.NewNode("node1", fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t)), nil, 1)
	if err := n.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	n.Stop()
	n.Stop() // must not panic on a closed channel
}

func TestNode_StopWithoutStart(t *testing.T) {
	n := cluster.NewNode("node1", "127.0.0.1:7946", nil, 5)
	n.Stop() // must not panic or block
}

// ---------- End to end over UDP ----------

// TestNode_GossipOverUDP is the test that would have caught the listenLoop bug:
// the loop unmarshalled the entire 4096-byte buffer rather than the bytes
// actually read, and json.Unmarshal rejects the trailing NULs, so every
// datagram was discarded before reaching MergeGossip.
func TestNode_GossipOverUDP(t *testing.T) {
	addrA := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	addrB := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))

	a := cluster.NewNode("node-a", addrA, []string{addrB}, 1)
	b := cluster.NewNode("node-b", addrB, []string{addrA}, 1)

	if err := a.Start(); err != nil {
		t.Fatalf("start node-a: %v", err)
	}
	defer a.Stop()
	if err := b.Start(); err != nil {
		t.Fatalf("start node-b: %v", err)
	}
	defer b.Stop()

	a.SetHealth("http://backend1", false)

	// Gossip rounds are driven by a 1s ticker, so allow several rounds.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if !b.GetHealth("http://backend1", true) {
			return // propagated
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("health state never propagated from node-a to node-b over UDP")
}

// TestNode_OwnGossipIgnored confirms a node never treats its own datagram as
// peer state, which would let one node's term climb without bound.
func TestNode_OwnGossipIgnored(t *testing.T) {
	n := cluster.NewNode("node1", "127.0.0.1:7946", nil, 5)
	n.MergeGossip(cluster.GossipMessage{
		Version: "1",
		NodeID:  n.NodeID, // same node
		Term:    99,
		Health:  map[string]cluster.Health{"http://backend1": {Healthy: false, Term: 99}},
	})

	if got := n.GetTerm(); got == 99 {
		t.Fatal("a node must not adopt a term from its own message")
	}
	if !n.GetHealth("http://backend1", true) {
		t.Fatal("a node must not apply health from its own message")
	}
}

// ---------- Config drift ----------

func TestNode_ConfigMismatchCounted(t *testing.T) {
	n := cluster.NewNode("node1", "127.0.0.1:7946", nil, 5)
	n.SetConfigHash("aaaa")

	n.MergeGossip(cluster.GossipMessage{Version: "1", NodeID: "node2", Term: 1, ConfigHash: "bbbb"})
	n.MergeGossip(cluster.GossipMessage{Version: "1", NodeID: "node3", Term: 2, ConfigHash: "bbbb"})

	if got := n.ConfigMismatches(); got != 2 {
		t.Fatalf("expected 2 config mismatches, got %d", got)
	}
}

func TestNode_MatchingConfigHashNotCounted(t *testing.T) {
	n := cluster.NewNode("node1", "127.0.0.1:7946", nil, 5)
	n.SetConfigHash("aaaa")

	n.MergeGossip(cluster.GossipMessage{Version: "1", NodeID: "node2", Term: 1, ConfigHash: "aaaa"})

	if got := n.ConfigMismatches(); got != 0 {
		t.Fatalf("matching config hash must not be counted, got %d", got)
	}
}

// TestNode_UnsetConfigHashNeverMismatches keeps the check opt-in: a node with no
// recorded hash (clustering wired without a config path) must not report every
// peer as drifted.
func TestNode_UnsetConfigHashNeverMismatches(t *testing.T) {
	n := cluster.NewNode("node1", "127.0.0.1:7946", nil, 5)

	n.MergeGossip(cluster.GossipMessage{Version: "1", NodeID: "node2", Term: 1, ConfigHash: "bbbb"})

	if got := n.ConfigMismatches(); got != 0 {
		t.Fatalf("a node without a config hash must not report drift, got %d", got)
	}
}

// ---------- Peer liveness ----------

// TestNode_LivePeerCount covers the meaning of haribon_cluster_peers: replicas
// actually heard from, not configured peer addresses. Counting configured
// addresses would report every peer as up forever, because a UDP send to a dead
// peer's address succeeds — there is no connection to refuse.
func TestNode_LivePeerCount(t *testing.T) {
	n := cluster.NewNode("node1", "127.0.0.1:7946", []string{"127.0.0.1:1", "127.0.0.1:2"}, 5)

	if got := n.LivePeerCount(); got != 0 {
		t.Fatalf("a peer we have never heard from must not be counted, got %d", got)
	}

	n.MergeGossip(cluster.GossipMessage{Version: "1", NodeID: "node2", Term: 1})
	n.MergeGossip(cluster.GossipMessage{Version: "1", NodeID: "node3", Term: 1})
	// Repeats from the same node are one peer, not three.
	n.MergeGossip(cluster.GossipMessage{Version: "1", NodeID: "node3", Term: 2})

	if got := n.LivePeerCount(); got != 2 {
		t.Fatalf("expected 2 peers heard from, got %d", got)
	}
}
