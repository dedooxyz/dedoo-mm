package processor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dedooxyz/dedoo-mm/internal/chain"
)

func consolidationFixture(t *testing.T, mc *mockChain, keys trioKeys, n int, value int64) []chain.UTXO {
	t.Helper()
	spk := p2shSpkHex(keys.redeem)
	utxos := make([]chain.UTXO, n)
	for i := range utxos {
		utxos[i] = chain.UTXO{
			Txid:          fmt.Sprintf("%064x", 4000+i),
			Vout:          0,
			Value:         value + int64(i),
			Confirmations: 40,
		}
		mc.PrevOut(utxos[i].Txid, utxos[i].Vout, utxos[i].Value, spk, 40)
	}
	mc.SetUTXOs(keys.vaultAddr, utxos)
	return utxos
}

// TestProposeConsolidationSelectsRequestedUTXOs: the caller chooses the UTXO
// count and the output amount is derived from exactly those UTXOs minus fee.
func TestProposeConsolidationSelectsRequestedUTXOs(t *testing.T) {
	keys := newTrioKeys(t)
	mc := newMockChain()
	consolidationFixture(t, mc, keys, 5, 10_000_000)
	k := newTestKeeper(t, baseTestCfg(0, keys.vaultAddr, false, ""), mc, keys.privs[0], keys.pubs[0], keys.redeem)

	id, err := k.ProposeConsolidation(keys.vaultAddr, 3, "merge three")
	if err != nil {
		t.Fatalf("ProposeConsolidation: %v", err)
	}
	prop := k.props[id].transfer
	ins, err := chain.ParseInputsFromRawHex(prop.RawTxHex)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 3 {
		t.Fatalf("inputs = %d, want 3", len(ins))
	}

	wantGross := int64(10_000_000 + 10_000_001 + 10_000_002)
	wantFee := mc.EstimateFeeSatsWithMin(3, 1, 6, 10)
	if prop.GrossSats != wantGross {
		t.Fatalf("gross = %d, want %d", prop.GrossSats, wantGross)
	}
	if prop.FeeSats != wantFee {
		t.Fatalf("fee = %d, want %d", prop.FeeSats, wantFee)
	}
	if prop.AmountSats != wantGross-wantFee {
		t.Fatalf("amount = %d, want %d", prop.AmountSats, wantGross-wantFee)
	}
	if prop.Memo != "merge three" {
		t.Fatalf("memo = %q, want the caller memo", prop.Memo)
	}

	outs, err := chain.ParseOutputsFromRawHex(prop.RawTxHex)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 || outs[0].Address != keys.vaultAddr || outs[0].ValueSats != wantGross-wantFee {
		t.Fatalf("outputs = %+v, want one output to the vault with %d sats", outs, wantGross-wantFee)
	}
}

// TestPlanConsolidation: the preview selects the same UTXOs the proposal will
// spend and reports the derived amount.
func TestPlanConsolidation(t *testing.T) {
	keys := newTrioKeys(t)
	mc := newMockChain()
	consolidationFixture(t, mc, keys, 5, 10_000_000)
	k := newTestKeeper(t, baseTestCfg(0, keys.vaultAddr, false, ""), mc, keys.privs[0], keys.pubs[0], keys.redeem)

	all, err := k.PlanConsolidation(keys.vaultAddr, 0)
	if err != nil {
		t.Fatalf("plan all: %v", err)
	}
	if all.Available != 5 || all.Selected != 5 || all.Requested != 0 {
		t.Fatalf("plan all = %+v", all)
	}

	two, err := k.PlanConsolidation(keys.vaultAddr, 2)
	if err != nil {
		t.Fatalf("plan two: %v", err)
	}
	if two.Selected != 2 || two.GrossSats != 10_000_000+10_000_001 {
		t.Fatalf("plan two = %+v", two)
	}
	if two.NetSats != two.GrossSats-two.FeeSats {
		t.Fatalf("net %d != gross %d - fee %d", two.NetSats, two.GrossSats, two.FeeSats)
	}
	if len(two.UTXOs) != 2 {
		t.Fatalf("plan utxos = %d, want 2", len(two.UTXOs))
	}

	if _, err := k.PlanConsolidation(keys.vaultAddr, 6); err == nil {
		t.Fatal("requesting more UTXOs than available must fail")
	}
}

// TestConsolidationRespectsConfiguredLimit: an explicit count above
// MAX_TX_INPUTS is refused instead of silently truncated.
func TestConsolidationRespectsConfiguredLimit(t *testing.T) {
	keys := newTrioKeys(t)
	mc := newMockChain()
	consolidationFixture(t, mc, keys, 5, 10_000_000)
	cfg := baseTestCfg(0, keys.vaultAddr, false, "")
	cfg.MaxTxInputs = 2
	k := newTestKeeper(t, cfg, mc, keys.privs[0], keys.pubs[0], keys.redeem)

	if _, err := k.ProposeConsolidation(keys.vaultAddr, 3, ""); err == nil || !strings.Contains(err.Error(), "per-transaction limit") {
		t.Fatalf("expected a per-transaction limit error, got %v", err)
	}
	if _, err := k.PlanConsolidation(keys.vaultAddr, 3); err == nil {
		t.Fatal("plan above the configured limit must fail")
	}

	// 0 means "all", which is capped rather than rejected.
	id, err := k.ProposeConsolidation(keys.vaultAddr, 0, "")
	if err != nil {
		t.Fatalf("consolidate all: %v", err)
	}
	prop := k.props[id].transfer
	ins, _ := chain.ParseInputsFromRawHex(prop.RawTxHex)
	if len(ins) != 2 {
		t.Fatalf("consolidate all inputs = %d, want 2 (cap)", len(ins))
	}
	if !strings.Contains(prop.Memo, "2 of 5") || !strings.Contains(prop.Memo, "max 2 per tx") {
		t.Fatalf("memo = %q, want 2 of 5 and the per-tx cap", prop.Memo)
	}
}

// TestChainLimitEnforced: the hard bound is the chain's standard transaction
// size, even when MAX_TX_INPUTS is configured higher.
func TestChainLimitEnforced(t *testing.T) {
	keys := newTrioKeys(t)
	mc := newMockChain()
	const n = 400
	consolidationFixture(t, mc, keys, n, 10_000_000)
	cfg := baseTestCfg(0, keys.vaultAddr, false, "")
	cfg.MaxTxInputs = 600 // above the chain's ~333-input standard limit
	k := newTestKeeper(t, cfg, mc, keys.privs[0], keys.pubs[0], keys.redeem)

	plan, err := k.PlanConsolidation(keys.vaultAddr, 0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.ChainLimit < 300 || plan.ChainLimit > 340 {
		t.Fatalf("chain limit = %d, want about 333", plan.ChainLimit)
	}
	if plan.MaxInputs != plan.ChainLimit {
		t.Fatalf("effective max = %d, want chain limit %d", plan.MaxInputs, plan.ChainLimit)
	}
	if plan.Selected != plan.ChainLimit {
		t.Fatalf("selected = %d, want %d (capped)", plan.Selected, plan.ChainLimit)
	}

	if _, err := k.ProposeConsolidation(keys.vaultAddr, plan.ChainLimit+1, ""); err == nil || !strings.Contains(err.Error(), "chain limit") {
		t.Fatalf("expected a chain limit error, got %v", err)
	}

	// A transfer that would need more inputs than the chain allows must fail
	// with a consolidation hint rather than build an unrelayable transaction.
	tooMuch := int64(plan.ChainLimit+1) * 10_000_000
	if _, err := k.ProposeTransfer(keys.vaultAddr, canonicalDest, tooMuch, ""); err == nil || !strings.Contains(err.Error(), "chain limit") {
		t.Fatalf("expected a chain limit transfer error, got %v", err)
	}
}

// TestTransferFailsWhenInputsExceedLimit: a normal transfer stops at the input
// cap and reports the limit instead of dropping inputs or over-spending.
func TestTransferFailsWhenInputsExceedLimit(t *testing.T) {
	keys := newTrioKeys(t)
	mc := newMockChain()
	consolidationFixture(t, mc, keys, 10, 10_000_000)
	cfg := baseTestCfg(0, keys.vaultAddr, false, "")
	cfg.MaxTxInputs = 3
	k := newTestKeeper(t, cfg, mc, keys.privs[0], keys.pubs[0], keys.redeem)

	// 25M + fee fits in 3 inputs.
	id, err := k.ProposeTransfer(keys.vaultAddr, canonicalDest, 25_000_000, "fits")
	if err != nil {
		t.Fatalf("transfer within limit: %v", err)
	}
	ins, _ := chain.ParseInputsFromRawHex(k.props[id].transfer.RawTxHex)
	if len(ins) != 3 {
		t.Fatalf("inputs = %d, want 3", len(ins))
	}

	// 35M needs a fourth input, which the cap forbids.
	_, err = k.ProposeTransfer(keys.vaultAddr, canonicalDest, 35_000_000, "too many")
	if err == nil || !strings.Contains(err.Error(), "needs more than 3 inputs") || !strings.Contains(err.Error(), "consolidate") {
		t.Fatalf("expected an input-limit error with a consolidation hint, got %v", err)
	}
}
