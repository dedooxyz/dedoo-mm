package chain

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/txscript/v2"
)

// BuildMultisigRedeemScript assembles a bare M-of-3 CHECKMULTISIG redeem
// script:
//
//	OP_M <pub> <pub> <pub> OP_3 OP_CHECKMULTISIG
//
// required is the M signature threshold (from REQUIRED_SIGS in .env).
// pubOrder indexes into pubs — reordering the same 3 ceremony pubkeys yields
// a different (yet equivalent) vault script/address.
func BuildMultisigRedeemScript(pubs []string, pubOrder []int, required int) (string, error) {
	if len(pubs) != 3 {
		return "", fmt.Errorf("ceremony requires exactly 3 pubkeys, got %d", len(pubs))
	}
	if len(pubOrder) != 3 {
		return "", fmt.Errorf("pubkey order must be 3 indexes, got %d", len(pubOrder))
	}
	if required < 1 || required > 3 {
		return "", fmt.Errorf("required sigs must be 1..3, got %d", required)
	}

	seen := make(map[int]bool)
	b := txscript.NewScriptBuilder().
		AddOp(byte(txscript.OP_1 + required - 1))
	for _, i := range pubOrder {
		if i < 0 || i >= 3 {
			return "", fmt.Errorf("pubkey order index %d out of range", i)
		}
		if seen[i] {
			return "", fmt.Errorf("pubkey order repeats index %d", i)
		}
		seen[i] = true
		raw, err := hex.DecodeString(pubs[i])
		if err != nil {
			return "", fmt.Errorf("pubkey %d: %w", i, err)
		}
		b.AddData(raw)
	}
	b.AddOp(byte(txscript.OP_1 + len(pubs) - 1))
	b.AddOp(txscript.OP_CHECKMULTISIG)

	script, err := b.Script()
	if err != nil {
		return "", err
	}
	return strings.ToLower(hex.EncodeToString(script)), nil
}
