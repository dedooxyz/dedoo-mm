package chain

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// --- Hash160 script-shape extraction ---------------------------------------

func TestHash160FromScriptShapes(t *testing.T) {
	h := Hash160([]byte("wojak"))
	if len(h) != 20 {
		t.Fatalf("hash160 length = %d", len(h))
	}

	// P2SH: a9 14 <h> 87
	p2sh := append([]byte{0xa9, 0x14}, h...)
	p2sh = append(p2sh, 0x87)
	if got := Hash160FromScript(p2sh); !bytes.Equal(got, h) {
		t.Fatalf("P2SH extract = %x, want %x", got, h)
	}

	// P2PKH: 76 a9 14 <h> 88 ac
	p2pkh := append([]byte{0x76, 0xa9, 0x14}, h...)
	p2pkh = append(p2pkh, 0x88, 0xac)
	if got := Hash160FromScript(p2pkh); !bytes.Equal(got, h) {
		t.Fatalf("P2PKH extract = %x, want %x", got, h)
	}

	// Non-standard shapes return nil.
	cases := []string{"P2SH wrong length", "OP_RETURN", "P2SH wrong prefix", "P2SH wrong suffix", "P2PKH wrong suffix", "P2WPKH (22 bytes)", "empty", "hash only"}
	scripts := [][]byte{
		{0xa9, 0x14, 0x01, 0x87},
		{0x6a, 0x14},
		append(append([]byte{0xa8, 0x14}, h...), 0x87),
		append(append([]byte{0xa9, 0x14}, h...), 0x88),
		append(append([]byte{0x76, 0xa9, 0x14}, h...), 0x80, 0xac),
		append([]byte{0x00, 0x14}, h...),
		nil,
		append([]byte{}, h...),
	}
	for i, name := range cases {
		if got := Hash160FromScript(scripts[i]); got != nil {
			t.Fatalf("%s: expected nil, got %x", name, got)
		}
	}
}

func TestRedeemScriptPubKeyHash(t *testing.T) {
	redeem, err := BuildMultisigRedeemScript([]string{testPub(t, 1), testPub(t, 2), testPub(t, 3)}, []int{0, 1, 2}, 2)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(redeem)
	if err != nil {
		t.Fatal(err)
	}
	h160, err := RedeemScriptPubKeyHash(redeem)
	if err != nil {
		t.Fatalf("RedeemScriptPubKeyHash: %v", err)
	}
	if !bytes.Equal(h160, Hash160(raw)) {
		t.Fatal("redeem hash160 mismatch with Hash160(raw redeem)")
	}
	// The P2SH script encoding that hash160 must round-trip.
	spk := append([]byte{0xa9, 0x14}, h160...)
	spk = append(spk, 0x87)
	if got := Hash160FromScript(spk); !bytes.Equal(got, h160) {
		t.Fatal("vault P2SH script must round-trip through Hash160FromScript")
	}
	if _, err := RedeemScriptPubKeyHash("this is not hex"); err == nil {
		t.Fatal("expected error for invalid redeem hex")
	}

	// The derived P2SH address must decode back to the same hash160.
	addr, err := P2SHAddressFromRedeemScript(redeem)
	if err != nil {
		t.Fatalf("P2SHAddressFromRedeemScript: %v", err)
	}
	ver, payload, err := DecodeAddress(addr)
	if err != nil || ver != AddrScriptVersion || !bytes.Equal(payload, h160) {
		t.Fatalf("derived address %q invalid (ver=%d err=%v)", addr, ver, err)
	}
	if _, err := P2SHAddressFromRedeemScript("zz"); err == nil {
		t.Fatal("expected error for invalid redeem hex")
	}

	// Threshold parser must read M from the same script.
	m, err := ParseMultisigThreshold(redeem)
	if err != nil || m != 2 {
		t.Fatalf("ParseMultisigThreshold = %d, %v; want 2", m, err)
	}
	if _, err := ParseMultisigThreshold("aa"); err == nil {
		t.Fatal("expected error for non-multisig script")
	}
}

// --- fake Esplora server ---------------------------------------------------

// esploraVout is one confirmed/confirmed-pending output of a fake tx.
type esploraVout struct {
	value     int64
	scriptHex string
}

// fakeEsplora serves the three endpoints getTxPrevOut touches:
// /blocks/tip/height, /tx/<txid>, /tx/<txid>/outspends.
type fakeEsplora struct {
	tip       int64
	txs       map[string]*esploraTx
	outspends map[string][]bool // per-vout spent flags
}

// esploraTx is one transaction record.
type esploraTx struct {
	vouts       []esploraVout
	blockHeight int64 // 0 -> unconfirmed
}

func (f *fakeEsplora) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/blocks/tip/height":
			fmt.Fprintln(w, f.tip)
			return
		case strings.HasPrefix(path, "/tx/") && strings.HasSuffix(path, "/outspends"):
			txid := strings.TrimSuffix(strings.TrimPrefix(path, "/tx/"), "/outspends")
			spent, ok := f.outspends[txid]
			if !ok {
				http.Error(w, "unknown tx", http.StatusNotFound)
				return
			}
			outs := make([]map[string]bool, len(spent))
			for i, s := range spent {
				outs[i] = map[string]bool{"spent": s}
			}
			_ = json.NewEncoder(w).Encode(outs)
			return
		case strings.HasPrefix(path, "/tx/"):
			tx := f.txs[strings.TrimPrefix(path, "/tx/")]
			if tx == nil {
				http.Error(w, "unknown tx", http.StatusNotFound)
				return
			}
			vouts := make([]map[string]interface{}, len(tx.vouts))
			for i, v := range tx.vouts {
				vouts[i] = map[string]interface{}{
					"value":        v.value,
					"scriptpubkey": v.scriptHex, // standard Esplora: hex string
				}
			}
			body := map[string]interface{}{"vout": vouts}
			if tx.blockHeight > 0 {
				body["status"] = map[string]interface{}{"confirmed": true, "block_height": tx.blockHeight}
			} else {
				body["status"] = map[string]interface{}{"confirmed": false}
			}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
}

// newFakeEsplora boots a one-node failover client backed by the fake server.
func newFakeEsplora(t *testing.T) (*FailoverElectrsClient, *fakeEsplora) {
	t.Helper()
	f := &fakeEsplora{
		tip:       1000,
		txs:       map[string]*esploraTx{},
		outspends: map[string][]bool{},
	}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	fc, err := NewFailoverElectrsClient([]string{srv.URL}, 2*time.Second)
	if err != nil {
		t.Fatalf("NewFailoverElectrsClient: %v", err)
	}
	return fc, f
}

// vaultScript builds the P2SH locking script for a redeem script hex.
func vaultScript(t *testing.T, redeemHex string) []byte {
	t.Helper()
	h160, err := RedeemScriptPubKeyHash(redeemHex)
	if err != nil {
		t.Fatal(err)
	}
	spk := append([]byte{0xa9, 0x14}, h160...)
	return append(spk, 0x87)
}

// addUtxo registers a funding transaction with one output (txid:0).
func (f *fakeEsplora) addUtxo(txid, scriptHex string, value, blockHeight int64) {
	f.txs[txid] = &esploraTx{
		vouts:       []esploraVout{{value: value, scriptHex: scriptHex}},
		blockHeight: blockHeight,
	}
	f.outspends[txid] = []bool{false}
}

// addTx registers an empty funding transaction and returns its record so
// callers can append further vouts.
func (f *fakeEsplora) addTx(txid string, blockHeight int64) *esploraTx {
	tx := &esploraTx{blockHeight: blockHeight}
	f.txs[txid] = tx
	f.outspends[txid] = []bool{}
	return tx
}

func fakeTxid(seed byte) string {
	var h [32]byte
	for i := range h {
		h[i] = seed + byte(i)
	}
	rev := make([]byte, len(h))
	for i := range h {
		rev[len(h)-1-i] = h[i]
	}
	return hex.EncodeToString(rev) // little-endian display form
}

// spendTxHex hex-serializes a legacy transaction spending one output (vout 0)
// of each funding txid and paying one OP_RETURN output.
func spendTxHex(t *testing.T, funding []string) string {
	t.Helper()
	tx := wire.NewMsgTx(wire.TxVersion)
	for _, txid := range funding {
		hash, err := chainhash.NewHashFromStr(txid)
		if err != nil {
			t.Fatalf("funding txid: %v", err)
		}
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{Hash: *hash, Index: 0},
			SignatureScript:  []byte{0x00}, // dummy unlock
			Sequence:         wire.MaxTxInSequenceNum,
		})
	}
	tx.AddTxOut(wire.NewTxOut(100, []byte{0x6a, 0x00}))
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return hex.EncodeToString(buf.Bytes())
}

// --- GetTxPrevOut mapping --------------------------------------------------

func TestGetTxPrevOutFakeEsplora(t *testing.T) {
	redeem, err := BuildMultisigRedeemScript([]string{testPub(t, 1), testPub(t, 2), testPub(t, 3)}, []int{0, 1, 2}, 2)
	if err != nil {
		t.Fatal(err)
	}
	vspk := hex.EncodeToString(vaultScript(t, redeem))

	fc, f := newFakeEsplora(t)
	mature := fakeTxid(0x10)
	f.addUtxo(mature, vspk, 30_000, 993) // 1000-993+1 = 8 confs
	unconf := fakeTxid(0x30)
	f.addUtxo(unconf, vspk, 10_000, 0)
	spent := fakeTxid(0x50)
	f.addUtxo(spent, vspk, 40_000, 993)
	f.outspends[spent] = []bool{true}

	t.Run("mature confirms and maps value", func(t *testing.T) {
		po, err := fc.GetTxPrevOut(mature, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if po.ValueSats != 30_000 || po.Confirmations != 8 {
			t.Fatalf("got %+v, want value=30000 confs=8", po)
		}
		if po.ScriptHex != vspk {
			t.Fatal("script hex mismatch")
		}
	})
	t.Run("unconfirmed has zero confs", func(t *testing.T) {
		po, err := fc.GetTxPrevOut(unconf, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if po.Confirmations != 0 {
			t.Fatalf("unconfirmed confs = %d, want 0", po.Confirmations)
		}
	})
	t.Run("unknown txid fails closed", func(t *testing.T) {
		if _, err := fc.GetTxPrevOut(fakeTxid(0xEE), 0); err == nil {
			t.Fatal("expected error for unknown txid")
		}
	})
	t.Run("spent outspend fails closed", func(t *testing.T) {
		if _, err := fc.GetTxPrevOut(spent, 0); err == nil || !strings.Contains(err.Error(), "already spent") {
			t.Fatalf("expected 'already spent' error, got %v", err)
		}
	})
	t.Run("vout out of range", func(t *testing.T) {
		if _, err := fc.GetTxPrevOut(mature, 1); err == nil || !strings.Contains(err.Error(), "no vout 1") {
			t.Fatalf("expected vout OOB error, got %v", err)
		}
	})
}

// --- VerifyProposalInputs ---------------------------------------------------

func TestVerifyProposalInputs(t *testing.T) {
	keys := []string{testPub(t, 1), testPub(t, 2), testPub(t, 3)}
	redeem, err := BuildMultisigRedeemScript(keys, []int{0, 1, 2}, 2)
	if err != nil {
		t.Fatal(err)
	}
	other, err := BuildMultisigRedeemScript(keys, []int{0, 1, 2}, 1) // different layout
	if err != nil {
		t.Fatal(err)
	}
	vspk := hex.EncodeToString(vaultScript(t, redeem))
	ospk := hex.EncodeToString(vaultScript(t, other))

	t.Run("happy path sums vault inputs", func(t *testing.T) {
		fc, f := newFakeEsplora(t)
		id1, id2 := fakeTxid(0x01), fakeTxid(0x02)
		f.addUtxo(id1, vspk, 30_000, 993)
		f.addUtxo(id2, vspk, 40_000, 993)
		sum, err := VerifyProposalInputs(fc, spendTxHex(t, []string{id1, id2}), redeem, 6)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if sum != 70_000 {
			t.Fatalf("input sum = %d, want 70000", sum)
		}
	})

	t.Run("foreign script rejected", func(t *testing.T) {
		fc, f := newFakeEsplora(t)
		id := fakeTxid(0x03)
		f.addUtxo(id, ospk, 30_000, 993)
		_, err := VerifyProposalInputs(fc, spendTxHex(t, []string{id}), redeem, 6)
		if err == nil || !strings.Contains(err.Error(), "different script") {
			t.Fatalf("expected foreign-script error, got %v", err)
		}
	})

	t.Run("immature rejected", func(t *testing.T) {
		fc, f := newFakeEsplora(t)
		id := fakeTxid(0x04)
		f.addUtxo(id, vspk, 30_000, 999) // 2 confs < 6
		_, err := VerifyProposalInputs(fc, spendTxHex(t, []string{id}), redeem, 6)
		if err == nil || !strings.Contains(err.Error(), "immature") {
			t.Fatalf("expected immature error, got %v", err)
		}
	})

	t.Run("spent input rejected", func(t *testing.T) {
		fc, f := newFakeEsplora(t)
		id := fakeTxid(0x05)
		f.addUtxo(id, vspk, 30_000, 993)
		f.outspends[id] = []bool{true}
		_, err := VerifyProposalInputs(fc, spendTxHex(t, []string{id}), redeem, 6)
		if err == nil || !strings.Contains(err.Error(), "already spent") {
			t.Fatalf("expected spent error, got %v", err)
		}
	})

	t.Run("unknown input tx rejected", func(t *testing.T) {
		fc, _ := newFakeEsplora(t)
		txid := fakeTxid(0x06) // never registered
		_, err := VerifyProposalInputs(fc, spendTxHex(t, []string{txid}), redeem, 6)
		if err == nil || !strings.Contains(err.Error(), txid) {
			t.Fatalf("expected error naming missing txid, got %v", err)
		}
	})

	t.Run("second vout of same tx respected", func(t *testing.T) {
		// A funding tx with TWO outputs: vout 0 vault, vout 1 foreign.
		// VerifyProposalInputs must accept an input referencing vout 0 and
		// reject one referencing vout 1 — the fake now gets multi-vout data.
		fc, f := newFakeEsplora(t)
		id := fakeTxid(0x07)
		tx := f.addTx(id, 993)
		tx.vouts = []esploraVout{
			{value: 30_000, scriptHex: vspk},
			{value: 5_000, scriptHex: ospk},
		}
		f.outspends[id] = []bool{false, false}

		// Spend vout 0: accepted.
		sum, err := VerifyProposalInputs(fc, spendVoutHex(t, id, 0), redeem, 6)
		if err != nil {
			t.Fatalf("vout 0 must verify: %v", err)
		}
		if sum != 30_000 {
			t.Fatalf("sum = %d, want 30000", sum)
		}
		// Spend vout 1 (foreign): rejected.
		if _, err := VerifyProposalInputs(fc, spendVoutHex(t, id, 1), redeem, 6); err == nil ||
			!strings.Contains(err.Error(), "different script") {
			t.Fatalf("expected foreign-script error for vout 1, got %v", err)
		}
		// A spent second outspend is also rejected even from the vault vout.
		f.outspends[id] = []bool{true, false}
		if _, err := VerifyProposalInputs(fc, spendVoutHex(t, id, 0), redeem, 6); err == nil ||
			!strings.Contains(err.Error(), "already spent") {
			t.Fatalf("expected spent error, got %v", err)
		}
	})

	// Note: the "proposal has no inputs" branch of VerifyProposalInputs is
	// defensive only — btcd's Deserialize rejects any inputless tx (legacy and
	// segwit encodings both fail), so a parsed raw hex always yields >=1 input.

	t.Run("malformed raw hex rejected", func(t *testing.T) {
		fc, _ := newFakeEsplora(t)
		_, err := VerifyProposalInputs(fc, "zzzz", redeem, 6)
		if err == nil || !strings.Contains(err.Error(), "parse tx inputs") {
			t.Fatalf("expected parse error, got %v", err)
		}
	})
}

// spendVoutHex builds a tx spending exactly txid:vout (vout varying).
func spendVoutHex(t *testing.T, txid string, vout uint32) string {
	t.Helper()
	hash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		t.Fatalf("txid: %v", err)
	}
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: *hash, Index: vout},
		SignatureScript:  []byte{0x00},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	tx.AddTxOut(wire.NewTxOut(100, []byte{0x6a, 0x00}))
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return hex.EncodeToString(buf.Bytes())
}
