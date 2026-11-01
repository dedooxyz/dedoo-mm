package processor

import (
	"strings"
	"testing"

	"wojak-keeper/internal/chain"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcutil/base58"
)

func wifForKey(t *testing.T, priv *btcec.PrivateKey, version byte) string {
	t.Helper()
	payload := append(priv.Serialize(), 0x01) // compressed pubkey marker
	return base58.CheckEncode(payload, version)
}

// TestJunkcoinProfileGuards runs the keeper under the Junkcoin profile and
// checks that both WIF and destination addresses are validated against it.
func TestJunkcoinProfileGuards(t *testing.T) {
	chain.SelectNetParams(chain.JunkcoinParams)
	t.Cleanup(func() { chain.SelectNetParams(chain.WojakCoinParams) })

	keys := newTrioKeys(t)
	cfg := baseTestCfg(0, keys.vaultAddr, false, "")
	cfg.CoinbaseMaturity = chain.JunkcoinParams.CoinbaseMaturity
	cfg.ElectrsURL = "http://127.0.0.1:1" // lazy client; never dialed in this test
	cfg.KeeperWif = wifForKey(t, keys.privs[0], chain.JunkcoinParams.WifVersion)

	k, err := NewKeeper(cfg)
	if err != nil {
		t.Fatalf("NewKeeper with JKC WIF: %v", err)
	}
	k.SetRedeemScript(keys.redeem)

	// Mock chain with one mature (>=70 confs) vault UTXO.
	mc := newMockChain()
	spk := p2shSpkHex(keys.redeem)
	txid := strings.Repeat("a", 64)
	mc.SetUTXOs(keys.vaultAddr, []chain.UTXO{{Txid: txid, Vout: 0, Value: 50_000_000, Confirmations: 100}})
	mc.PrevOut(txid, 0, 50_000_000, spk, 100)
	k.chain = mc

	// A WojakCoin P2PKH destination (version 73) is foreign on Junkcoin.
	if _, err := k.ProposeTransfer(keys.vaultAddr, canonicalDest, 20_000_000, "cross-chain"); err == nil ||
		!strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("WJK destination must be rejected under JKC profile, got %v", err)
	}

	// A Junkcoin P2PKH destination (version 16) is accepted.
	jkcDest := chain.EncodeAddress(chain.JunkcoinParams.AddrPubkeyVersion, chain.Hash160([]byte("junkcoin-destination")))
	id, err := k.ProposeTransfer(keys.vaultAddr, jkcDest, 20_000_000, "jkc transfer")
	if err != nil {
		t.Fatalf("JKC destination should be accepted: %v", err)
	}
	v, err := k.GetProposal(id)
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}
	if v.To != jkcDest {
		t.Fatalf("proposal destination = %q, want %q", v.To, jkcDest)
	}
}

// TestWojakProfileRejectsJunkcoinWif makes sure the WIF version guard still
// protects the default (WojakCoin) profile.
func TestWojakProfileRejectsJunkcoinWif(t *testing.T) {
	chain.SelectNetParams(chain.WojakCoinParams)

	keys := newTrioKeys(t)
	cfg := baseTestCfg(0, keys.vaultAddr, false, "")
	cfg.ElectrsURL = "http://127.0.0.1:1"
	cfg.KeeperWif = wifForKey(t, keys.privs[0], chain.JunkcoinParams.WifVersion)

	_, err := NewKeeper(cfg)
	if err == nil || !strings.Contains(err.Error(), "WIF version") {
		t.Fatalf("expected JKC WIF to be rejected under WJK profile, got %v", err)
	}
}
