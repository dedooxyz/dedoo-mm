package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"golang.org/x/crypto/ripemd160"
)

// testP2SHOutput is a dummy P2SH address (hash of the unspendable "52ae"
// script). It has no on-chain history and is not controlled by anyone.
const testP2SHOutput = "3CHkEXVtx484ymadFo29rhkm7e3qtpNi2T"

func TestBuildAndCosignMultisigTx(t *testing.T) {
	// Generate 3 test keys for 2-of-3
	priv1, _ := btcec.NewPrivateKey()
	priv2, _ := btcec.NewPrivateKey()
	priv3, _ := btcec.NewPrivateKey()

	pub1Hex := hex.EncodeToString(priv1.PubKey().SerializeCompressed())
	pub2Hex := hex.EncodeToString(priv2.PubKey().SerializeCompressed())
	pub3Hex := hex.EncodeToString(priv3.PubKey().SerializeCompressed())

	pubs := []string{pub1Hex, pub2Hex, pub3Hex}
	redeemHex, err := BuildMultisigRedeemScript(pubs, []int{0, 1, 2}, 2)
	if err != nil {
		t.Fatalf("BuildMultisigRedeemScript: %v", err)
	}

	mockUTXOs := []UTXO{
		{
			Txid:          "0000000000000000000000000000000000000000000000000000000000000001",
			Vout:          0,
			Value:         5000000000, // 50 WJK
			Confirmations: 25,
		},
	}

	outputs := []Output{
		{Address: testP2SHOutput, ValueSats: 4999000000},
		{OpReturnHex: "0102030405060708090a0b0c0d0e0f1011121314"},
	}

	// 1. Sign with Key 1
	txHex1, err := BuildAndSignMultisigTx(mockUTXOs, outputs, redeemHex, []*btcec.PrivateKey{priv1})
	if err != nil {
		t.Fatalf("BuildAndSignMultisigTx: %v", err)
	}

	// 2. Cosign with Key 2
	txHex2, complete, err := CosignMultisigTx(txHex1, redeemHex, []*btcec.PrivateKey{priv2}, 2)
	if err != nil {
		t.Fatalf("CosignMultisigTx: %v", err)
	}
	if !complete {
		t.Fatalf("expected tx to be complete with 2 of 3 signatures")
	}

	// 3. Signature aggregation / verification with nil signingKeys (peer receiving complete tx)
	txHexFinal, completeFinal, err := CosignMultisigTx(txHex2, redeemHex, nil, 2)
	if err != nil {
		t.Fatalf("CosignMultisigTx with nil keys: %v", err)
	}
	if !completeFinal {
		t.Fatalf("expected completeFinal to be true")
	}
	if txHexFinal != txHex2 {
		t.Fatalf("expected txHexFinal to match txHex2")
	}

	// 4. Verify script execution with btcd script engine
	redeemBytes, _ := hex.DecodeString(redeemHex)
	p2shScript, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_HASH160).
		AddData(btcutilHash160(redeemBytes)).
		AddOp(txscript.OP_EQUAL).
		Script()
	if err != nil {
		t.Fatalf("build p2sh script: %v", err)
	}

	txBytes, _ := hex.DecodeString(txHexFinal)
	var msgTx wire.MsgTx
	if err := msgTx.Deserialize(bytes.NewReader(txBytes)); err != nil {
		t.Fatalf("deserialize msgTx: %v", err)
	}

	flags := txscript.ScriptBip16 | txscript.ScriptVerifyDERSignatures
	engine, err := txscript.NewEngine(p2shScript, &msgTx, 0, flags, nil, nil, 5000000000, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if err := engine.Execute(); err != nil {
		t.Fatalf("Script execution failed: %v", err)
	}
}

func TestParseOutputsAndFingerprint(t *testing.T) {
	priv, _ := btcec.NewPrivateKey()
	pubHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())
	redeemHex, _ := BuildMultisigRedeemScript([]string{pubHex, pubHex, pubHex}, []int{0, 1, 2}, 2)

	mockUTXOs := []UTXO{
		{Txid: "0000000000000000000000000000000000000000000000000000000000000002", Vout: 1, Value: 1000000},
	}
	outputs := []Output{
		{Address: testP2SHOutput, ValueSats: 900000},
		{OpReturnHex: "aabbccddeeff00112233445566778899aabbccdd"},
	}

	txHex, err := BuildAndSignMultisigTx(mockUTXOs, outputs, redeemHex, []*btcec.PrivateKey{priv})
	if err != nil {
		t.Fatalf("BuildAndSignMultisigTx: %v", err)
	}

	parsedOutputs, err := ParseOutputsFromRawHex(txHex)
	if err != nil {
		t.Fatalf("ParseOutputsFromRawHex: %v", err)
	}
	if len(parsedOutputs) != 2 {
		t.Fatalf("expected 2 parsed outputs, got %d", len(parsedOutputs))
	}
	if parsedOutputs[1].OpReturnHex != "aabbccddeeff00112233445566778899aabbccdd" {
		t.Fatalf("OP_RETURN mismatch: got %s", parsedOutputs[1].OpReturnHex)
	}

	fp, err := TxFingerprint(txHex)
	if err != nil {
		t.Fatalf("TxFingerprint: %v", err)
	}
	if len(fp) != 64 {
		t.Fatalf("expected 64 hex char fingerprint, got %d", len(fp))
	}
}

func btcutilHash160(data []byte) []byte {
	_, h160, _ := DecodeAddress(testP2SHOutput)
	_ = h160
	// Hash160 is RIPEMD160(SHA256(data))
	// We can compute via DecodeAddress or makeOutputScript
	script, _ := makeOutputScript(testP2SHOutput)
	_ = script
	// Standard hash160 calculation
	h := sha256.Sum256(data)
	return ripemd160Hash(h[:])
}

func ripemd160Hash(b []byte) []byte {
	h := ripemd160.New()
	h.Write(b)
	return h.Sum(nil)
}
