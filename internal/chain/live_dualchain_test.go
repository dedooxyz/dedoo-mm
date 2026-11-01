package chain

import (
	"os"
	"testing"
	"time"
)

// Live checks against the public Esplora endpoints for both supported chains.
// Skipped with -short (like TestLiveElectrsVault); otherwise they require
// network access.

func TestLiveWojakChainDefaults(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	c := NewElectrsClient(WojakCoinParams.DefaultElectrsURL, 15*time.Second)

	tip, err := c.getTipHeight()
	if err != nil {
		t.Fatalf("WJK tip: %v", err)
	}
	if tip <= 0 {
		t.Fatalf("WJK tip = %d", tip)
	}

	// Optional fixture: point WJK_LIVE_ADDRESS at a funded watch-only address.
	addr := os.Getenv("WJK_LIVE_ADDRESS")
	if addr == "" {
		t.Log("WJK_LIVE_ADDRESS not set: skipping the funded-address fixture")
		return
	}
	st, err := c.getAddressStats(addr)
	if err != nil {
		t.Fatalf("%s stats: %v", addr, err)
	}
	if st.UnspentCount <= 0 || st.UnspentValue <= 0 {
		t.Fatalf("%s empty: %+v", addr, st)
	}
	t.Logf("vault %s: %d utxos, %.4f coin", addr, st.UnspentCount, float64(st.UnspentValue)/1e8)

	// UTXO paging must stop at the safety cap on daemon addresses (tens of
	// thousands of coinbase outputs exist at the vault).
	utxos, err := c.getUTXOs(addr, 20)
	if err != nil {
		t.Fatalf("vault utxos: %v", err)
	}
	if len(utxos) == 0 || len(utxos) > maxUTXOEntries {
		t.Fatalf("bounded utxo fetch = %d entries (want 1..%d)", len(utxos), maxUTXOEntries)
	}
	t.Logf("bounded vault fetch: %d utxos (max %d)", len(utxos), maxUTXOEntries)
}

func TestLiveJunkcoinEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	c := NewElectrsClient(JunkcoinParams.DefaultElectrsURL, 15*time.Second)

	tip, err := c.getTipHeight()
	if err != nil {
		t.Fatalf("JKC tip: %v", err)
	}
	if tip < 1_000_000 {
		t.Fatalf("JKC tip = %d, expected >1M", tip)
	}

	// Optional fixture: point JKC_LIVE_ADDRESS at a funded watch-only address.
	addr := os.Getenv("JKC_LIVE_ADDRESS")
	if addr == "" {
		t.Log("JKC_LIVE_ADDRESS not set: skipping the funded-address fixture")
		return
	}
	st, err := c.getAddressStats(addr)
	if err != nil {
		t.Fatalf("JKC stats: %v", err)
	}
	if st.UnspentCount <= 0 || st.UnspentValue <= 0 {
		t.Fatalf("JKC fixture address empty: %+v", st)
	}
	t.Logf("JKC tip=%d fixture=%s utxos=%d value=%d sats", tip, addr, st.UnspentCount, st.UnspentValue)

	// Core paging path also works on the JKC endpoint.
	utxos, err := c.getUTXOs(addr, 1)
	if err != nil {
		t.Fatalf("JKC utxos: %v", err)
	}
	if len(utxos) == 0 || len(utxos) > maxUTXOEntries {
		t.Fatalf("JKC utxo fetch = %d", len(utxos))
	}
}
