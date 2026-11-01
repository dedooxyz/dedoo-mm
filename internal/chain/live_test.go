package chain

import (
	"os"
	"testing"
	"time"
)

// Live check against wojak-api.s3na.xyz — skipped unless WJK_LIVE=1.
func TestLiveElectrsVault(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	addr := os.Getenv("WJK_LIVE_ADDRESS")
	if addr == "" {
		t.Skip("set WJK_LIVE_ADDRESS to run the live vault check")
	}
	c := NewElectrsClient("https://wojak-api.s3na.xyz", 15*time.Second)
	utxos, err := c.getUTXOs(addr, 20)
	if err != nil {
		t.Fatalf("getUTXOs: %v", err)
	}
	t.Logf("mature(20) utxos: %d", len(utxos))
	if len(utxos) == 0 {
		t.Fatal("expected utxos")
	}
	for _, u := range utxos[:3] {
		t.Logf("  %s:%d value=%d conf=%d", u.Txid, u.Vout, u.Value, u.Confirmations)
	}
	_, err = c.getTipHeight()
	if err != nil {
		t.Fatalf("tip: %v", err)
	}
}
