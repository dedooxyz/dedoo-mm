package chain

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcutil/base58"
)

// withProfile selects p for one test and restores WojakCoin afterwards.
func withProfile(t *testing.T, p NetParams) {
	t.Helper()
	SelectNetParams(p)
	t.Cleanup(func() { SelectNetParams(WojakCoinParams) })
}

func TestLookupNetParams(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"wojakcoin", "wojakcoin"},
		{"WOJAKCOIN", "wojakcoin"},
		{"wojak", "wojakcoin"},
		{"wjk", "wojakcoin"},
		{"mainnet", "wojakcoin"}, // legacy CHAIN value
		{"junkcoin", "junkcoin"},
		{"JunkCoin", "junkcoin"},
		{"junk", "junkcoin"},
		{"jkc", "junkcoin"},
	}
	for _, tc := range cases {
		p, err := LookupNetParams(tc.in)
		if err != nil {
			t.Fatalf("LookupNetParams(%q): %v", tc.in, err)
		}
		if p.Name != tc.want {
			t.Fatalf("LookupNetParams(%q) = %q, want %q", tc.in, p.Name, tc.want)
		}
	}
	if _, err := LookupNetParams("dogecoin"); err == nil {
		t.Fatal("expected error for unsupported chain")
	}
}

func TestProfileConstants(t *testing.T) {
	if WojakCoinParams.AddrPubkeyVersion != 73 ||
		WojakCoinParams.AddrScriptVersion != 5 ||
		WojakCoinParams.WifVersion != 201 ||
		WojakCoinParams.CoinbaseMaturity != 20 ||
		WojakCoinParams.DefaultP2PPort != 20759 {
		t.Fatalf("WojakCoin profile drifted: %+v", WojakCoinParams)
	}
	if WojakCoinParams.DefaultElectrsURL != "https://wojak-api.s3na.xyz" {
		t.Fatalf("WojakCoin endpoint default drifted: %+v", WojakCoinParams)
	}

	if JunkcoinParams.AddrPubkeyVersion != 16 ||
		JunkcoinParams.AddrScriptVersion != 5 ||
		JunkcoinParams.WifVersion != 144 ||
		JunkcoinParams.CoinbaseMaturity != 70 ||
		JunkcoinParams.DefaultP2PPort != 9771 ||
		JunkcoinParams.DefaultElectrsURL != "https://junk-api.s3na.xyz" {
		t.Fatalf("Junkcoin profile drifted: %+v", JunkcoinParams)
	}
}

// TestJunkcoinAddressFixture decodes a test-only Junkcoin address to pin the
// version byte and the roundtrip encoding.
func TestJunkcoinAddressFixture(t *testing.T) {
	withProfile(t, JunkcoinParams)
	const (
		addr = "7VX8ctLrBuowG8KLCJWQh2KptUMgkakeuY"
		h160 = "2222222222222222222222222222222222222222"
	)
	ver, payload, err := DecodeAddress(addr)
	if err != nil {
		t.Fatalf("DecodeAddress: %v", err)
	}
	if ver != 16 {
		t.Fatalf("version = %d, want 16 (JKC P2PKH)", ver)
	}
	if got := hex.EncodeToString(payload); got != h160 {
		t.Fatalf("payload = %s, want %s", got, h160)
	}
	if round := EncodeAddress(ver, payload); round != addr {
		t.Fatalf("roundtrip = %q, want %q", round, addr)
	}
}

func TestAddressEncodingPerProfile(t *testing.T) {
	h := Hash160([]byte("profile-test"))
	withProfile(t, JunkcoinParams)
	jkcAddr := EncodeAddress(JunkcoinParams.AddrPubkeyVersion, h)
	if !strings.HasPrefix(jkcAddr, "7") {
		t.Fatalf("JKC P2PKH should start with 7, got %q", jkcAddr)
	}

	withProfile(t, WojakCoinParams)
	wjkAddr := EncodeAddress(WojakCoinParams.AddrPubkeyVersion, h)
	if jkcAddr == wjkAddr {
		t.Fatal("P2PKH encodings must differ between profiles")
	}

	// P2SH version is shared (5) — same redeem script, same vault address on
	// both chains. This is why explicit vault configuration still matters.
	withProfile(t, JunkcoinParams)
	jkcScript := EncodeAddress(JunkcoinParams.AddrScriptVersion, h)
	withProfile(t, WojakCoinParams)
	wjkScript := EncodeAddress(WojakCoinParams.AddrScriptVersion, h)
	if jkcScript != wjkScript {
		t.Fatalf("P2SH encoding should be identical across profiles: %q vs %q", jkcScript, wjkScript)
	}
}

func TestWifPerProfile(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	payload := append(priv.Serialize(), 0x01) // compressed-key marker
	jkcWif := base58.CheckEncode(payload, JunkcoinParams.WifVersion)
	wjkWif := base58.CheckEncode(payload, WojakCoinParams.WifVersion)

	withProfile(t, JunkcoinParams)
	got, compressed, err := LoadWifKey(jkcWif)
	if err != nil {
		t.Fatalf("JKC WIF rejected under JKC profile: %v", err)
	}
	if !compressed || hex.EncodeToString(got.Serialize()) != hex.EncodeToString(priv.Serialize()) {
		t.Fatal("JKC WIF roundtrip mismatch")
	}
	if _, _, err := LoadWifKey(wjkWif); err == nil || !strings.Contains(err.Error(), "WIF version") {
		t.Fatalf("WJK WIF must be rejected under JKC profile, got %v", err)
	}

	withProfile(t, WojakCoinParams)
	if _, _, err := LoadWifKey(jkcWif); err == nil || !strings.Contains(err.Error(), "WIF version") {
		t.Fatalf("JKC WIF must be rejected under WJK profile, got %v", err)
	}
	if _, _, err := LoadWifKey(wjkWif); err != nil {
		t.Fatalf("WJK WIF rejected under WJK profile: %v", err)
	}
}

func TestCurrentNetParams(t *testing.T) {
	withProfile(t, JunkcoinParams)
	if p := CurrentNetParams(); p.Name != "junkcoin" {
		t.Fatalf("current = %q, want junkcoin", p.Name)
	}
	withProfile(t, WojakCoinParams)
	if p := CurrentNetParams(); p.Name != "wojakcoin" {
		t.Fatalf("current = %q, want wojakcoin", p.Name)
	}
}
