package processor

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"wojak-keeper/internal/chain"
	"wojak-keeper/internal/p2p"

	"github.com/btcsuite/btcd/btcec/v2"
)

// DustLimit is the minimum change output size we emit.
const DustLimit = 15000

// MaxConsolidationInputs is the hard cap for one transaction. ~600 P2SH
// 2-of-3 inputs is the practical ceiling for the 1 MB network tx limit.
const MaxConsolidationInputs = 600

func (k *Keeper) maxInputs() int {
	n := k.cfg.MaxTxInputs
	if n <= 0 {
		n = 100
	}
	if n > MaxConsolidationInputs {
		n = MaxConsolidationInputs
	}
	return n
}

// ErrInvalidRequest describes a rejected manual transfer request.
type ErrInvalidRequest struct{ msg string }

func (e ErrInvalidRequest) Error() string { return e.msg }

// ProposeTransfer builds a manual wallet transfer/consolidation, signs it with
// the node key that matches the wallet, and broadcasts the proposal.
func (k *Keeper) ProposeTransfer(walletAddr, to string, amountSats int64, memo string) (string, error) {
	if walletAddr == "" {
		walletAddr = k.cfg.VaultAddress
	}
	k.mu.Lock()
	w := k.accountByAddressLocked(walletAddr)
	var key *signingKey
	var redeem string
	if w != nil {
		key, _ = k.walletKeyLocked(w)
		redeem = w.view.RedeemScript
	}
	k.mu.Unlock()
	if w == nil {
		return "", ErrInvalidRequest{msg: fmt.Sprintf("unknown wallet %s", walletAddr)}
	}
	if key == nil {
		return "", ErrInvalidRequest{msg: fmt.Sprintf("wallet %q has no signing key on this node; add a private key in the UI", w.view.Name)}
	}

	verTo, _, err := chain.DecodeAddress(to)
	if err != nil {
		return "", ErrInvalidRequest{msg: fmt.Sprintf("bad destination: %v", err)}
	}
	if verTo != chain.AddrPubkeyVersion && verTo != chain.AddrScriptVersion {
		return "", ErrInvalidRequest{msg: fmt.Sprintf("destination version %d is not canonical for this chain (want %d or %d)", verTo, chain.AddrPubkeyVersion, chain.AddrScriptVersion)}
	}
	if amountSats < k.cfg.MinTransferSats {
		return "", ErrInvalidRequest{msg: fmt.Sprintf("amount %d below min %d", amountSats, k.cfg.MinTransferSats)}
	}

	// Deterministic id (per wallet) = idempotency window for identical submits.
	propID := newTransferID(walletAddr, to, amountSats, memo, "")
	k.mu.Lock()
	if st := k.props[propID]; st != nil {
		if !st.terminal() {
			k.mu.Unlock()
			return propID, nil
		}
		for i := 0; i < 8; i++ {
			candidate := newTransferID(walletAddr, to, amountSats, memo, randomNonce())
			if k.props[candidate] == nil {
				propID = candidate
				break
			}
		}
	}
	k.mu.Unlock()

	vaultAddr := walletAddr
	selfTarget := to == vaultAddr

	minConf := k.cfg.CoinbaseMaturity
	utxos, err := k.chain.GetUTXOs(vaultAddr, minConf)
	if err != nil {
		return "", fmt.Errorf("wallet utxos: %w", err)
	}
	var mature []chain.UTXO
	for _, u := range utxos {
		if u.Confirmations >= minConf {
			mature = append(mature, u)
		}
	}
	if len(mature) == 0 {
		return "", ErrInvalidRequest{msg: fmt.Sprintf("wallet has no mature utxos (requires >= %d confs)", minConf)}
	}
	totalMature := len(mature)
	maxInputs := k.maxInputs()

	var selected []chain.UTXO
	var gross int64
	if selfTarget {
		if len(mature) > maxInputs {
			mature = mature[:maxInputs]
		}
		selected = mature
		for _, u := range selected {
			gross += u.Value
		}
	} else {
		fee := k.chain.EstimateFeeSatsWithMin(1, 2, 6, 10)
		for _, u := range mature {
			selected = append(selected, u)
			gross += u.Value
			fee = k.chain.EstimateFeeSatsWithMin(len(selected), 2, 6, 10)
			if gross >= amountSats+fee || len(selected) >= maxInputs {
				break
			}
		}
		if gross < amountSats+fee {
			return "", ErrInvalidRequest{msg: fmt.Sprintf(
				"insufficient mature funds within %d inputs: have %d sats, need %d + fee %d; raise MAX_TX_INPUTS (max %d) or consolidate in batches",
				len(selected), gross, amountSats, fee, maxInputs)}
		}
	}

	nOutputs := 2
	if selfTarget {
		nOutputs = 1
	}
	fee := k.chain.EstimateFeeSatsWithMin(len(selected), nOutputs, 6, 10)

	outputs := []chain.Output{{Address: to, ValueSats: amountSats}}
	switch {
	case selfTarget:
		if gross < fee+DustLimit {
			return "", ErrInvalidRequest{msg: fmt.Sprintf("consolidation aborted: gross %d <= fee %d", gross, fee)}
		}
		outputs = []chain.Output{{Address: vaultAddr, ValueSats: gross - fee}}
		amountSats = gross - fee
		fee = 0
		if memo == "" {
			memo = fmt.Sprintf("consolidate %d utxos", len(selected))
		}
		if len(selected) < totalMature {
			memo = fmt.Sprintf("consolidate %d of %d utxos (max %d per tx)", len(selected), totalMature, maxInputs)
		}
	case gross < amountSats+fee:
		return "", ErrInvalidRequest{msg: fmt.Sprintf("insufficient mature funds: gross %d < requested %d + fee %d", gross, amountSats, fee)}
	default:
		change := gross - amountSats - fee
		if change > 0 && change < DustLimit {
			return "", ErrInvalidRequest{msg: fmt.Sprintf("amount leaves unreadable dust change (%d sats); adjust the amount so change >= %d", change, DustLimit)}
		}
		if change > 0 {
			outputs = append(outputs, chain.Output{Address: vaultAddr, ValueSats: change})
		}
	}

	raw, err := chain.BuildAndSignMultisigTx(selected, outputs, redeem, []*btcec.PrivateKey{key.priv})
	if err != nil {
		return "", fmt.Errorf("build/sign transfer tx: %w", err)
	}
	txid, err := chain.TxFingerprint(raw)
	if err != nil {
		return "", err
	}
	pubHex := hex.EncodeToString(key.priv.PubKey().SerializeCompressed())

	prop := p2p.TransferProposal{
		ProposalId: propID,
		Vault:      walletAddr,
		To:         to,
		AmountSats: amountSats,
		Memo:       memo,
		GrossSats:  gross,
		FeeSats:    fee,
		RawTxHex:   raw,
		Txid:       txid,
		SignedBy:   []string{pubHex},
	}

	k.mu.Lock()
	k.createLocked(propID, walletAddr, &prop, raw, true, "")
	k.mu.Unlock()

	if k.node != nil {
		k.node.Broadcast(p2p.TypeProposeTransfer, prop)
	}
	log.Printf("[transfer] proposed %s (%s): %d sats -> %s", propID, w.view.Name, amountSats, to)
	return propID, nil
}

// verifyTransfer re-derives a peer's transfer against the wallet named in the
// proposal. Unknown wallets fail closed.
func (k *Keeper) verifyTransfer(prop p2p.TransferProposal) error {
	k.mu.Lock()
	w := k.accountByAddressLocked(prop.Vault)
	var redeem, vaultAddr string
	var required int
	if w != nil {
		redeem = w.view.RedeemScript
		vaultAddr = w.view.Address
		required = w.view.Required
	}
	k.mu.Unlock()
	if w == nil {
		return fmt.Errorf("unknown wallet %s", prop.Vault)
	}
	_ = required

	verTo, _, err := chain.DecodeAddress(prop.To)
	if err != nil {
		return fmt.Errorf("bad destination: %w", err)
	}
	if verTo != chain.AddrPubkeyVersion && verTo != chain.AddrScriptVersion {
		return fmt.Errorf("destination version %d is not canonical for this chain", verTo)
	}
	outputs, err := chain.ParseOutputsFromRawHex(prop.RawTxHex)
	if err != nil {
		return fmt.Errorf("invalid tx outputs: %w", err)
	}
	if len(outputs) == 0 || len(outputs) > 2 {
		return fmt.Errorf("transfer has %d outputs (expected 1..2)", len(outputs))
	}
	if outputs[0].Address != prop.To || outputs[0].ValueSats != prop.AmountSats {
		return fmt.Errorf("output[0] (%s, %d sats) != proposal intent (%s, %d sats)",
			outputs[0].Address, outputs[0].ValueSats, prop.To, prop.AmountSats)
	}
	if len(outputs) == 2 {
		if outputs[1].OpReturnHex != "" {
			return fmt.Errorf("unexpected OP_RETURN output in transfer")
		}
		if outputs[1].Address != vaultAddr {
			return fmt.Errorf("change output[1] recipient %s != wallet %s", outputs[1].Address, vaultAddr)
		}
	}

	minConf := k.cfg.CoinbaseMaturity
	inputSum, err := chain.VerifyProposalInputs(k.chain, prop.RawTxHex, redeem, minConf)
	if err != nil {
		return fmt.Errorf("input verification: %w", err)
	}
	if prop.GrossSats > 0 && inputSum != prop.GrossSats {
		return fmt.Errorf("input sum %d != claimed gross %d", inputSum, prop.GrossSats)
	}
	var outSum int64
	for _, o := range outputs {
		outSum += o.ValueSats
	}
	fee := inputSum - outSum
	if fee < 0 {
		return fmt.Errorf("outputs %d exceed inputs %d", outSum, inputSum)
	}
	if fee > inputSum/2 {
		return fmt.Errorf("abnormal fee %d for inputs %d", fee, inputSum)
	}
	if prop.FeeSats > 0 && fee > prop.FeeSats*10 && fee-prop.FeeSats > 1_000_000 {
		log.Printf("[transfer] warning: realized fee %d differs from advertised %d", fee, prop.FeeSats)
	}
	return nil
}

// onProposeTransfer records a peer's transfer proposal for the wallet it names.
func (k *Keeper) onProposeTransfer(prop p2p.TransferProposal) error {
	k.mu.Lock()
	if _, seen := k.props[prop.ProposalId]; seen {
		k.mu.Unlock()
		return nil
	}
	w := k.accountByAddressLocked(prop.Vault)
	var key *signingKey
	var redeem string
	var required int
	if w != nil {
		key, _ = k.walletKeyLocked(w)
		redeem = w.view.RedeemScript
		required = w.view.Required
	}
	k.mu.Unlock()

	if err := k.verifyTransfer(prop); err != nil {
		log.Printf("[transfer] REJECT %s: %v", prop.ProposalId, err)
		k.mu.Lock()
		k.createLocked(prop.ProposalId, prop.Vault, &prop, prop.RawTxHex, false, err.Error())
		k.mu.Unlock()
		return err
	}
	if k.cfg.AutoSign && key != nil {
		raw, complete, err := chain.CosignMultisigTx(prop.RawTxHex, redeem, []*btcec.PrivateKey{key.priv}, required)
		if err != nil {
			log.Printf("[transfer] cosign %s: %v", prop.ProposalId, err)
			return err
		}
		k.mu.Lock()
		st := k.createLocked(prop.ProposalId, prop.Vault, &prop, prop.RawTxHex, false, "")
		k.markSignedLocked(st, raw)
		k.broadcastApprovalLocked(st, raw)
		if complete {
			k.finalizeCompleteLocked(st, raw)
		}
		k.mu.Unlock()
		return nil
	}
	k.mu.Lock()
	k.createLocked(prop.ProposalId, prop.Vault, &prop, prop.RawTxHex, false, "")
	k.mu.Unlock()
	log.Printf("[transfer] %s pending (%s): %d sats -> %s: waiting for manual approval",
		prop.ProposalId, prop.Vault, prop.AmountSats, prop.To)
	return nil
}

func newTransferID(vault, to string, amount int64, memo, nonce string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%d|%s|%d|%s", vault, to, amount, memo, time.Now().Unix()/300, nonce)
	return idPrefixTransfer + hex.EncodeToString(h.Sum(nil))[:16]
}

func randomNonce() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
