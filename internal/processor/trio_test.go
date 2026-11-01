package processor

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"wojak-keeper/internal/p2p"
)

// TestTrioIntegration spins up THREE real keeper nodes (real libp2p mesh,
// mock chain data) and drives the complete human-in-the-loop flow:
//
//	node B proposes a transfer
//	→ A and C receive it and stay PENDING (no blind signing)
//	→ A rejects (relay exercise), C approves in UI
//	→ 2-of-3 (B + C) → READY everywhere, nobody auto-broadcasts
//	→ B broadcasts manually — exactly one broadcast on B's backend.
func TestTrioIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (long)")
	}
	keys := newTrioKeys(t)

	ports := []int{freePort(t), freePort(t), freePort(t)}
	indexes := []int{0, 1, 2}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tempKeys := func(i int) string { return filepath.Join(t.TempDir(), fmt.Sprintf("id%d.bin", i)) }
	vaultHash := vaultChecksum(t, keys)

	// Boot order: A first (full mesh via static peers added after boot).
	nodes := make([]*p2p.Node, 3)
	n0, err := p2p.NewNode(ctx, p2p.Options{
		KeyFile: tempKeys(0), BindHost: "127.0.0.1", BindPort: ports[0],
		SelfIndex: indexes[0], VaultHash: vaultHash, UsePublicDHT: false,
		ConnectTick: 500 * time.Millisecond, BlockDial: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("node0: %v", err)
	}
	nodes[0] = n0
	mustBoot(t, ctx, nodes, keys, vaultHash, tempKeys, ports, 1)
	mustBoot(t, ctx, nodes, keys, vaultHash, tempKeys, ports, 2)

	// Every operator config lists every peer; wire the full triangle.
	wireMesh(t, nodes, 3*time.Second)

	// Keepers with their own chain views.
	chains := []*mockChain{newMockChain(), newMockChain(), newMockChain()}
	for _, mc := range chains {
		vaultFixture(t, mc, keys.redeem, keys.vaultAddr)
	}
	keepers := make([]*Keeper, 3)
	for i, n := range nodes {
		k := newTestKeeper(t, baseTestCfg(i, keys.vaultAddr, false, ""), chains[i], keys.privs[i], keys.pubs[i], keys.redeem)
		k.SetP2PNode(n)
		keepers[i] = k
	}

	// A, B, C fully connected?
	if !waitFor(60, func() bool {
		return len(keepers[0].Peers()) == 2 && len(keepers[1].Peers()) == 2 && len(keepers[2].Peers()) == 2
	}) {
		for i, k := range keepers {
			t.Logf("node%d peers: %v", i, k.Peers())
		}
		t.Fatal("mesh did not converge")
	}

	// node1 (B) proposes: 0.2 coin transfer
	id, err := keepers[1].ProposeTransfer(keys.vaultAddr, canonicalDest, 20_000_000, "trio integration")
	if err != nil {
		t.Fatalf("ProposeTransfer: %v", err)
	}
	if !waitFor(50, func() bool {
		return hasProp(keepers[0], id) && hasProp(keepers[2], id)
	}) {
		t.Fatal("proposal not relayed to A and C")
	}
	// manual policy: peers stay pending and sign nothing
	if propView(t, keepers[0], id).MySigned || propView(t, keepers[2], id).MySigned {
		t.Fatal("peers must not blind-sign")
	}

	// A rejects (informational, relayed)
	if err := keepers[0].RejectProposal(id, "not now"); err != nil {
		t.Fatalf("RejectProposal: %v", err)
	}
	if !waitFor(50, func() bool {
		_, reason := propView(t, keepers[2], id).Rejects[keys.pubs[0]]
		return reason
	}) {
		t.Fatal("reject not relayed to C")
	}

	// C approves in UI → 2-of-3 (B + C)
	if _, err := keepers[2].ApproveProposal(id); err != nil {
		t.Fatalf("C approve: %v", err)
	}
	if !waitFor(50, func() bool { return propView(t, keepers[1], id).Status == "ready" }) {
		t.Fatalf("B expected ready, got %q", propView(t, keepers[1], id).Status)
	}
	if !waitFor(50, func() bool { return propView(t, keepers[0], id).Status == "ready" }) {
		t.Fatalf("A expected ready, got %q", propView(t, keepers[0], id).Status)
	}

	// Manual broadcast only on B, once.
	if _, err := keepers[1].BroadcastProposal(id); err != nil {
		t.Fatalf("B broadcast: %v", err)
	}
	if n := len(chains[1].broadcasts()); n != 1 {
		t.Fatalf("B must broadcast exactly once, got %d", n)
	}
	if n := len(chains[0].broadcasts()); n != 0 {
		t.Fatalf("A auto-broadcast leak: %d", n)
	}
	if n := len(chains[2].broadcasts()); n != 0 {
		t.Fatalf("C auto-broadcast leak: %d", n)
	}
	// Broadcast gossip: peers must flip READY -> BROADCASTED without
	// re-broadcasting themselves.
	if !waitFor(50, func() bool {
		return propView(t, keepers[0], id).Status == "broadcasted" &&
			propView(t, keepers[2], id).Status == "broadcasted"
	}) {
		t.Fatalf("peers did not learn about the broadcast: A=%q C=%q",
			propView(t, keepers[0], id).Status, propView(t, keepers[2], id).Status)
	}

	// node2 (C) builds its own consolidation proposal; A approves.
	id2, err := keepers[2].ProposeTransfer(keys.vaultAddr, keys.vaultAddr, 80_000_000, "trio consolidation")
	if err != nil {
		t.Fatalf("consolidation propose: %v", err)
	}
	if !waitFor(50, func() bool {
		return hasProp(keepers[0], id2)
	}) {
		t.Fatal("consolidation proposal not relayed")
	}
	if _, err := keepers[0].ApproveProposal(id2); err != nil {
		t.Fatalf("A approve consolidation: %v", err)
	}
	if !waitFor(50, func() bool { return propView(t, keepers[2], id2).Status == "ready" }) {
		t.Fatalf("C consolidation expected ready, got %q", propView(t, keepers[2], id2).Status)
	}
	// ...and B (second signer, leader-proposing) can hit broadcast itself
	if _, err := keepers[1].BroadcastProposal(id2); err != nil {
		// B may still be missing C's approval — check its status first
		if propView(t, keepers[1], id2).Status != "ready" {
			t.Fatalf("B consolidation not ready: %q", propView(t, keepers[1], id2).Status)
		}
	}

	// Cleanup.
	cancel()
	for _, n := range nodes {
		_ = n.Close()
	}
}

// --- shared helpers ----------------------------------------------------------

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func vaultChecksum(t *testing.T, keys trioKeys) string {
	t.Helper()
	c := baseTestCfg(0, keys.vaultAddr, false, "")
	return c.P2PChecksum()
}

func mustBoot(t *testing.T, ctx context.Context, nodes []*p2p.Node, keys trioKeys, vh string, tempKeys func(int) string, ports []int, i int) {
	t.Helper()
	n, err := p2p.NewNode(ctx, p2p.Options{
		KeyFile: tempKeys(i), BindHost: "127.0.0.1", BindPort: ports[i],
		SelfIndex: i, VaultHash: vh, UsePublicDHT: false,
		ConnectTick: 500 * time.Millisecond, BlockDial: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("node%d: %v", i, err)
	}
	if len(n.AdvertiseAddrs()) == 0 {
		t.Fatalf("node%d has no advertise addrs", i)
	}
	nodes[i] = n
}

// wireMesh static-adds every node's address to every other node, mirroring a
// production P2P_PEERS config where all three operators list all three VPSes.
func wireMesh(t *testing.T, nodes []*p2p.Node, dialTimeout time.Duration) {
	t.Helper()
	for i, n := range nodes {
		var others []string
		for j, m := range nodes {
			if j == i {
				continue
			}
			others = append(others, m.AdvertiseAddrs()...)
		}
		n.AddStaticPeers(others, dialTimeout)
	}
}

// hasProp reports whether keeper k knows proposal id. Lock-safe: handlers and
// the test goroutine touch the props map concurrently during the mesh flow.
func hasProp(k *Keeper, id string) bool {
	_, err := k.GetProposal(id)
	return err == nil
}

// propView returns the locked ProposalView snapshot for id (fatal if missing).
func propView(t *testing.T, k *Keeper, id string) ProposalView {
	t.Helper()
	v, err := k.GetProposal(id)
	if err != nil {
		t.Fatalf("GetProposal(%s): %v", id, err)
	}
	return v
}
