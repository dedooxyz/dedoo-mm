package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/txscript/v2"
	"golang.org/x/crypto/ripemd160"
)

// Hash160FromScript extracts the HASH160 payload from a P2SH or P2PKH
// locking script:
//
//	P2SH:  a9 14 <20-byte hash160> 87
//	P2PKH: 76 a9 14 <20-byte hash160> 88 ac
//
// Returns nil for any other script shape.
func Hash160FromScript(spk []byte) []byte {
	switch len(spk) {
	case 23:
		if spk[0] == 0xa9 && spk[1] == 0x14 && spk[22] == 0x87 {
			return spk[2:22]
		}
	case 25:
		if spk[0] == 0x76 && spk[1] == 0xa9 && spk[2] == 0x14 && spk[23] == 0x88 && spk[24] == 0xac {
			return spk[3:23]
		}
	}
	return nil
}

// Hash160 returns RIPEMD160(SHA256(data)).
func Hash160(data []byte) []byte {
	h := sha256.Sum256(data)
	r := ripemd160.New()
	r.Write(h[:])
	return r.Sum(nil)
}

// RedeemScriptPubKeyHash computes the HASH160 of a P2SH redeem script — the
// value embedded in the vault's P2SH address.
func RedeemScriptPubKeyHash(redeemScriptHex string) ([]byte, error) {
	redeemBytes, err := hex.DecodeString(redeemScriptHex)
	if err != nil {
		return nil, fmt.Errorf("invalid redeemScript hex: %w", err)
	}
	return Hash160(redeemBytes), nil
}

// P2SHAddressFromRedeemScript derives the chain's P2SH address for a redeem
// script. Used at startup to prove VAULT_ADDRESS matches the configured
// redeem script (both are operator-supplied via .env).
func P2SHAddressFromRedeemScript(redeemScriptHex string) (string, error) {
	h160, err := RedeemScriptPubKeyHash(redeemScriptHex)
	if err != nil {
		return "", err
	}
	return EncodeAddress(AddrScriptVersion, h160), nil
}

// P2SHAddressForVersion derives a P2SH address using an explicit script
// version, without mutating the package-level globals. Used by the
// multi-chain wallet manager.
func P2SHAddressForVersion(redeemScriptHex string, scriptVersion byte) (string, error) {
	h160, err := RedeemScriptPubKeyHash(redeemScriptHex)
	if err != nil {
		return "", err
	}
	return EncodeAddress(scriptVersion, h160), nil
}

// ParseMultisigThreshold reads the M in an M-of-N CHECKMULTISIG redeem script.
func ParseMultisigThreshold(redeemScriptHex string) (int, error) {
	b, err := hex.DecodeString(redeemScriptHex)
	if err != nil {
		return 0, fmt.Errorf("invalid redeemScript hex: %w", err)
	}
	if len(b) < 3 || b[0] < txscript.OP_1 || b[0] > txscript.OP_16 {
		return 0, fmt.Errorf("redeem script does not start with OP_M")
	}
	return int(b[0]-txscript.OP_1) + 1, nil
}

// VerifyProposalInputs re-derives every input of a raw proposal transaction
// against the chain backend (independent chain read, NOT trusting the
// proposer's view):
//
//   - each input must resolve to an unspent output
//   - its locking script hash160 must equal the vault redeem script hash160
//   - each input must have at least minConf confirmations
//
// Returns the summed input value so callers can additionally tie economic
// claims (e.g. `grossSats`) to on-chain reality.
func VerifyProposalInputs(backend ChainBackend, rawHex, redeemScriptHex string, minConf int64) (inputSum int64, err error) {
	redeemHash, err := RedeemScriptPubKeyHash(redeemScriptHex)
	if err != nil {
		return 0, err
	}
	inputs, err := ParseInputsFromRawHex(rawHex)
	if err != nil {
		return 0, fmt.Errorf("parse tx inputs: %w", err)
	}
	if len(inputs) == 0 {
		return 0, fmt.Errorf("proposal has no inputs")
	}
	for _, in := range inputs {
		prev, err := backend.GetTxPrevOut(in.Txid, in.Vout)
		if err != nil {
			return 0, fmt.Errorf("input %s:%d: %w", in.Txid, in.Vout, err)
		}
		spk, err := hex.DecodeString(prev.ScriptHex)
		if err != nil {
			return 0, fmt.Errorf("input %s:%d: bad script hex: %w", in.Txid, in.Vout, err)
		}
		h160 := Hash160FromScript(spk)
		if h160 == nil || !bytes.Equal(h160, redeemHash) {
			return 0, fmt.Errorf("input %s:%d (%d sats): locked to a different script, not this vault", in.Txid, in.Vout, prev.ValueSats)
		}
		if prev.Confirmations < minConf {
			return 0, fmt.Errorf("input %s:%d immature: %d confs < %d", in.Txid, in.Vout, prev.Confirmations, minConf)
		}
		inputSum += prev.ValueSats
	}
	return inputSum, nil
}
