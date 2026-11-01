package processor

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/dedooxyz/dedoo-mm/internal/chain"
	"github.com/dedooxyz/dedoo-mm/internal/p2p"

	"github.com/btcsuite/btcd/btcec/v2"
)

// DustLimit is the minimum change output size we emit.
const DustLimit = 15000

// ConsolidationPlan is a read-only preview of a consolidation transaction.
// The output amount is always derived from the selected UTXOs minus the
// network fee, never supplied by the caller.
type ConsolidationPlan struct {
	Wallet     string       `json:"wallet"`
	Available  int          `json:"availableUtxos"`
	Requested  int          `json:"requestedUtxos"`
	Selected   int          `json:"selectedUtxos"`
	MaxInputs  int          `json:"maxInputs"`  // effective cap: min(MAX_TX_INPUTS, chain)
	ChainLimit int          `json:"chainLimit"` // hard cap from the chain's 100 KB standard tx size
	GrossSats  int64        `json:"grossSats"`  // sum of selected UTXOs
	FeeSats    int64        `json:"feeSats"`    // estimated network fee
	NetSats    int64        `json:"netSats"`    // consolidated output value
	UTXOs      []chain.UTXO `json:"utxos"`      // exactly the inputs that will be spent
}

type inputSelection struct {
	utxos     []chain.UTXO
	available int
	gross     int64
	fee       int64
	net       int64
	maxInputs int
	chainMax  int
}

// chainInputLimit returns the hard number of inputs the chain allows for this
// wallet's redeem script.
func chainInputLimit(redeem string, required int) int {
	return chain.MaxInputsForRedeem(redeem, required)
}

// maxInputsFor is the effective per-transaction cap: the operator's
// MAX_TX_INPUTS, never above the chain's standard transaction size limit.
func (k *Keeper) maxInputsFor(redeem string, required int) int {
	chainMax := chainInputLimit(redeem, required)
	n := k.cfg.MaxTxInputs
	if n <= 0 || n > chainMax {
		n = chainMax
	}
	return n
}

// minConsolidationOutput is the smallest acceptable consolidated output: the
// operator's transfer floor, but never below the chain's dust threshold.
func (k *Keeper) minConsolidationOutput() int64 {
	min := k.cfg.MinTransferSats
	if min < DustLimit {
		min = DustLimit
	}
	return min
}

// ErrInvalidRequest describes a rejected manual transfer request.
type ErrInvalidRequest struct{ msg string }

func (e ErrInvalidRequest) Error() string { return e.msg }

// ProposeTransfer builds a manual wallet transfer, signs it with the node key
// that matches the wallet, and broadcasts the proposal. A transfer to the
// wallet itself is treated as a consolidation of all mature UTXOs (capped by
// the transaction limit); use ProposeConsolidation to choose a specific count.
func (k *Keeper) ProposeTransfer(walletAddr, to string, amountSats int64, memo string) (string, error) {
	return k.proposeTransfer(walletAddr, to, amountSats, memo, false, 0)
}

// ProposeConsolidation merges utxoCount mature UTXOs into one output back to
// the wallet. utxoCount == 0 means "all mature, capped by the transaction
// limit". The coin amount is derived from the selected UTXOs minus the fee.
func (k *Keeper) ProposeConsolidation(walletAddr string, utxoCount int, memo string) (string, error) {
	if utxoCount < 0 {
		return "", ErrInvalidRequest{msg: "utxo count must be >= 0 (0 = all mature)"}
	}
	return k.proposeTransfer(walletAddr, "", 0, memo, true, utxoCount)
}

// PlanConsolidation previews a consolidation without creating a proposal.
func (k *Keeper) PlanConsolidation(walletAddr string, utxoCount int) (ConsolidationPlan, error) {
	if walletAddr == "" {
		walletAddr = k.cfg.VaultAddress
	}
	if utxoCount < 0 {
		return ConsolidationPlan{}, ErrInvalidRequest{msg: "utxo count must be >= 0 (0 = all mature)"}
	}
	sel, err := k.selectConsolidation(walletAddr, utxoCount)
	if err != nil {
		return ConsolidationPlan{}, err
	}
	return ConsolidationPlan{
		Wallet:     walletAddr,
		Available:  sel.available,
		Requested:  utxoCount,
		Selected:   len(sel.utxos),
		MaxInputs:  sel.maxInputs,
		ChainLimit: sel.chainMax,
		GrossSats:  sel.gross,
		FeeSats:    sel.fee,
		NetSats:    sel.net,
		UTXOs:      sel.utxos,
	}, nil
}

func (k *Keeper) proposeTransfer(walletAddr, to string, amountSats int64, memo string, consolidate bool, utxoCount int) (string, error) {
	if walletAddr == "" {
		walletAddr = k.cfg.VaultAddress
	}
	k.mu.Lock()
	w := k.accountByAddressLocked(walletAddr)
	var key *signingKey
	var redeem string
	var required int
	if w != nil {
		key, _ = k.walletKeyLocked(w)
		redeem = w.view.RedeemScript
		required = w.view.Required
	}
	k.mu.Unlock()
	if w == nil {
		return "", ErrInvalidRequest{msg: fmt.Sprintf("unknown wallet %s", walletAddr)}
	}
	if key == nil {
		return "", ErrInvalidRequest{msg: fmt.Sprintf("wallet %q has no signing key on this node; add a private key in the UI", w.view.Name)}
	}

	if consolidate {
		to = walletAddr
	}
	verTo, _, err := chain.DecodeAddress(to)
	if err != nil {
		return "", ErrInvalidRequest{msg: fmt.Sprintf("bad destination: %v", err)}
	}
	if verTo != chain.AddrPubkeyVersion && verTo != chain.AddrScriptVersion {
		return "", ErrInvalidRequest{msg: fmt.Sprintf("destination version %d is not canonical for this chain (want %d or %d)", verTo, chain.AddrPubkeyVersion, chain.AddrScriptVersion)}
	}
	if !consolidate && (amountSats <= 0 || amountSats < k.cfg.MinTransferSats) {
		return "", ErrInvalidRequest{msg: fmt.Sprintf("amount %d below min %d", amountSats, k.cfg.MinTransferSats)}
	}

	// Deterministic id (per wallet) = idempotency window for identical submits.
	// The UTXO count is part of the id so different consolidation selections
	// never collide.
	propID := newTransferID(walletAddr, to, amountSats, memo, "", utxoCount)
	k.mu.Lock()
	if st := k.props[propID]; st != nil {
		if !st.terminal() {
			k.mu.Unlock()
			return propID, nil
		}
		for i := 0; i < 8; i++ {
			candidate := newTransferID(walletAddr, to, amountSats, memo, randomNonce(), utxoCount)
			if k.props[candidate] == nil {
				propID = candidate
				break
			}
		}
	}
	k.mu.Unlock()

	selfTarget := consolidate || to == walletAddr

	var sel *inputSelection
	if selfTarget {
		sel, err = k.selectConsolidation(walletAddr, utxoCount)
	} else {
		sel, err = k.selectTransfer(walletAddr, amountSats, redeem, required)
	}
	if err != nil {
		return "", err
	}
	selected := sel.utxos
	gross := sel.gross

	nOutputs := 2
	if selfTarget {
		nOutputs = 1
	}
	fee := k.chain.EstimateFeeSatsWithMin(len(selected), nOutputs, 6, 10)

	outputs := []chain.Output{{Address: to, ValueSats: amountSats}}
	switch {
	case selfTarget:
		net := gross - fee
		if net <= 0 {
			return "", ErrInvalidRequest{msg: fmt.Sprintf("consolidation aborted: gross %d <= fee %d", gross, fee)}
		}
		if net < k.minConsolidationOutput() {
			return "", ErrInvalidRequest{msg: fmt.Sprintf("consolidation aborted: output %d below minimum %d", net, k.minConsolidationOutput())}
		}
		outputs = []chain.Output{{Address: walletAddr, ValueSats: net}}
		amountSats = net
		if memo == "" {
			memo = fmt.Sprintf("consolidate %d utxos", len(selected))
			if len(selected) < sel.available {
				memo = fmt.Sprintf("consolidate %d of %d utxos", len(selected), sel.available)
				if utxoCount == 0 {
					memo += fmt.Sprintf(" (max %d per tx)", sel.maxInputs)
				}
			}
		}
	case gross < amountSats+fee:
		return "", ErrInvalidRequest{msg: fmt.Sprintf("insufficient mature funds: gross %d < requested %d + fee %d", gross, amountSats, fee)}
	default:
		change := gross - amountSats - fee
		if change > 0 && change < DustLimit {
			return "", ErrInvalidRequest{msg: fmt.Sprintf("amount leaves unreadable dust change (%d sats); adjust the amount so change >= %d", change, DustLimit)}
		}
		if change > 0 {
			outputs = append(outputs, chain.Output{Address: walletAddr, ValueSats: change})
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
	if selfTarget {
		log.Printf("[transfer] proposed consolidation %s (%s): %d utxos, %d sats -> %s", propID, w.view.Name, len(selected), amountSats, walletAddr)
	} else {
		log.Printf("[transfer] proposed %s (%s): %d sats -> %s", propID, w.view.Name, amountSats, to)
	}
	return propID, nil
}

// matureUTXOs returns the wallet's spendable outputs after applying the
// chain's coinbase maturity window.
func (k *Keeper) matureUTXOs(walletAddr string) ([]chain.UTXO, error) {
	minConf := k.cfg.CoinbaseMaturity
	utxos, err := k.chain.GetUTXOs(walletAddr, minConf)
	if err != nil {
		return nil, fmt.Errorf("wallet utxos: %w", err)
	}
	mature := make([]chain.UTXO, 0, len(utxos))
	for _, u := range utxos {
		if u.Confirmations >= minConf {
			mature = append(mature, u)
		}
	}
	return mature, nil
}

// sortForSpend puts larger outputs first so a transfer needs as few inputs as
// possible, which keeps it under the chain limit.
func sortForSpend(utxos []chain.UTXO) {
	sort.Slice(utxos, func(i, j int) bool {
		if utxos[i].Value != utxos[j].Value {
			return utxos[i].Value > utxos[j].Value
		}
		if utxos[i].Confirmations != utxos[j].Confirmations {
			return utxos[i].Confirmations > utxos[j].Confirmations
		}
		if utxos[i].Txid != utxos[j].Txid {
			return utxos[i].Txid < utxos[j].Txid
		}
		return utxos[i].Vout < utxos[j].Vout
	})
}

// sortForConsolidation puts smaller outputs first so dust and low-value coins
// are cleared first. The order is deterministic so the preview and the
// proposal select the same UTXOs.
func sortForConsolidation(utxos []chain.UTXO) {
	sort.Slice(utxos, func(i, j int) bool {
		if utxos[i].Value != utxos[j].Value {
			return utxos[i].Value < utxos[j].Value
		}
		if utxos[i].Confirmations != utxos[j].Confirmations {
			return utxos[i].Confirmations > utxos[j].Confirmations
		}
		if utxos[i].Txid != utxos[j].Txid {
			return utxos[i].Txid < utxos[j].Txid
		}
		return utxos[i].Vout < utxos[j].Vout
	})
}

func (k *Keeper) selectConsolidation(walletAddr string, utxoCount int) (*inputSelection, error) {
	k.mu.Lock()
	w := k.accountByAddressLocked(walletAddr)
	var redeem string
	var required int
	if w != nil {
		redeem = w.view.RedeemScript
		required = w.view.Required
	}
	k.mu.Unlock()
	if w == nil {
		return nil, ErrInvalidRequest{msg: fmt.Sprintf("unknown wallet %s", walletAddr)}
	}

	mature, err := k.matureUTXOs(walletAddr)
	if err != nil {
		return nil, err
	}
	if len(mature) == 0 {
		return nil, ErrInvalidRequest{msg: fmt.Sprintf("wallet has no mature utxos (requires >= %d confs)", k.cfg.CoinbaseMaturity)}
	}
	sortForConsolidation(mature)

	chainMax := chainInputLimit(redeem, required)
	maxInputs := k.maxInputsFor(redeem, required)
	available := len(mature)

	switch {
	case utxoCount == 0:
		utxoCount = available
		if utxoCount > maxInputs {
			utxoCount = maxInputs
		}
	case utxoCount > maxInputs:
		return nil, ErrInvalidRequest{msg: fmt.Sprintf(
			"requested %d utxos exceeds this node's per-transaction limit of %d (chain limit %d); raise MAX_TX_INPUTS or consolidate in batches",
			utxoCount, maxInputs, chainMax)}
	case utxoCount > available:
		return nil, ErrInvalidRequest{msg: fmt.Sprintf("requested %d utxos but only %d are mature", utxoCount, available)}
	}

	selected := mature[:utxoCount]
	var gross int64
	for _, u := range selected {
		gross += u.Value
	}
	fee := k.chain.EstimateFeeSatsWithMin(len(selected), 1, 6, 10)
	net := gross - fee
	if net <= 0 {
		return nil, ErrInvalidRequest{msg: fmt.Sprintf("consolidation aborted: gross %d <= fee %d", gross, fee)}
	}
	if net < k.minConsolidationOutput() {
		return nil, ErrInvalidRequest{msg: fmt.Sprintf("consolidation aborted: output %d below minimum %d", net, k.minConsolidationOutput())}
	}

	return &inputSelection{
		utxos:     selected,
		available: available,
		gross:     gross,
		fee:       fee,
		net:       net,
		maxInputs: maxInputs,
		chainMax:  chainMax,
	}, nil
}

func (k *Keeper) selectTransfer(walletAddr string, amountSats int64, redeem string, required int) (*inputSelection, error) {
	mature, err := k.matureUTXOs(walletAddr)
	if err != nil {
		return nil, err
	}
	if len(mature) == 0 {
		return nil, ErrInvalidRequest{msg: fmt.Sprintf("wallet has no mature utxos (requires >= %d confs)", k.cfg.CoinbaseMaturity)}
	}
	sortForSpend(mature)

	chainMax := chainInputLimit(redeem, required)
	maxInputs := k.maxInputsFor(redeem, required)
	available := len(mature)

	var selected []chain.UTXO
	var gross int64
	var total int64
	for _, u := range mature {
		total += u.Value
	}

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
		if len(selected) >= maxInputs {
			return nil, ErrInvalidRequest{msg: fmt.Sprintf(
				"amount %d sats needs more than %d inputs (this node's limit; chain limit %d) with %d mature utxos worth %d sats: consolidate first or raise MAX_TX_INPUTS",
				amountSats, maxInputs, chainMax, available, total)}
		}
		return nil, ErrInvalidRequest{msg: fmt.Sprintf(
			"insufficient mature funds: have %d sats across %d utxos, need %d + fee %d",
			total, available, amountSats, fee)}
	}

	return &inputSelection{
		utxos:     selected,
		available: available,
		gross:     gross,
		fee:       fee,
		net:       gross - amountSats - fee,
		maxInputs: maxInputs,
		chainMax:  chainMax,
	}, nil
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

	// Reject transactions the chain will not relay. This is checked here as
	// well as in the builder because a peer may have produced the raw tx with
	// a different (or compromised) implementation.
	if len(prop.RawTxHex)/2 > chain.MaxTxBytes {
		return fmt.Errorf("transaction is %d bytes; chain standard limit is %d", len(prop.RawTxHex)/2, chain.MaxTxBytes)
	}
	inputs, err := chain.ParseInputsFromRawHex(prop.RawTxHex)
	if err != nil {
		return fmt.Errorf("invalid tx inputs: %w", err)
	}
	chainMax := chainInputLimit(redeem, required)
	if len(inputs) > chainMax {
		return fmt.Errorf("transaction has %d inputs; chain limit for this wallet is %d", len(inputs), chainMax)
	}
	if localMax := k.maxInputsFor(redeem, required); len(inputs) > localMax {
		log.Printf("[transfer] warning: proposal %s has %d inputs, above this node's MAX_TX_INPUTS=%d (chain limit %d)",
			prop.ProposalId, len(inputs), localMax, chainMax)
	}

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

func newTransferID(vault, to string, amount int64, memo, nonce string, utxoCount int) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%d|%s|%d|%s|%d", vault, to, amount, memo, time.Now().Unix()/300, nonce, utxoCount)
	return idPrefixTransfer + hex.EncodeToString(h.Sum(nil))[:16]
}

func randomNonce() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
