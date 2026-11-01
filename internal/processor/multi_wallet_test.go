package processor

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/dedooxyz/dedoo-mm/internal/chain"
	"github.com/dedooxyz/dedoo-mm/internal/p2p"
)

// TestTrioMultiWallet runs one trio that manages TWO multisig wallets on the
// same chain. A key is shared across wallets; switching the proposal target is
// enough to sign for the second wallet.
func TestTrioMultiWallet(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (long)")
	}
	keys := newTrioKeys(t)

	// A second wallet over the same ceremony keys, different order.
	secondRedeem, err := chain.BuildMultisigRedeemScript(
		[]string{keys.pubs[0], keys.pubs[1], keys.pubs[2]}, []int{2, 1, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	secondAddr := p2shFromRedeem(secondRedeem)

	ports := []int{freePort(t), freePort(t), freePort(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tempKeys := func(i int) string { return filepath.Join(t.TempDir(), fmt.Sprintf("id%d.bin", i)) }
	vaultHash := vaultChecksum(t, keys)

	nodes := make([]*p2p.Node, 3)
	n0, err := p2p.NewNode(ctx, p2p.Options{
		KeyFile: tempKeys(0), BindHost: "127.0.0.1", BindPort: ports[0],
		SelfIndex: 0, VaultHash: vaultHash, UsePublicDHT: false,
		ConnectTick: 500 * time.Millisecond, BlockDial: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes[0] = n0
	mustBoot(t, ctx, nodes, keys, vaultHash, tempKeys, ports, 1)
	mustBoot(t, ctx, nodes, keys, vaultHash, tempKeys, ports, 2)
	wireMesh(t, nodes, 3*time.Second)

	chains := []*mockChain{newMockChain(), newMockChain(), newMockChain()}
	for _, mc := range chains {
		vaultFixture(t, mc, keys.redeem, keys.vaultAddr)
		vaultFixture(t, mc, secondRedeem, secondAddr)
	}
	keepers := make([]*Keeper, 3)
	for i, n := range nodes {
		k := newTestKeeper(t, baseTestCfg(i, keys.vaultAddr, false, ""), chains[i], keys.privs[i], keys.pubs[i], keys.redeem)
		k.SetP2PNode(n)
		keepers[i] = k
	}
	if !waitFor(60, func() bool {
		return len(keepers[0].Peers()) == 2 && len(keepers[1].Peers()) == 2 && len(keepers[2].Peers()) == 2
	}) {
		t.Fatal("mesh did not converge")
	}

	// Every node adds the same second wallet (metadata + shared key).
	req := CreateAccountRequest{
		Name: "Second Vault", Chain: "wojakcoin",
		Pubkeys: []string{keys.pubs[0], keys.pubs[1], keys.pubs[2]},
		Order:   []int{2, 1, 0}, Required: 2,
	}
	for i, k := range keepers {
		acct, _, err := k.CreateAccount(req)
		if err != nil {
			t.Fatalf("node%d create wallet: %v", i, err)
		}
		if acct.Address != secondAddr {
			t.Fatalf("node%d address = %s, want %s", i, acct.Address, secondAddr)
		}
		if !acct.HasKey {
			t.Fatalf("node%d must sign the second wallet with the shared key", i)
		}
	}

	// Propose on node1 for the SECOND wallet.
	id, err := keepers[1].ProposeTransfer(secondAddr, canonicalDest, 20_000_000, "second wallet")
	if err != nil {
		t.Fatalf("ProposeTransfer second wallet: %v", err)
	}
	if !waitFor(50, func() bool {
		return hasProp(keepers[0], id) && hasProp(keepers[2], id)
	}) {
		t.Fatal("second-wallet proposal not relayed")
	}
	if v := propView(t, keepers[0], id); v.Vault != secondAddr {
		t.Fatalf("proposal vault = %s, want %s", v.Vault, secondAddr)
	}

	// Approve on the other two nodes, then broadcast.
	for _, p := range []int{0, 2} {
		if _, err := keepers[p].ApproveProposal(id); err != nil {
			t.Fatalf("node%d approve: %v", p, err)
		}
	}
	if !waitFor(50, func() bool {
		return propView(t, keepers[1], id).Status == "ready"
	}) {
		t.Fatalf("node1 expected ready, got %q", propView(t, keepers[1], id).Status)
	}
	if _, err := keepers[1].BroadcastProposal(id); err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	if n := len(chains[1].broadcasts()); n != 1 {
		t.Fatalf("broadcasts = %d, want 1", n)
	}
	if !waitFor(50, func() bool {
		return propView(t, keepers[0], id).Status == "broadcasted" &&
			propView(t, keepers[2], id).Status == "broadcasted"
	}) {
		t.Fatal("broadcast gossip did not reach peers")
	}

	// The legacy wallet must still be present and untouched.
	if acct, _ := keepers[0].GetAccount(keys.vaultAddr); !acct.Active || !acct.HasKey {
		t.Fatalf("legacy wallet lost: %+v", acct)
	}
}
