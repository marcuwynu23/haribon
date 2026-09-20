package cluster

// cluster_expiry_test.go — tests for the lifetime of a peer's health finding.
//
// Peer findings used to live forever. A replica that marked a backend down and
// then died left that verdict in every other replica's map with nothing to
// clear it, so a backend that was perfectly healthy stayed out of rotation
// cluster-wide until the remaining replicas were restarted. These tests are in
// package cluster (not cluster_test) so they can shorten staleAfter instead of
// sleeping for the real 15-second floor.

import (
	"testing"
	"time"
)

// remoteFinding builds the message a peer sends after marking a backend down.
func remoteFinding(nodeID string, term uint64, healthy bool) GossipMessage {
	return GossipMessage{
		Version: "1",
		NodeID:  nodeID,
		Term:    term,
		Health:  map[string]Health{"b1": {Healthy: healthy, Term: term}},
	}
}

func TestNode_RemoteFindingExpires(t *testing.T) {
	n := NewNode("node-b", "", nil, 5)
	n.staleAfter = 10 * time.Millisecond

	n.MergeGossip(remoteFinding("node-a", 1, false))
	if n.GetHealth("b1", true) {
		t.Fatal("a peer's finding must apply as soon as it arrives")
	}

	time.Sleep(25 * time.Millisecond)
	n.expireRemote()

	if !n.GetHealth("b1", true) {
		t.Fatal("a replica that stopped reporting must not keep suppressing a backend")
	}
}

// TestNode_LocalFindingRestoredWhenPeerFindingExpires covers the case the
// expiry must not get wrong: dropping the departed peer's entry must not throw
// away this node's own finding for the same backend along with it.
func TestNode_LocalFindingRestoredWhenPeerFindingExpires(t *testing.T) {
	n := NewNode("node-b", "", nil, 5)
	n.SetHealth("b1", true) // this node probes it and finds it healthy

	peer := NewNode("node-a", "", nil, 5)
	peer.SetHealth("b1", false)
	peer.SetHealth("b1", false) // second stamp so the entry term beats the local one

	n.staleAfter = 10 * time.Millisecond
	n.MergeGossip(GossipMessage{
		Version: "1",
		NodeID:  peer.NodeID,
		Term:    peer.GetTerm(),
		Health:  peer.ownHealth(),
	})

	if n.GetHealth("b1", true) {
		t.Fatal("the peer's newer finding should win while the peer is reporting")
	}

	time.Sleep(25 * time.Millisecond)
	n.expireRemote()

	if !n.GetHealth("b1", true) {
		t.Fatal("once the peer is gone this node's own finding must come back")
	}
}

// TestNode_PeerFindingsAreNotRelayed guards the invariant the expiry depends on.
// If a node forwarded a peer's finding, every other node would keep refreshing
// it, so the finding would outlive the replica that produced it and the expiry
// window would never close.
func TestNode_PeerFindingsAreNotRelayed(t *testing.T) {
	n := NewNode("node-b", "", nil, 5)
	n.MergeGossip(remoteFinding("node-a", 1, false))

	if _, relayed := n.ownHealth()["b1"]; relayed {
		t.Fatal("a peer's finding must not become this node's own and be gossiped onward")
	}
	if n.GetHealth("b1", true) {
		t.Fatal("the peer's finding should still apply locally")
	}
}

// TestNode_ReassertionRefreshesExpiry is the regression test for a subtle way
// the expiry could have gone wrong: sendGossip ships the whole map every round,
// mostly unchanged, so the same entry arrives repeatedly with the same term.
// Restamping only on a term change would let a live, still-reporting peer's
// finding expire out from under it.
func TestNode_ReassertionRefreshesExpiry(t *testing.T) {
	n := NewNode("node-b", "", nil, 5)
	n.staleAfter = 40 * time.Millisecond

	msg := remoteFinding("node-a", 1, false)

	n.MergeGossip(msg)
	time.Sleep(25 * time.Millisecond)
	n.MergeGossip(msg) // same term: the sender is alive and still asserting it
	time.Sleep(25 * time.Millisecond)

	n.expireRemote() // 50ms since the first report, 25ms since the last

	if n.GetHealth("b1", true) {
		t.Fatal("a peer that is still re-asserting a finding must not have it expire")
	}
}

// TestNode_OlderPeerFindingDoesNotClobberLocal pins the precedence rule: a
// first-hand finding outranks a peer entry carrying an older term, whatever
// order the two arrive in.
func TestNode_OlderPeerFindingDoesNotClobberLocal(t *testing.T) {
	n := NewNode("node-b", "", nil, 5)
	n.SetHealth("b1", true) // entry term 1

	n.MergeGossip(GossipMessage{
		Version: "1",
		NodeID:  "node-a",
		Term:    1,
		Health:  map[string]Health{"b1": {Healthy: false, Term: 0}},
	})

	if !n.GetHealth("b1", true) {
		t.Fatal("a peer entry with an older term must not overwrite this node's own finding")
	}
}

// TestNode_MergedHealthHidesStalePeerFindings checks the read path the cluster
// metrics use, which must not report a departed replica's findings even between
// gossip ticks.
func TestNode_MergedHealthHidesStalePeerFindings(t *testing.T) {
	n := NewNode("node-b", "", nil, 5)
	n.staleAfter = 10 * time.Millisecond

	n.MergeGossip(remoteFinding("node-a", 1, false))
	if got := len(n.GetMergedHealth()); got != 1 {
		t.Fatalf("expected the peer's finding to be visible, got %d entries", got)
	}

	time.Sleep(25 * time.Millisecond)

	if got := len(n.GetMergedHealth()); got != 0 {
		t.Fatalf("expected the stale finding to be hidden, got %d entries", got)
	}
}

// TestNode_LivePeerCountForgetsDepartedReplica keeps haribon_cluster_peers
// honest during a rolling update: replicas that stop reporting must leave the
// count, otherwise the metric an operator watches for "are my peers up?" only
// ever climbs to a number that means nothing.
func TestNode_LivePeerCountForgetsDepartedReplica(t *testing.T) {
	n := NewNode("node-b", "", nil, 5)
	n.staleAfter = 10 * time.Millisecond

	n.MergeGossip(GossipMessage{Version: "1", NodeID: "node-a", Term: 1})
	if got := n.LivePeerCount(); got != 1 {
		t.Fatalf("expected 1 peer heard from, got %d", got)
	}

	time.Sleep(25 * time.Millisecond)

	if got := n.LivePeerCount(); got != 0 {
		t.Fatalf("a replica that stopped reporting must leave the count, got %d", got)
	}
}
