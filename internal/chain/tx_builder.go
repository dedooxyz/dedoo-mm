package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	btcececdsa "github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/v2"
	chainhashv2 "github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcutil/base58"
)

var (
	// AddrPubkeyVersion, AddrScriptVersion and WifVersion live in params.go so
	// a chain profile can be selected at startup (CHAIN=wojakcoin|junkcoin).
	DustSats = getInt64Env("DUST_SATS", 10000)
)

func getInt64Env(key string, defaultVal int64) int64 {
	str := os.Getenv(key)
	if str == "" {
		return defaultVal
	}
	val, err := strconv.ParseInt(str, 10, 64)
	if err != nil {
		return defaultVal
	}
	return val
}

type Input struct {
	Txid string `json:"txid"`
	Vout uint32 `json:"vout"`
}

type Output struct {
	ValueSats   int64  `json:"value_sats"`
	Address     string `json:"address,omitempty"`
	OpReturnHex string `json:"op_return_hex,omitempty"`
}

func DecodeAddress(addr string) (byte, []byte, error) {
	decoded, version, err := base58.CheckDecode(addr)
	if err != nil {
		return 0, nil, err
	}
	return version, decoded, nil
}

func EncodeAddress(version byte, hash160 []byte) string {
	return base58.CheckEncode(hash160, version)
}

// LoadWifKey decodes a WIF-encoded private key with a configurable version byte.
// Supports custom WIF version bytes (JKC uses 0x90 = 144 by default).
func LoadWifKey(wifStr string) (*btcec.PrivateKey, bool, error) {
	decoded, version, err := base58.CheckDecode(wifStr)
	if err != nil {
		return nil, false, fmt.Errorf("WIF base58 decode: %w", err)
	}
	if version != WifVersion {
		return nil, false, fmt.Errorf("WIF version byte %d, want %d", version, WifVersion)
	}
	compressed := false
	keyBytes := decoded
	if len(decoded) == 33 && decoded[32] == 0x01 {
		compressed = true
		keyBytes = decoded[:32]
	}
	if len(keyBytes) != 32 {
		return nil, false, fmt.Errorf("WIF key data length %d, want 32", len(keyBytes))
	}
	privKey, _ := btcec.PrivKeyFromBytes(keyBytes)
	return privKey, compressed, nil
}

func ParsePubkeysFromRedeem(redeemBytes []byte) [][]byte {
	var pubkeys [][]byte
	i := 1
	for i < len(redeemBytes) {
		length := int(redeemBytes[i])
		if length == 33 || length == 65 {
			if i+1+length <= len(redeemBytes) {
				pubkeys = append(pubkeys, redeemBytes[i+1:i+1+length])
				i += 1 + length
			} else {
				break
			}
		} else {
			i++
		}
	}
	return pubkeys
}

func makeOutputScript(addr string) ([]byte, error) {
	ver, hash160, err := DecodeAddress(addr)
	if err != nil {
		return nil, err
	}

	if ver == AddrScriptVersion {
		// P2SH: OP_HASH160 <hash160> OP_EQUAL
		return txscript.NewScriptBuilder().
			AddOp(txscript.OP_HASH160).
			AddData(hash160).
			AddOp(txscript.OP_EQUAL).
			Script()
	}

	// P2PKH: OP_DUP OP_HASH160 <hash160> OP_EQUALVERIFY OP_CHECKSIG
	return txscript.NewScriptBuilder().
		AddOp(txscript.OP_DUP).
		AddOp(txscript.OP_HASH160).
		AddData(hash160).
		AddOp(txscript.OP_EQUALVERIFY).
		AddOp(txscript.OP_CHECKSIG).
		Script()
}

func makeOpReturnScript(dataHex string) ([]byte, error) {
	dataHex = strings.TrimPrefix(dataHex, "0x")
	data, err := hex.DecodeString(dataHex)
	if err != nil {
		return nil, err
	}
	return txscript.NewScriptBuilder().
		AddOp(txscript.OP_RETURN).
		AddData(data).
		Script()
}

func ParseInputsFromRawHex(rawHex string) ([]Input, error) {
	txBytes, err := hex.DecodeString(rawHex)
	if err != nil {
		return nil, err
	}
	var msgTx wire.MsgTx
	if err := msgTx.Deserialize(bytes.NewReader(txBytes)); err != nil {
		return nil, err
	}
	inputs := make([]Input, len(msgTx.TxIn))
	for i, in := range msgTx.TxIn {
		inputs[i] = Input{
			Txid: in.PreviousOutPoint.Hash.String(),
			Vout: in.PreviousOutPoint.Index,
		}
	}
	return inputs, nil
}

func ParseOutputsFromRawHex(rawHex string) ([]Output, error) {
	txBytes, err := hex.DecodeString(rawHex)
	if err != nil {
		return nil, err
	}
	var msgTx wire.MsgTx
	if err := msgTx.Deserialize(bytes.NewReader(txBytes)); err != nil {
		return nil, err
	}
	outputs := make([]Output, len(msgTx.TxOut))
	for i, out := range msgTx.TxOut {
		val := out.Value
		spk := out.PkScript

		var addr string
		var opReturnHex string

		if len(spk) > 0 && spk[0] == 0x6a {
			if pushes, err := txscript.PushedData(spk); err == nil && len(pushes) > 0 {
				var allData []byte
				for _, p := range pushes {
					allData = append(allData, p...)
				}
				opReturnHex = hex.EncodeToString(allData)
			}
		} else {
			// Local copy: never mutate the shared chaincfg.MainNetParams global.
			params := chaincfg.MainNetParams
			params.PubKeyHashAddrID = AddrPubkeyVersion
			params.ScriptHashAddrID = AddrScriptVersion

			_, addrs, _, err := txscript.ExtractPkScriptAddrs(spk, &params)
			if err == nil && len(addrs) > 0 {
				addr = addrs[0].EncodeAddress()
			}
		}

		outputs[i] = Output{
			ValueSats:   val,
			Address:     addr,
			OpReturnHex: opReturnHex,
		}
	}
	return outputs, nil
}

func TxFingerprint(rawHex string) (string, error) {
	inputs, err := ParseInputsFromRawHex(rawHex)
	if err != nil {
		return "", err
	}
	outputs, err := ParseOutputsFromRawHex(rawHex)
	if err != nil {
		return "", err
	}

	sort.Slice(inputs, func(i, j int) bool {
		if inputs[i].Txid != inputs[j].Txid {
			return inputs[i].Txid < inputs[j].Txid
		}
		return inputs[i].Vout < inputs[j].Vout
	})

	var inParts []string
	for _, in := range inputs {
		inParts = append(inParts, fmt.Sprintf("%s:%d", in.Txid, in.Vout))
	}
	inPart := strings.Join(inParts, "|")

	var outParts []string
	for _, o := range outputs {
		addrStr := o.Address
		if addrStr == "" {
			addrStr = "None"
		}
		outParts = append(outParts, fmt.Sprintf("%s:%d-%s", addrStr, o.ValueSats, o.OpReturnHex))
	}
	outPart := strings.Join(outParts, "|")

	h := sha256.New()
	h.Write([]byte(inPart + "#" + outPart))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// BuildAndSignMultisigTx builds and signs a P2SH multisig transaction.
func BuildAndSignMultisigTx(
	utxos []UTXO,
	outputs []Output,
	redeemScriptHex string,
	signingKeys []*btcec.PrivateKey,
) (string, error) {
	redeemBytes, err := hex.DecodeString(redeemScriptHex)
	if err != nil {
		return "", fmt.Errorf("invalid redeemScript hex: %w", err)
	}

	orderedPubkeys := ParsePubkeysFromRedeem(redeemBytes)

	// Map private keys to positions in redeemScript
	type keyPos struct {
		pos int
		key *btcec.PrivateKey
	}
	var keyedKeys []keyPos
	for _, key := range signingKeys {
		pubBytes := key.PubKey().SerializeCompressed()
		for pos, pub := range orderedPubkeys {
			if bytes.Equal(pub, pubBytes) {
				keyedKeys = append(keyedKeys, keyPos{pos: pos, key: key})
				break
			}
		}
	}

	sort.Slice(keyedKeys, func(i, j int) bool {
		return keyedKeys[i].pos < keyedKeys[j].pos
	})

	if len(keyedKeys) == 0 {
		return "", fmt.Errorf("none of the loaded keys match the redeemScript")
	}

	// Create unsigned transaction
	msgTx := wire.NewMsgTx(1)

	// Add inputs
	for _, u := range utxos {
		h, err := chainhashv2.NewHashFromStr(u.Txid)
		if err != nil {
			return "", fmt.Errorf("invalid input txid: %w", err)
		}
		outpoint := wire.NewOutPoint(h, u.Vout)
		txIn := wire.NewTxIn(outpoint, nil, nil)
		msgTx.AddTxIn(txIn)
	}

	// Add outputs
	for _, o := range outputs {
		var pkScript []byte
		var err error
		if o.OpReturnHex != "" {
			pkScript, err = makeOpReturnScript(o.OpReturnHex)
		} else {
			pkScript, err = makeOutputScript(o.Address)
		}
		if err != nil {
			return "", fmt.Errorf("failed to make output script: %w", err)
		}
		msgTx.AddTxOut(wire.NewTxOut(o.ValueSats, pkScript))
	}

	// Sign inputs
	for i := range utxos {
		var sigs [][]byte
		sighash, err := txscript.CalcSignatureHash(redeemBytes, txscript.SigHashAll, msgTx, i)
		if err != nil {
			return "", fmt.Errorf("failed to calculate signature hash: %w", err)
		}

		for _, kk := range keyedKeys {
			btcecSig := btcececdsa.Sign(kk.key, sighash)
			derSig := btcecSig.Serialize()
			sigWithHashType := append(derSig, byte(txscript.SigHashAll))
			sigs = append(sigs, sigWithHashType)
		}

		// scriptSig: OP_0 <sig1> ... <sigN> <redeemScript>
		builder := txscript.NewScriptBuilder().AddOp(txscript.OP_0)
		for _, sig := range sigs {
			builder.AddData(sig)
		}
		builder.AddData(redeemBytes)
		scriptSig, err := builder.Script()
		if err != nil {
			return "", fmt.Errorf("failed to build scriptSig: %w", err)
		}

		msgTx.TxIn[i].SignatureScript = scriptSig
	}

	var buf bytes.Buffer
	if err := msgTx.Serialize(&buf); err != nil {
		return "", fmt.Errorf("failed to serialize tx: %w", err)
	}
	if buf.Len() > MaxTxBytes {
		return "", fmt.Errorf("transaction is %d bytes; chain standard limit is %d (reduce MAX_TX_INPUTS)", buf.Len(), MaxTxBytes)
	}
	return hex.EncodeToString(buf.Bytes()), nil
}

// CosignMultisigTx adds our own signature(s) to a partially-signed P2SH multisig tx.
func CosignMultisigTx(
	rawHex string,
	redeemScriptHex string,
	signingKeys []*btcec.PrivateKey,
	requiredSigs int,
) (string, bool, error) {
	redeemBytes, err := hex.DecodeString(redeemScriptHex)
	if err != nil {
		return "", false, fmt.Errorf("invalid redeemScript hex: %w", err)
	}

	orderedPubkeys := ParsePubkeysFromRedeem(redeemBytes)

	type keyPos struct {
		pos int
		key *btcec.PrivateKey
	}
	var keyedKeys []keyPos
	for _, key := range signingKeys {
		pubBytes := key.PubKey().SerializeCompressed()
		for pos, pub := range orderedPubkeys {
			if bytes.Equal(pub, pubBytes) {
				keyedKeys = append(keyedKeys, keyPos{pos: pos, key: key})
				break
			}
		}
	}
	sort.Slice(keyedKeys, func(i, j int) bool {
		return keyedKeys[i].pos < keyedKeys[j].pos
	})

	if len(signingKeys) > 0 && len(keyedKeys) == 0 {
		return "", false, fmt.Errorf("none of the loaded keys match the redeemScript")
	}

	// Deserialize the transaction
	txBytes, err := hex.DecodeString(rawHex)
	if err != nil {
		return "", false, fmt.Errorf("invalid tx hex: %w", err)
	}
	var msgTx wire.MsgTx
	if err := msgTx.Deserialize(bytes.NewReader(txBytes)); err != nil {
		return "", false, fmt.Errorf("failed to deserialize tx: %w", err)
	}

	// Create an unsigned clone for sighash calculations
	msgTxUnsigned := msgTx.Copy()
	for i := range msgTxUnsigned.TxIn {
		msgTxUnsigned.TxIn[i].SignatureScript = nil
	}

	anyComplete := false
	allInputsComplete := true

	for i, inp := range msgTx.TxIn {
		// Parse existing pushed datas (sigs + redeemScript)
		pushes, err := txscript.PushedData(inp.SignatureScript)
		var existingSigs [][]byte
		if err == nil && len(pushes) > 1 {
			// Preceding pushes are sigs (last one is redeemScript)
			existingSigs = pushes[:len(pushes)-1]
		}

		sighash, err := txscript.CalcSignatureHash(redeemBytes, txscript.SigHashAll, msgTxUnsigned, i)
		if err != nil {
			return "", false, fmt.Errorf("failed to calculate signature hash: %w", err)
		}

		// Map existing signatures to pubkey positions
		type sigWithPos struct {
			pos int
			sig []byte
		}
		var sigsWithPos []sigWithPos
		signedPositions := make(map[int]bool)

		for _, sigBytes := range existingSigs {
			if len(sigBytes) == 0 {
				continue
			}
			// Strip sighash type byte (usually last byte) to parse signature
			sigBody := sigBytes[:len(sigBytes)-1]
			btcecSig, err := btcececdsa.ParseDERSignature(sigBody)
			if err != nil {
				continue
			}

			// Verify signature against pubkeys to find the signer position
			for pos, pubBytes := range orderedPubkeys {
				pub, err := btcec.ParsePubKey(pubBytes)
				if err == nil {
					if btcecSig.Verify(sighash, pub) {
						sigsWithPos = append(sigsWithPos, sigWithPos{pos: pos, sig: sigBytes})
						signedPositions[pos] = true
						break
					}
				}
			}
		}

		// Sign with our keys if not already signed at that position
		for _, kk := range keyedKeys {
			if !signedPositions[kk.pos] {
				btcecSig := btcececdsa.Sign(kk.key, sighash)
				derSig := btcecSig.Serialize()
				sigWithHashType := append(derSig, byte(txscript.SigHashAll))
				sigsWithPos = append(sigsWithPos, sigWithPos{pos: kk.pos, sig: sigWithHashType})
				signedPositions[kk.pos] = true
			}
		}

		// Sort signatures by pubkey position
		sort.Slice(sigsWithPos, func(i, j int) bool {
			return sigsWithPos[i].pos < sigsWithPos[j].pos
		})

		// Keep only up to requiredSigs signatures
		finalSigs := make([][]byte, 0)
		for idx, item := range sigsWithPos {
			if idx >= requiredSigs {
				break
			}
			finalSigs = append(finalSigs, item.sig)
		}

		// Build scriptSig
		builder := txscript.NewScriptBuilder().AddOp(txscript.OP_0)
		for _, sig := range finalSigs {
			builder.AddData(sig)
		}
		builder.AddData(redeemBytes)
		scriptSig, err := builder.Script()
		if err != nil {
			return "", false, fmt.Errorf("failed to build scriptSig: %w", err)
		}

		msgTx.TxIn[i].SignatureScript = scriptSig

		if len(finalSigs) >= requiredSigs {
			anyComplete = true
		} else {
			allInputsComplete = false
		}
	}

	var buf bytes.Buffer
	if err := msgTx.Serialize(&buf); err != nil {
		return "", false, fmt.Errorf("failed to serialize tx: %w", err)
	}
	if buf.Len() > MaxTxBytes {
		return "", false, fmt.Errorf("transaction is %d bytes; chain standard limit is %d (reduce MAX_TX_INPUTS)", buf.Len(), MaxTxBytes)
	}

	return hex.EncodeToString(buf.Bytes()), anyComplete && allInputsComplete, nil
}

func EstimateTxFee(nInputs int, nOutputs int, feePerKBSats int64) int64 {
	estimatedBytes := int64(nInputs*300 + nOutputs*34 + 10)
	fee := (estimatedBytes * feePerKBSats) / 1000
	if fee < 1000 {
		return 1000
	}
	return fee
}
