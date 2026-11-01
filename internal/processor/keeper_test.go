package processor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dedooxyz/dedoo-mm/internal/chain"
	"github.com/dedooxyz/dedoo-mm/internal/p2p"

	"github.com/btcsuite/btcd/btcec/v2"
)

// TestManualPendingThenApproveAndReadyManualBroadcast is the core keeper
// flow: proposal from node0 -> node1 stores PENDING (no blind signing) ->
// operator approves in UI -> 2-of-3 complete -> READY (no auto broadcast)
// -> manual broadcast exactly once.
func TestManualPendingThenApproveAndReadyManualBroadcast(t *testing.T) {
	keys := newTrioKeys(t)

	mc0, mc1 := newMockChain(), newMockChain()
	vaultFixture(t, mc0, keys.redeem, keys.vaultAddr)
	vaultFixture(t, mc1, keys.redeem, keys.vaultAddr)

	k0 := newTestKeeper(t, baseTestCfg(0, keys.vaultAddr, false, ""), mc0, keys.privs[0], keys.pubs[0], keys.redeem)
	k1 := newTestKeeper(t, baseTestCfg(1, keys.vaultAddr, false, ""), mc1, keys.privs[1], keys.pubs[1], keys.redeem)

	// node 0 proposes a 0.3 coin transfer
	id, err := k0.ProposeTransfer(keys.vaultAddr, canonicalDest, 30_000_000, "test transfer")
	if err != nil {
		t.Fatalf("ProposeTransfer: %v", err)
	}
	if len(mc0.broadcasts()) != 0 {
		t.Fatalf("proposal phase must not broadcast: %v", mc0.broadcasts())
	}

	// node 1 receives the proposal over p2p (simulate by calling handler)
	if err := k1.onProposeTransfer(*k0.props[id].transfer); err != nil {
		t.Fatalf("onProposeTransfer: %v", err)
	}
	st1 := k1.props[id]
	if st1 == nil {
		t.Fatal("node1 must store the incoming proposal")
	}
	if st1.status() != "pending" {
		t.Fatalf("expected pending (no blind sign), got %q", st1.status())
	}
	if st1.mySigned {
		t.Fatal("manual policy must NOT auto-sign incoming proposals")
	}
	if len(mc1.broadcasts()) != 0 {
		t.Fatalf("manual policy must not broadcast: %v", mc1.broadcasts())
	}

	// node 1 operator approves in the UI
	if _, err := k1.ApproveProposal(id); err != nil {
		t.Fatalf("ApproveProposal: %v", err)
	}
	if k1.props[id].status() != "ready" {
		t.Fatalf("expected ready after 2-of-3, got %q", k1.props[id].status())
	}
	if len(mc1.broadcasts()) != 0 {
		t.Fatal("manual broadcast policy must not broadcast automatically")
	}

	// node 0 receives node1's approval (the wire carries the merged hex)
	merged := k1.props[id].signedHex
	if err := k0.onApproval(id, keys.pubs[1], merged); err != nil {
		t.Fatalf("node0 onApproval: %v", err)
	}
	if k0.props[id].status() != "ready" {
		t.Fatalf("node0 expected ready, got %q", k0.props[id].status())
	}

	// manual broadcast on node 0 — exactly once
	if _, err := k0.BroadcastProposal(id); err != nil {
		t.Fatalf("BroadcastProposal: %v", err)
	}
	if n := len(mc0.broadcasts()); n != 1 {
		t.Fatalf("expected exactly 1 broadcast, got %d", n)
	}
	if _, err := k0.BroadcastProposal(id); err != nil {
		t.Fatalf("idempotent re-broadcast call: %v", err)
	}
	if n := len(mc0.broadcasts()); n != 1 {
		t.Fatalf("broadcast must dedupe: got %d", n)
	}

	// broadcasting an unknown proposal must fail
	if _, err := k0.BroadcastProposal("trzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"); err == nil {
		t.Fatal("broadcasting unknown proposal must fail")
	}
}

// TestTransferVerificationRejectsRogueProps covers the acceptance-critical
// checks: destination/intent tampering, change theft, gross inflation,
// foreign-script inputs and immature inputs.
func TestTransferVerificationRejectsRogueProps(t *testing.T) {
	keys := newTrioKeys(t)
	mc1 := newMockChain()
	vaultFixture(t, mc1, keys.redeem, keys.vaultAddr)
	k1 := newTestKeeper(t, baseTestCfg(1, keys.vaultAddr, false, ""), mc1, keys.privs[1], keys.pubs[1], keys.redeem)

	mc0 := newMockChain()
	vaultFixture(t, mc0, keys.redeem, keys.vaultAddr)
	k0 := newTestKeeper(t, baseTestCfg(0, keys.vaultAddr, false, ""), mc0, keys.privs[0], keys.pubs[0], keys.redeem)
	id, err := k0.ProposeTransfer(keys.vaultAddr, canonicalDest, 30_000_000, "honest")
	if err != nil {
		t.Fatalf("honest propose: %v", err)
	}
	honest := *k0.props[id].transfer
	if err := k1.verifyTransfer(honest); err != nil {
		t.Fatalf("honest proposal must verify: %v", err)
	}

	attacker := chain.EncodeAddress(chain.AddrPubkeyVersion, chain.Hash160([]byte("attacker-destination")))

	// helper: build a tx from scratch spending 2 fake vault utxos.
	build := func(outs []chain.Output) (string, []chain.UTXO) {
		t.Helper()
		utxos := rogueUtxos(2)
		registerUtxos(t, mc1, utxos, keys.redeem)
		raw, err := chain.BuildAndSignMultisigTx(utxos, outs, keys.redeem, []*btcec.PrivateKey{keys.privs[0]})
		if err != nil {
			t.Fatalf("build rogue: %v", err)
		}
		return raw, utxos
	}

	t.Run("destination mismatch", func(t *testing.T) {
		rogue := honest
		rogue.To = attacker
		if err := k1.verifyTransfer(rogue); err == nil {
			t.Fatal("tampered destination must be rejected")
		}
	})

	t.Run("change goes to attacker", func(t *testing.T) {
		raw, _ := build([]chain.Output{
			{Address: canonicalDest, ValueSats: 30_000_000},
			{Address: attacker, ValueSats: 19_000_000},
		})
		rogue := honest
		rogue.RawTxHex = raw
		rogue.GrossSats = 50_000_000
		rogue.FeeSats = 1_000_000
		if err := k1.verifyTransfer(rogue); err == nil {
			t.Fatal("change to a non-vault address must be rejected")
		}
	})

	t.Run("gross inflation", func(t *testing.T) {
		rogue := honest
		rogue.GrossSats = honest.GrossSats * 1000
		if err := k1.verifyTransfer(rogue); err == nil {
			t.Fatal("gross inflation must fail (input sum != gross)")
		}
	})

	t.Run("inputs locked to a foreign script", func(t *testing.T) {
		other, err := chain.BuildMultisigRedeemScript([]string{keys.pubs[0], keys.pubs[1], keys.pubs[2]}, []int{2, 1, 0}, 2)
		if err != nil {
			t.Fatal(err)
		}
		raw, utxos := build([]chain.Output{
			{Address: canonicalDest, ValueSats: 30_000_000},
			{Address: keys.vaultAddr, ValueSats: 19_000_000},
		})
		foreignSpk := p2shSpkHex(other)
		for _, u := range utxos {
			mc1.PrevOut(u.Txid, u.Vout, u.Value, foreignSpk, 50)
		}
		rogue := honest
		rogue.RawTxHex = raw
		rogue.GrossSats = 50_000_000
		rogue.FeeSats = 1_000_000
		if err := k1.verifyTransfer(rogue); err == nil {
			t.Fatal("inputs from a foreign script must be rejected")
		}
	})

	t.Run("immature inputs", func(t *testing.T) {
		raw, utxos := build([]chain.Output{
			{Address: canonicalDest, ValueSats: 30_000_000},
			{Address: keys.vaultAddr, ValueSats: 19_000_000},
		})
		spk := p2shSpkHex(keys.redeem)
		for _, u := range utxos {
			mc1.PrevOut(u.Txid, u.Vout, u.Value, spk, 5) // below maturity floor
		}
		rogue := honest
		rogue.RawTxHex = raw
		rogue.GrossSats = 50_000_000
		rogue.FeeSats = 1_000_000
		if err := k1.verifyTransfer(rogue); err == nil {
			t.Fatal("immature inputs must be rejected")
		}
	})
}

// TestRejectIsInformative: a reject never blocks — it is recorded, relayed
// and the proposal can still reach 2-of-3.
func TestRejectIsInformative(t *testing.T) {
	keys := newTrioKeys(t)
	mc0, mc1 := newMockChain(), newMockChain()
	vaultFixture(t, mc0, keys.redeem, keys.vaultAddr)
	vaultFixture(t, mc1, keys.redeem, keys.vaultAddr)
	k0 := newTestKeeper(t, baseTestCfg(0, keys.vaultAddr, false, ""), mc0, keys.privs[0], keys.pubs[0], keys.redeem)
	k1 := newTestKeeper(t, baseTestCfg(1, keys.vaultAddr, false, ""), mc1, keys.privs[1], keys.pubs[1], keys.redeem)

	id, err := k0.ProposeTransfer(keys.vaultAddr, canonicalDest, 30_000_000, "disputed")
	if err != nil {
		t.Fatalf("ProposeTransfer: %v", err)
	}
	prop := k0.props[id].transfer
	if err := k1.onProposeTransfer(*prop); err != nil {
		t.Fatalf("onProposeTransfer: %v", err)
	}
	if err := k1.RejectProposal(id, "I disagree"); err != nil {
		t.Fatalf("RejectProposal: %v", err)
	}
	if k1.props[id].status() != "pending" {
		t.Fatalf("reject must not change flow status, got %q", k1.props[id].status())
	}
	// leader sees the reject arriving over the wire
	if err := k0.onReject(p2p.RejectMsg{ProposalId: id, KeeperId: keys.pubs[1], Reason: "I disagree"}); err != nil {
		t.Fatalf("onReject: %v", err)
	}
	if got := k0.props[id].rejects[keys.pubs[1]]; got != "I disagree" {
		t.Fatalf("leader reject record missing: %q", got)
	}
	// ...and node1's operator can still approve later
	if _, err := k1.ApproveProposal(id); err != nil {
		t.Fatalf("ApproveProposal after reject: %v", err)
	}
	if k1.props[id].status() != "ready" {
		t.Fatalf("expected ready, got %q", k1.props[id].status())
	}
}

// TestUnknownApprovalTriggersRecovery: an approval for an id we never saw is
// tolerated (no crash) and requests the body from the cluster.
func TestUnknownApprovalTriggersRecovery(t *testing.T) {
	keys := newTrioKeys(t)
	k1 := newTestKeeper(t, baseTestCfg(1, keys.vaultAddr, false, ""), newMockChain(), keys.privs[1], keys.pubs[1], keys.redeem)

	if err := k1.onApproval("trunknown000000000000000000000002", keys.pubs[0], "deadhex"); err != nil {
		t.Fatalf("unknown approval must be tolerated, got: %v", err)
	}
	// request log rate-limits repeated recoveries
	if k1.reqLog["trunknown000000000000000000000002"].IsZero() {
		t.Fatal("recovery request not recorded")
	}
}

// TestStatePersistenceRoundtrip: proposals survive restart via StateFile.
func TestStatePersistenceRoundtrip(t *testing.T) {
	keys := newTrioKeys(t)
	dir := t.TempDir()
	sf := filepath.Join(dir, "state.json")

	mc0, mc1 := newMockChain(), newMockChain()
	vaultFixture(t, mc0, keys.redeem, keys.vaultAddr)
	vaultFixture(t, mc1, keys.redeem, keys.vaultAddr)

	k0 := newTestKeeper(t, baseTestCfg(0, keys.vaultAddr, false, sf), mc0, keys.privs[0], keys.pubs[0], keys.redeem)
	k1 := newTestKeeper(t, baseTestCfg(1, keys.vaultAddr, false, sf), mc1, keys.privs[1], keys.pubs[1], keys.redeem)

	id, err := k0.ProposeTransfer(keys.vaultAddr, canonicalDest, 30_000_000, "persist")
	if err != nil {
		t.Fatalf("ProposeTransfer: %v", err)
	}
	if err := k1.onProposeTransfer(*k0.props[id].transfer); err != nil {
		t.Fatal(err)
	}
	if _, err := k1.ApproveProposal(id); err != nil {
		t.Fatal(err)
	}

	// "restart" node1: fresh keeper, same config/state file
	k1b := newTestKeeper(t, baseTestCfg(1, keys.vaultAddr, false, sf), mc1, keys.privs[1], keys.pubs[1], keys.redeem)
	if err := k1b.LoadState(); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	st := k1b.props[id]
	if st == nil {
		t.Fatalf("proposal %s lost after restart", id)
	}
	if st.status() != "ready" {
		t.Fatalf("post-restart status = %q, want ready", st.status())
	}
	if !st.mySigned {
		t.Fatal("mySigned flag lost")
	}
	if len(st.signedHex) == 0 {
		t.Fatal("signed hex lost")
	}
	if _, err := os.Stat(sf); err != nil {
		t.Fatalf("state file missing: %v", err)
	}
}

// TestAutoSignLegacyEquivalence keeps AUTO_SIGN+AUTO_BROADCAST working:
// incoming proposals are cosigned and auto-broadcast immediately.
func TestAutoSignLegacyEquivalence(t *testing.T) {
	keys := newTrioKeys(t)
	mc0, mc1 := newMockChain(), newMockChain()
	vaultFixture(t, mc0, keys.redeem, keys.vaultAddr)
	vaultFixture(t, mc1, keys.redeem, keys.vaultAddr)

	cfg1 := baseTestCfg(1, keys.vaultAddr, true, "")
	cfg1.AutoBroadcast = true
	k1 := newTestKeeper(t, cfg1, mc1, keys.privs[1], keys.pubs[1], keys.redeem)

	k0 := newTestKeeper(t, baseTestCfg(0, keys.vaultAddr, false, ""), mc0, keys.privs[0], keys.pubs[0], keys.redeem)
	id, err := k0.ProposeTransfer(keys.vaultAddr, canonicalDest, 30_000_000, "legacy")
	if err != nil {
		t.Fatalf("ProposeTransfer: %v", err)
	}
	if err := k1.onProposeTransfer(*k0.props[id].transfer); err != nil {
		t.Fatalf("auto-cosign path failed: %v", err)
	}
	// k1 now has a live auto-broadcast goroutine; all reads go through the
	// locked view accessor.
	if !waitFor(50, func() bool { return propView(t, k1, id).Status == "broadcasted" }) {
		t.Fatalf("AUTO_SIGN+AUTO_BROADCAST must complete+broadcast, got %q", propView(t, k1, id).Status)
	}
	if n := len(mc1.broadcasts()); n < 1 {
		t.Fatal("auto broadcast missing")
	}
}

// TestConsolidationSingleOutput: consolidation to the same vault produces
// exactly ONE output consuming full balance minus fee.
func TestConsolidationSingleOutput(t *testing.T) {
	keys := newTrioKeys(t)
	mc0 := newMockChain()
	vaultFixture(t, mc0, keys.redeem, keys.vaultAddr) // gross = 80M
	k0 := newTestKeeper(t, baseTestCfg(0, keys.vaultAddr, false, ""), mc0, keys.privs[0], keys.pubs[0], keys.redeem)

	id, err := k0.ProposeTransfer(keys.vaultAddr, keys.vaultAddr, 80_000_000, "consolidate")
	if err != nil {
		t.Fatalf("ProposeTransfer consolidated: %v", err)
	}
	raw := k0.props[id].transfer.RawTxHex
	outs, err := chain.ParseOutputsFromRawHex(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 {
		t.Fatalf("consolidation must be 1 output, got %d", len(outs))
	}
	if outs[0].Address != keys.vaultAddr {
		t.Fatalf("output must target the vault, got %s", outs[0].Address)
	}
	wantFee := mc0.EstimateFeeSatsWithMin(2, 1, 6, 10)
	if outs[0].ValueSats != 80_000_000-wantFee {
		t.Fatalf("consolidation value %d, want %d", outs[0].ValueSats, 80_000_000-wantFee)
	}
}

// TestDustChangeRejected: amounts leaving < 15000 sats change are refused.
func TestDustChangeRejected(t *testing.T) {
	keys := newTrioKeys(t)
	mc0 := newMockChain()
	vaultFixture(t, mc0, keys.redeem, keys.vaultAddr)
	k0 := newTestKeeper(t, baseTestCfg(0, keys.vaultAddr, false, ""), mc0, keys.privs[0], keys.pubs[0], keys.redeem)

	fee := mc0.EstimateFeeSatsWithMin(2, 2, 6, 10)
	dirty := 80_000_000 - fee - 1000 // change = 1000 < DustLimit
	if _, err := k0.ProposeTransfer(keys.vaultAddr, canonicalDest, dirty, "dust"); err == nil {
		t.Fatal("dust change must be rejected")
	}
}

// TestTransferSelectsEnoughInputs: a transfer must keep adding UTXOs until
// the amount+fee is covered, not blindly use only the first N.
func TestTransferSelectsEnoughInputs(t *testing.T) {
	keys := newTrioKeys(t)
	mc := newMockChain()
	const n = 130
	const value = int64(1_000_000)
	spk := p2shSpkHex(keys.redeem)
	utxos := make([]chain.UTXO, n)
	for i := range utxos {
		utxos[i] = chain.UTXO{Txid: fmt.Sprintf("%064x", 2000+i), Vout: 0, Value: value, Confirmations: 40}
		mc.PrevOut(utxos[i].Txid, utxos[i].Vout, value, spk, 40)
	}
	mc.SetUTXOs(keys.vaultAddr, utxos)

	cfg := baseTestCfg(0, keys.vaultAddr, false, "")
	cfg.MaxTxInputs = 200
	k := newTestKeeper(t, cfg, mc, keys.privs[0], keys.pubs[0], keys.redeem)

	// 120 coins require 121 inputs once the fee is included.
	id, err := k.ProposeTransfer(keys.vaultAddr, canonicalDest, 120_000_000, "select many")
	if err != nil {
		t.Fatalf("ProposeTransfer: %v", err)
	}
	ins, err := chain.ParseInputsFromRawHex(k.props[id].transfer.RawTxHex)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) < 121 {
		t.Fatalf("expected the proposal to use >=121 inputs, got %d", len(ins))
	}
}

// TestConsolidationRespectsInputCap: consolidation is capped per tx and the
// memo says how many of the total UTXOs are being consolidated.
func TestConsolidationRespectsInputCap(t *testing.T) {
	keys := newTrioKeys(t)
	mc := newMockChain()
	const n = 5
	spk := p2shSpkHex(keys.redeem)
	utxos := make([]chain.UTXO, n)
	for i := range utxos {
		utxos[i] = chain.UTXO{Txid: fmt.Sprintf("%064x", 3000+i), Vout: 0, Value: 10_000_000, Confirmations: 40}
		mc.PrevOut(utxos[i].Txid, utxos[i].Vout, 10_000_000, spk, 40)
	}
	mc.SetUTXOs(keys.vaultAddr, utxos)

	cfg := baseTestCfg(0, keys.vaultAddr, false, "")
	cfg.MaxTxInputs = 3
	k := newTestKeeper(t, cfg, mc, keys.privs[0], keys.pubs[0], keys.redeem)

	id, err := k.ProposeTransfer(keys.vaultAddr, keys.vaultAddr, 50_000_000, "")
	if err != nil {
		t.Fatalf("consolidation: %v", err)
	}
	prop := k.props[id].transfer
	ins, err := chain.ParseInputsFromRawHex(prop.RawTxHex)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 3 {
		t.Fatalf("consolidation inputs = %d, want 3 (cap)", len(ins))
	}
	if !strings.Contains(prop.Memo, "3 of 5") {
		t.Fatalf("memo should mention 3 of 5, got %q", prop.Memo)
	}
}

// TestProposalIDCollision: identical in-flight submits reuse the proposal,
// but a terminal (e.g. already broadcast) proposal never gets overwritten.
func TestProposalIDCollision(t *testing.T) {
	keys := newTrioKeys(t)
	mc := newMockChain()
	vaultFixture(t, mc, keys.redeem, keys.vaultAddr)
	k := newTestKeeper(t, baseTestCfg(0, keys.vaultAddr, false, ""), mc, keys.privs[0], keys.pubs[0], keys.redeem)

	id1, err := k.ProposeTransfer(keys.vaultAddr, canonicalDest, 30_000_000, "same intent")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := k.ProposeTransfer(keys.vaultAddr, canonicalDest, 30_000_000, "same intent")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("in-flight duplicate should reuse id: %s vs %s", id1, id2)
	}

	k.mu.Lock()
	k.props[id1].broadcasted = true
	k.mu.Unlock()
	id3, err := k.ProposeTransfer(keys.vaultAddr, canonicalDest, 30_000_000, "same intent")
	if err != nil {
		t.Fatal(err)
	}
	if id3 == id1 {
		t.Fatal("terminal proposal id must not be reused")
	}
}

// --- fixture helpers ---------------------------------------------------------

func rogueUtxos(n int) []chain.UTXO {
	out := make([]chain.UTXO, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, chain.UTXO{
			Txid:          fmt.Sprintf("%064x", 900+i),
			Vout:          0,
			Value:         25_000_000,
			Confirmations: 50,
		})
	}
	return out
}

// registerUtxos writes matching prevouts (for the vault redeem) to the mock.
func registerUtxos(t *testing.T, mc *mockChain, utxos []chain.UTXO, redeemHex string) {
	t.Helper()
	spk := p2shSpkHex(redeemHex)
	for _, u := range utxos {
		mc.PrevOut(u.Txid, u.Vout, u.Value, spk, u.Confirmations)
	}
}
