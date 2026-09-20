package cluster_test

import (
	"sync"
	"testing"

	"github.com/marcuwynu23/haribon/internal/cluster"
)

// ---------- Node Basics ----------

func TestNode_NewNode(t *testing.T) {
	n := cluster.NewNode("node1", "0.0.0.0:7946", []string{"node2:7946"}, 5)
	if n.NodeID != "node1" {
		t.Fatalf("node ID: %s", n.NodeID)
	}
	if len(n.Peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(n.Peers))
	}
	if n.GossipSec != 5 {
		t.Fatalf("gossip sec: %d", n.GossipSec)
	}
	if n.GossipAddr != "0.0.0.0:7946" {
		t.Fatalf("gossip addr: %s", n.GossipAddr)
	}
}

func TestNode_DefaultGossipSec(t *testing.T) {
	n := cluster.NewNode("node1", "0.0.0.0:7946", nil, 0)
	if n.GossipSec != 5 {
		t.Fatalf("default gossip sec should be 5, got %d", n.GossipSec)
	}
}

// ---------- Health ----------

func TestNode_SetAndGetHealth(t *testing.T) {
	n := cluster.NewNode("node1", "0.0.0.0:7946", nil, 5)
	n.SetHealth("http://backend1", true)
	n.SetHealth("http://backend2", false)

	if !n.GetHealth("http://backend1", true) {
		t.Fatal("backend1 should be healthy")
	}
	if n.GetHealth("http://backend2", true) {
		t.Fatal("backend2 should be unhealthy")
	}
}

func TestNode_MergedHealth(t *testing.T) {
	n := cluster.NewNode("node1", "0.0.0.0:7946", nil, 5)
	n.SetHealth("http://backend1", true)
	n.SetHealth("http://backend2", false)

	merged := n.GetMergedHealth()
	if len(merged) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(merged))
	}
}

// ---------- Gossip Merge ----------

func TestNode_MergeGossip_HigherTermWins(t *testing.T) {
	n := cluster.NewNode("node1", "0.0.0.0:7946", nil, 5)
	n.SetHealth("http://backend1", true)

	msg := cluster.GossipMessage{
		Version: "1",
		NodeID:  "node2",
		Term:    10,
		Health: map[string]cluster.Health{
			"http://backend1": {Healthy: false, Failures: 3, Term: 10},
		},
	}
	n.MergeGossip(msg)

	// Higher term should override local health
	if n.GetHealth("http://backend1", true) {
		t.Fatal("backend1 should be unhealthy from gossip")
	}
}

func TestNode_MergeGossip_LowerTermIgnored(t *testing.T) {
	n := cluster.NewNode("node1", "0.0.0.0:7946", nil, 5)
	n.SetHealth("http://backend1", true)
	n.IncrementTerm() // term = 1

	msg := cluster.GossipMessage{
		Version: "1",
		NodeID:  "node2",
		Term:    0,
		Health: map[string]cluster.Health{
			"http://backend1": {Healthy: false, Failures: 3, Term: 0},
		},
	}
	n.MergeGossip(msg)

	// Lower term should NOT override
	if !n.GetHealth("http://backend1", true) {
		t.Fatal("backend1 should remain healthy (lower term ignored)")
	}
}

func TestNode_MergeGossip_SameTermIgnored(t *testing.T) {
	n := cluster.NewNode("node1", "0.0.0.0:7946", nil, 5)
	n.IncrementTerm() // term = 1
	n.SetHealth("http://backend1", true)

	msg := cluster.GossipMessage{
		Version: "1",
		NodeID:  "node2",
		Term:    1,
		Health: map[string]cluster.Health{
			"http://backend1": {Healthy: false, Failures: 3, Term: 1},
		},
	}
	n.MergeGossip(msg)

	// Same term from different node should NOT override (local wins)
	if !n.GetHealth("http://backend1", true) {
		t.Fatal("backend1 should remain healthy (same term, local wins)")
	}
}

// ---------- Discover ----------

func TestDiscover_ValidPeers(t *testing.T) {
	peers, err := cluster.Discover([]string{"localhost:7946", "127.0.0.1:7946"})
	if err != nil {
		t.Fatalf("discover failed: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("expected 2 peers, got %d", len(peers))
	}
}

func TestDiscover_InvalidPeer(t *testing.T) {
	_, err := cluster.Discover([]string{"invalid_host_name_that_does_not_exist:7946"})
	if err == nil {
		t.Fatal("expected error for invalid peer")
	}
}

// ---------- Term ----------

func TestNode_TermIncrement(t *testing.T) {
	n := cluster.NewNode("node1", "0.0.0.0:7946", nil, 5)
	if n.GetTerm() != 0 {
		t.Fatalf("initial term: %d", n.GetTerm())
	}
	n.IncrementTerm()
	if n.GetTerm() != 1 {
		t.Fatalf("term after increment: %d", n.GetTerm())
	}
	n.IncrementTerm()
	if n.GetTerm() != 2 {
		t.Fatalf("term after second increment: %d", n.GetTerm())
	}
}

// ---------- Concurrency ----------

func TestNode_ConcurrentHealthAndGossip(t *testing.T) {
	n := cluster.NewNode("node1", "0.0.0.0:7946", nil, 5)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				n.SetHealth("http://backend1", j%2 == 0)
				n.GetTerm()
				n.IncrementTerm()
			}
		}(i)
	}
	wg.Wait()
}
