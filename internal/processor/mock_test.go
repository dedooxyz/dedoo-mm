package processor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dedooxyz/dedoo-mm/internal/chain"
	"github.com/dedooxyz/dedoo-mm/internal/config"

	"github.com/btcsuite/btcd/btcec/v2"
	"golang.org/x/crypto/ripemd160"
)

// --- trio helpers -------------------------------------------------------

type trioKeys struct {
	privs     [3]*btcec.PrivateKey
	pubs      [3]string
	redeem    string
	vaultAddr string
}

func newTrioKeys(t *testing.T) trioKeys {
	t.Helper()
	var k trioKeys
	for i := 0; i < 3; i++ {
		priv, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		k.privs[i] = priv
		k.pubs[i] = hexEncode(priv.PubKey().SerializeCompressed())
	}
	var err error
	k.redeem, err = chain.BuildMultisigRedeemScript([]string{k.pubs[0], k.pubs[1], k.pubs[2]}, []int{0, 1, 2}, 2)
	if err != nil {
		t.Fatalf("BuildMultisigRedeemScript: %v", err)
	}
	k.vaultAddr = p2shFromRedeem(k.redeem)
	return k
}

func p2shFromRedeem(redeemHex string) string {
	b := hexDecode(redeemHex)
	s := sha256Sum(b)
	r := ripemd160.New()
	r.Write(s)
	h160 := r.Sum(nil)
	return chain.EncodeAddress(chain.AddrScriptVersion, h160)
}

// p2shSpkHex builds the locking-script hex for a P2SH(vault) utxo.
func p2shSpkHex(redeemHex string) string {
	addr := p2shFromRedeem(redeemHex)
	_, h160, err := chain.DecodeAddress(addr)
	if err != nil {
		return ""
	}
	hex160 := hexEncode(h160)
	return "a914" + hex160 + "87"
}

// --- mock chain backend -------------------------------------------------------

type mockChain struct {
	mu        sync.Mutex
	prevs     map[string]chain.PrevOut // keyed "txid:vout"
	utxos     map[string][]chain.UTXO  // by address
	bcastLog  []string
	bcastErr  error
	inputFees map[int64]int64 // deterministic fee per input count
}

func newMockChain() *mockChain {
	return &mockChain{
		prevs: map[string]chain.PrevOut{},
		utxos: map[string][]chain.UTXO{},
	}
}

func prevKey(txid string, vout uint32) string { return fmt.Sprintf("%s:%d", txid, vout) }

func (m *mockChain) PrevOut(txid string, vout uint32, valueSats int64, spkHex string, confs int64) {
	m.mu.Lock()
	m.prevs[prevKey(txid, vout)] = chain.PrevOut{ValueSats: valueSats, ScriptHex: spkHex, Confirmations: confs}
	m.mu.Unlock()
}

func (m *mockChain) SetUTXOs(addr string, utxos []chain.UTXO) {
	m.mu.Lock()
	m.utxos[addr] = utxos
	m.mu.Unlock()
}

func (m *mockChain) GetTipHeight() (int64, error) { return 1000, nil }
func (m *mockChain) GetTipHash() (string, error)  { return "mock-tip-hash", nil }

func (m *mockChain) GetUTXOs(address string, minConfirmations int64) ([]chain.UTXO, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []chain.UTXO
	for _, u := range m.utxos[address] {
		if u.Confirmations >= minConfirmations {
			out = append(out, u)
		}
	}
	return out, nil
}

func (m *mockChain) GetAddressStats(address string) (*chain.AddressStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := &chain.AddressStats{}
	for _, u := range m.utxos[address] {
		st.UnspentCount++
		st.UnspentValue += u.Value
	}
	return st, nil
}

func (m *mockChain) GetTxPrevOut(txid string, vout uint32) (*chain.PrevOut, error) {
	m.mu.Lock()
	p, ok := m.prevs[prevKey(txid, vout)]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("utxo %s:%d not found", txid, vout)
	}
	return &p, nil
}

func (m *mockChain) EstimateFeeSatsWithMin(nInputs int, nOutputs int, targetBlocks int, minSatPerVB int64) int64 {
	if m.inputFees != nil {
		m.mu.Lock()
		v, ok := m.inputFees[int64(nInputs)]
		m.mu.Unlock()
		if ok {
			return v
		}
	}
	return int64(nInputs*220 + nOutputs*34 + 10)
}

func (m *mockChain) Broadcast(rawHex string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bcastErr != nil {
		return "", m.bcastErr
	}
	m.bcastLog = append(m.bcastLog, rawHex)
	return fmt.Sprintf("mocktx-%d", len(m.bcastLog)), nil
}

func (m *mockChain) broadcasts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.bcastLog))
	copy(out, m.bcastLog)
	return out
}

// --- keeper construction -------------------------------------------------

func newTestKeeper(t *testing.T, cfg *config.Config, mc chain.ChainBackend, priv *btcec.PrivateKey, pubHex, redeem string) *Keeper {
	t.Helper()
	k := &Keeper{
		cfg:     cfg,
		chain:   mc,
		priv:    priv,
		pubHex:  pubHex,
		props:   make(map[string]*propState),
		reqLog:  make(map[string]time.Time),
		wallets: make(map[string]*wallet),
		keys:    make(map[string]*signingKey),
	}
	k.keys[pubHex] = &signingKey{priv: priv, env: true, added: time.Now()}
	p := cfg.NetParams
	if p.Name == "" {
		p = chain.WojakCoinParams
	}
	k.wallets[cfg.VaultAddress] = &wallet{view: AccountView{
		Id:                "active",
		Name:              "Active signing vault",
		Chain:             cfg.ChainName,
		ChainKey:          chainKeyFor(p),
		Address:           cfg.VaultAddress,
		RedeemScript:      redeem,
		Required:          cfg.RequiredSigs,
		AddrPubkeyVersion: int(p.AddrPubkeyVersion),
		AddrScriptVersion: int(p.AddrScriptVersion),
		WifVersion:        int(p.WifVersion),
		CoinbaseMaturity:  cfg.CoinbaseMaturity,
		ElectrsURL:        cfg.ElectrsURL,
		Active:            true,
	}}
	k.loadKeys()
	k.loadAccounts()
	return k
}

func baseTestCfg(selfIndex int, vaultAddr string, autoSign bool, stateFile string) *config.Config {
	return &config.Config{
		Mode:             config.ModeKeeper,
		SelfIndex:        selfIndex,
		Backend:          "mock",
		ChainName:        "wojakcoin",
		NetParams:        chain.WojakCoinParams,
		P2PGroup:         "test",
		CoinbaseMaturity: 20,
		VaultAddress:     vaultAddr,
		MaxTxInputs:      100,
		RequiredSigs:     2,
		AutoSign:         autoSign,
		AutoBroadcast:    false,
		StateFile:        stateFile,
		MinTransferSats:  10000,
	}
}

// vaultFixture installs 2 mature vault utxos backed by mock prevouts.
func vaultFixture(t *testing.T, mc *mockChain, redeem, vaultAddr string) []chain.UTXO {
	t.Helper()
	spk := p2shSpkHex(redeem)
	utxos := []chain.UTXO{
		{Txid: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Vout: 0, Value: 50_000_000, Confirmations: 40},
		{Txid: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Vout: 1, Value: 30_000_000, Confirmations: 30},
	}
	mc.SetUTXOs(vaultAddr, utxos)
	for _, u := range utxos {
		mc.PrevOut(u.Txid, u.Vout, u.Value, spk, u.Confirmations)
	}
	return utxos
}

// canonicalDest is a canonical WojakCoin P2PKH address (version 73).
var canonicalDest = chain.EncodeAddress(chain.AddrPubkeyVersion, chain.Hash160([]byte("dedoo-mm-test-destination")))

func waitFor(tries int, cond func() bool) bool {
	for i := 0; i < tries; i++ {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// small byte/format helpers shared by the tests below
func hexEncode(b []byte) string { return fmt.Sprintf("%x", b) }

func hexDecode(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}
