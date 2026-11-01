package chain

// Supported chains (WojakCoin and Junkcoin) relay non-witness transactions up
// to MAX_STANDARD_TX_WEIGHT = 400,000 weight units. For a P2SH transaction the
// serialized size is weight/4, so the relay limit is 100,000 bytes. A larger
// transaction may be consensus-valid but is rejected by peers as non-standard.
const MaxStandardTxBytes = 100_000

// MaxTxBytes is the serialized transaction ceiling enforced by the builder and
// by proposal verification. It is the chain's standard relay limit, not the
// 1 MB block limit: the keeper must never create a transaction that peers
// refuse to relay.
const MaxTxBytes = MaxStandardTxBytes

// MaxTxInputs is the absolute upper bound accepted by configuration. The
// effective per-wallet limit is lower (MaxInputsForRedeem) because every P2SH
// multisig input consumes roughly 300 bytes.
const MaxTxInputs = 600

// MaxInputsForRedeem returns the largest number of P2SH multisig inputs that
// fit in a standard transaction for the given redeem script and signature
// threshold. This is the chain limit the keeper enforces before building a
// transfer or consolidation; MAX_TX_INPUTS may only lower it.
func MaxInputsForRedeem(redeemScriptHex string, required int) int {
	redeemLen := len(redeemScriptHex) / 2
	if redeemLen < 3 {
		// Unknown/empty script: assume the common 2-of-3 shape so callers
		// still get a conservative limit instead of an unbounded one.
		redeemLen = 105
	}
	if required < 1 || required > 16 {
		required = 2
	}

	// Conservative signed input size:
	//   outpoint(36) + scriptSig length(<=3) + scriptSig + sequence(4)
	//   scriptSig = OP_0 + required * (push + max DER sig + sighash byte)
	//               + push(redeem) + redeem
	const maxSigBytes = 73
	scriptSigLen := 1 + required*(1+maxSigBytes) + pushDataLen(redeemLen) + redeemLen
	inputBytes := 36 + compactSizeLen(scriptSigLen) + scriptSigLen + 4

	// Reserve two P2SH outputs, the fixed transaction header and a safety
	// margin for signature-length variance.
	const reserve = 2*34 + 16 + 200
	n := (MaxStandardTxBytes - reserve) / inputBytes
	if n < 1 {
		n = 1
	}
	if n > MaxTxInputs {
		n = MaxTxInputs
	}
	return n
}

// pushDataLen is the size of the script push opcode for n bytes.
func pushDataLen(n int) int {
	switch {
	case n < 0x4c:
		return 1
	case n <= 0xff:
		return 2
	case n <= 0xffff:
		return 3
	default:
		return 5
	}
}

// compactSizeLen is the size of a Bitcoin CompactSize integer.
func compactSizeLen(n int) int {
	switch {
	case n < 0xfd:
		return 1
	case n <= 0xffff:
		return 3
	case uint64(n) <= 0xffffffff:
		return 5
	default:
		return 9
	}
}
