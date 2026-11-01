// Package processor coordinates one keeper node: it builds transfer and
// consolidation proposals for a single 2-of-3 multisig vault, verifies peers'
// proposals against the chain, and signs/broadcasts with human approval.
//
// Flow (default, human-in-the-loop):
//  1. a proposal arrives via P2P or is created locally -> stored PENDING
//  2. every keeper operator verifies the tx in the web UI (inputs, outputs,
//     fee, intent are all re-derived locally; nobody signs anything blind)
//  3. approve (adds this node's ECDSA sig) / reject (informational warning)
//  4. once 2-of-3 independent sigs exist the tx becomes READY and any
//     operator may hit Broadcast (AUTO_BROADCAST=1 restores old behaviour).
package processor

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"wojak-keeper/internal/chain"
	"wojak-keeper/internal/config"
	"wojak-keeper/internal/p2p"

	"github.com/btcsuite/btcd/btcec/v2"
)

// idPrefixTransfer marks every proposal id (tr<16hex>).
const idPrefixTransfer = "tr"

// propState tracks one transfer proposal across the trio.
type propState struct {
	id       string
	vault    string // wallet P2SH address
	transfer *p2p.TransferProposal

	rawForSigning string // lowest-known partially-signed hex
	signedHex     string // best merged hex (often == rawForSigning after our sig)

	// signing state
	mySigned  bool
	complete  bool
	verifyErr string // local verification failure (we will never sign)

	// broadcast state
	broadcasted   bool
	broadcasting  bool
	broadcastErr  string
	broadcastTxid string

	sigs    map[string]bool   // keeper pubHex -> true (who signed)
	rejects map[string]string // keeper pubHex -> reason

	created time.Time
	updated time.Time
}

// Keeper is the central state machine for one node in the 2-of-3.
type Keeper struct {
	cfg    *config.Config
	chain  chain.ChainBackend
	node   *p2p.Node
	priv   *btcec.PrivateKey // KEEPER_WIF key (env)
	pubHex string

	mu      sync.Mutex
	props   map[string]*propState
	wallets map[string]*wallet     // by P2SH address (includes the env vault)
	keys    map[string]*signingKey // by compressed pubkey hex; usable by all matching wallets

	reqLog map[string]time.Time // proposal-body recovery throttle
}

// NewKeeper wires config, chain clients and the signing key.
func NewKeeper(cfg *config.Config) (*Keeper, error) {
	var backend chain.ChainBackend
	if cfg.Backend == "rpc" {
		rpcClient, err := chain.NewFailoverBitcoindClient(cfg.RPCURLs(), cfg.RPCUser, cfg.RPCPass, 15*time.Second)
		if err != nil {
			return nil, err
		}
		backend = rpcClient
		log.Printf("[keeper] chain backend: 100%% Core RPC mode (endpoints: %v)", cfg.RPCURLs())
	} else {
		// default: 100% Electrs full mode
		fc, err := chain.NewFailoverElectrsClient(cfg.ElectrsURLs(), 15*time.Second)
		if err != nil {
			return nil, err
		}
		backend = fc
		log.Printf("[keeper] chain backend: 100%% Electrs REST mode (endpoints: %v)", cfg.ElectrsURLs())
	}

	var priv *btcec.PrivateKey
	pubHex := ""
	if cfg.KeeperWif != "" {
		var err error
		priv, _, err = chain.LoadWifKey(cfg.KeeperWif)
		if err != nil {
			return nil, err
		}
		pubHex = hex.EncodeToString(priv.PubKey().SerializeCompressed())
	}
	mode := "manual"
	if cfg.AutoSign {
		mode = "auto"
	}
	bmode := "manual-broadcast"
	if cfg.AutoBroadcast {
		bmode = "auto-broadcast"
	}
	log.Printf("[keeper] multi-wallet mode (chain %s), signing policy: %s, broadcast policy: %s",
		cfg.ChainName, mode, bmode)
	k := &Keeper{
		cfg:     cfg,
		chain:   backend,
		priv:    priv,
		pubHex:  pubHex,
		props:   make(map[string]*propState),
		reqLog:  make(map[string]time.Time),
		wallets: make(map[string]*wallet),
		keys:    make(map[string]*signingKey),
	}
	if priv != nil {
		k.keys[pubHex] = &signingKey{wif: cfg.KeeperWif, priv: priv, env: true, added: time.Now()}
	}
	if cfg.VaultAddress != "" {
		k.wallets[cfg.VaultAddress] = &wallet{view: AccountView{
			Id:                "active",
			Name:              "Active signing vault",
			Chain:             cfg.ChainName,
			ChainKey:          chainKeyFor(cfg.NetParams),
			Address:           cfg.VaultAddress,
			Required:          cfg.RequiredSigs,
			AddrPubkeyVersion: int(cfg.NetParams.AddrPubkeyVersion),
			AddrScriptVersion: int(cfg.NetParams.AddrScriptVersion),
			WifVersion:        int(cfg.NetParams.WifVersion),
			CoinbaseMaturity:  cfg.CoinbaseMaturity,
			ElectrsURL:        cfg.ElectrsURL,
			Active:            true,
		}}
	}
	k.loadKeys()
	k.loadAccounts()
	return k, nil
}

// SetRedeemScript records the env vault's redeem script.
func (k *Keeper) SetRedeemScript(redeemHex string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	w := k.wallets[k.cfg.VaultAddress]
	if w == nil {
		return
	}
	w.view.RedeemScript = redeemHex
	w.view.Required = k.cfg.RequiredSigs
	if b, err := hex.DecodeString(redeemHex); err == nil {
		pubs := chain.ParsePubkeysFromRedeem(b)
		w.view.Pubkeys = make([]string, 0, len(pubs))
		for _, p := range pubs {
			w.view.Pubkeys = append(w.view.Pubkeys, hex.EncodeToString(p))
		}
	}
}

// LoadState restores proposals from the persisted state file.
func (k *Keeper) LoadState() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.loadLocked()
}

// SetP2PNode attaches the libp2p node and its message handlers.
func (k *Keeper) SetP2PNode(n *p2p.Node) {
	k.node = n
	n.SetHandlers(p2p.Handlers{
		OnProposeTransfer: k.onProposeTransfer,
		OnTransferApprove: k.onTransferApprove,
		OnBroadcast:       k.onBroadcast,
		OnReject:          k.onReject,
		OnRequestProposal: k.onRequestProposal,
		OnHello:           k.onHello,
	})
}

func (k *Keeper) onHello(h p2p.Hello) error {
	expectedChecksum := k.cfg.P2PChecksum()
	if h.VaultHash != "" && h.VaultHash != expectedChecksum {
		log.Printf("[p2p] CRITICAL SECURITY ALERT: peer %s index %d reported consensus checksum %s, expected %s (node config mismatch!)",
			h.PeerID, h.KeeperIndex, h.VaultHash, expectedChecksum)
		return fmt.Errorf("peer consensus checksum mismatch")
	}
	return nil
}

// SelfView describes this node for the status endpoint.
type SelfView struct {
	SelfIndex     int    `json:"selfIndex"`
	PubHex        string `json:"pubkey"`
	PeerID        string `json:"peerId"`
	AutoSign      bool   `json:"autoSign"`
	AutoBroadcast bool   `json:"autoBroadcast"`
	RejectCount   int    `json:"openRejects,omitempty"`
}

func (k *Keeper) Self() SelfView {
	k.mu.Lock()
	rejects := 0
	for _, st := range k.props {
		rejects += len(st.rejects)
	}
	k.mu.Unlock()
	v := SelfView{
		SelfIndex:     k.cfg.SelfIndex,
		PubHex:        k.pubHex,
		AutoSign:      k.cfg.AutoSign,
		AutoBroadcast: k.cfg.AutoBroadcast,
		RejectCount:   rejects,
	}
	if k.node != nil {
		v.PeerID = k.node.ID()
	}
	return v
}

// --- proposal actions ------------------------------------------------------

// createLocked installs a NEW proposal state (must be verified already).
func (k *Keeper) createLocked(id, vault string, transfer *p2p.TransferProposal, rawForSigning string, mySigned bool, verifyErr string) *propState {
	now := time.Now()
	st := &propState{
		id:            id,
		vault:         vault,
		transfer:      transfer,
		rawForSigning: rawForSigning,
		mySigned:      mySigned,
		verifyErr:     verifyErr,
		sigs:          make(map[string]bool),
		rejects:       make(map[string]string),
		created:       now,
		updated:       now,
	}
	if mySigned {
		st.sigs[k.pubHex] = true
	}
	k.props[id] = st
	k.trimLocked()
	k.persistLocked()
	return st
}

// trimLocked bounds stale/rejected state accumulation from rogue peers.
func (k *Keeper) trimLocked() {
	const maxTracked = 800
	for len(k.props) <= maxTracked {
		return
	}
	// evict completed/rejected entries, oldest-updated first
	for _, id := range k.oldestDoneLocked() {
		delete(k.props, id)
		if len(k.props) <= maxTracked {
			return
		}
	}
}

func (k *Keeper) oldestDoneLocked() []string {
	var done []string
	for id, st := range k.props {
		if st.complete || st.verifyErr != "" {
			done = append(done, id)
		}
	}
	// newest-partition: stable sort by updated desc so we delete oldest first
	for i := 1; i < len(done); i++ {
		for j := i; j > 0 && k.props[done[j]].updated.Before(k.props[done[j-1]].updated); j-- {
			done[j], done[j-1] = done[j-1], done[j]
		}
	}
	return done
}

// ApproveProposal signs (or attaches this node's signature to) a pending
// proposal; returns the merged tx hex.
func (k *Keeper) ApproveProposal(id string) (string, error) {
	k.mu.Lock()
	st := k.props[id]
	if st == nil {
		k.mu.Unlock()
		return "", fmt.Errorf("no proposal %s", id)
	}
	if st.verifyErr != "" {
		k.mu.Unlock()
		return "", fmt.Errorf("refusing to sign %s: failed local verification: %s", id, st.verifyErr)
	}
	if st.complete {
		raw := st.signedHex
		k.mu.Unlock()
		return raw, nil
	}
	raw := st.rawForSigning
	if raw == "" {
		raw = k.rawForSigningLocked(st)
	}
	if raw == "" {
		k.mu.Unlock()
		return "", fmt.Errorf("proposal %s has no raw tx", id)
	}
	w := k.accountByAddressLocked(st.vault)
	if w == nil {
		k.mu.Unlock()
		return "", fmt.Errorf("proposal %s belongs to unknown wallet %s", id, st.vault)
	}
	key, _ := k.walletKeyLocked(w)
	if key == nil {
		k.mu.Unlock()
		return "", fmt.Errorf("wallet %s has no signing key on this node; add a private key in the UI", w.view.Name)
	}
	redeem := w.view.RedeemScript
	required := w.view.Required
	k.mu.Unlock()
	// cosign outside the lock: ECDSA + DER merge is CPU-bound
	merged, complete, err := chain.CosignMultisigTx(raw, redeem, []*btcec.PrivateKey{key.priv}, required)
	if err != nil {
		return "", fmt.Errorf("sign %s: %w", id, err)
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	st = k.props[id]
	if st == nil {
		return "", fmt.Errorf("no proposal %s", id)
	}
	k.markSignedLocked(st, merged)
	k.broadcastApprovalLocked(st, merged)
	if complete {
		k.finalizeCompleteLocked(st, merged)
	}
	return merged, nil
}

// RejectProposal records an informational rejection and tells the peers.
func (k *Keeper) RejectProposal(id, reason string) error {
	if reason == "" {
		reason = "rejected by operator"
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.props[id]
	if st == nil {
		return fmt.Errorf("no proposal %s", id)
	}
	st.rejects[k.pubHex] = reason
	st.updated = time.Now()
	k.persistLocked()
	if k.node != nil {
		k.node.Broadcast(p2p.TypeReject, p2p.RejectMsg{ProposalId: id, KeeperId: k.pubHex, Reason: reason})
	}
	log.Printf("[reject] %s by us (%.8s): %s", id, k.pubHex, reason)
	return nil
}

// BroadcastProposal pushes a fully-signed tx to the chain exactly once per
// node to guarantee atomicity of the SUCCEEDED marker.
func (k *Keeper) BroadcastProposal(id string) (string, error) {
	k.mu.Lock()
	st := k.props[id]
	if st == nil {
		k.mu.Unlock()
		return "", fmt.Errorf("no proposal %s", id)
	}
	if !st.complete {
		k.mu.Unlock()
		return "", fmt.Errorf("proposal %s is not complete (need 2-of-3 signatures)", id)
	}
	if st.broadcasting {
		k.mu.Unlock()
		return "", fmt.Errorf("broadcast already in progress for %s", id)
	}
	if st.broadcasted {
		txid := st.broadcastTxid
		k.mu.Unlock()
		return txid, nil
	}
	raw := st.signedHex
	if raw == "" {
		raw = k.rawForSigningLocked(st)
	}
	st.broadcasting = true
	k.mu.Unlock()

	txid, berr := k.chain.Broadcast(raw)

	k.mu.Lock()
	defer k.mu.Unlock()
	st.broadcasting = false
	st.updated = time.Now()
	if berr != nil {
		st.broadcastErr = berr.Error()
		k.persistLocked()
		return "", fmt.Errorf("broadcast %s: %w", id, berr)
	}
	st.broadcasted = true
	st.broadcastTxid = txid
	k.persistLocked()
	if k.node != nil {
		k.node.Broadcast(p2p.TypeTransferBroadcast, p2p.BroadcastMsg{ProposalId: id, Txid: txid})
	}
	log.Printf("[broadcast] %s -> %s", id, txid)
	return txid, nil
}

// broadcastApprovalLocked sends the merged hex to peers.
func (k *Keeper) broadcastApprovalLocked(st *propState, merged string) {
	if k.node == nil {
		return
	}
	k.node.Broadcast(p2p.TypeTransferApprove, p2p.TransferApprove{ProposalId: st.id, KeeperId: k.pubHex, SigHex: merged})
}

// markSignedLocked stores our merge and updates sig book-keeping.
func (k *Keeper) markSignedLocked(st *propState, merged string) {
	st.signedHex = merged
	st.mySigned = true
	st.sigs[k.pubHex] = true
	st.updated = time.Now()
	k.persistLocked()
}

// finalizeCompleteLocked applies the broadcast policy to a complete proposal.
func (k *Keeper) finalizeCompleteLocked(st *propState, merged string) {
	st.complete = true
	st.updated = time.Now()
	if k.cfg.AutoBroadcast {
		go func() {
			if _, err := k.BroadcastProposal(st.id); err != nil {
				log.Printf("[broadcast] auto %s: %v", st.id, err)
			}
		}()
		return
	}
	// manual policy: keep READY; re-persist for the UI
	k.persistLocked()
	log.Printf("[policy] %s complete (2-of-3); waiting for manual broadcast", st.id)
}

// --- incoming message handling ---------------------------------------------

func (k *Keeper) onTransferApprove(app p2p.TransferApprove) error {
	return k.onApproval(app.ProposalId, app.KeeperId, app.SigHex)
}

func (k *Keeper) onApproval(id, from, sigHex string) error {
	// Recovery: if we never received the proposal body, ask the cluster for it.
	k.mu.Lock()
	st := k.props[id]
	redeem := ""
	required := 2
	var signKey *btcec.PrivateKey
	if st != nil {
		if w := k.accountByAddressLocked(st.vault); w != nil {
			redeem = w.view.RedeemScript
			required = w.view.Required
			if key, _ := k.walletKeyLocked(w); key != nil {
				signKey = key.priv
			}
		}
	}
	k.mu.Unlock()
	if st == nil {
		k.requestProposal(id)
		return nil
	}
	if redeem == "" {
		log.Printf("[approve] %s: unknown wallet %s", id, st.vault)
		return nil
	}

	// Merge (adds any signature the sender had that we lack).
	merged, complete, err := chain.CosignMultisigTx(sigHex, redeem, nil, required)
	if err != nil {
		log.Printf("[approve] merge %s from %s: %v", id, from, err)
		return nil
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	st = k.props[id]
	if st == nil {
		return nil
	}
	if st.complete {
		return nil
	}
	st.sigs[from] = true
	st.updated = time.Now()
	// Prefer the merged result with the most signatures (ours + theirs).
	if st.mySigned && signKey != nil {
		// Both flows merged identical sig sets; store the self-signed variant
		// to guarantee eager local cosign didn't regress: run cosign ordering.
		if my, _, merr := chain.CosignMultisigTx(merged, redeem, []*btcec.PrivateKey{signKey}, required); merr == nil && my != "" {
			merged = my
		}
	}
	st.signedHex = merged
	k.persistLocked()
	if complete {
		k.finalizeCompleteLocked(st, merged)
	}
	return nil
}

// onBroadcast marks a proposal as broadcast when a peer already pushed it.
func (k *Keeper) onBroadcast(m p2p.BroadcastMsg) error {
	if m.ProposalId == "" || m.Txid == "" {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.props[m.ProposalId]
	if st == nil || st.broadcasted {
		return nil
	}
	st.broadcasted = true
	st.broadcastTxid = m.Txid
	st.updated = time.Now()
	k.persistLocked()
	log.Printf("[broadcast] %s confirmed by peer -> %s", m.ProposalId, m.Txid)
	return nil
}

// onReject records a peer's informational rejection.
func (k *Keeper) onReject(r p2p.RejectMsg) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.props[r.ProposalId]
	if st == nil {
		return nil
	}
	if r.KeeperId == "" {
		return nil
	}
	st.rejects[r.KeeperId] = r.Reason
	st.updated = time.Now()
	k.persistLocked()
	log.Printf("[reject] %s by %s: %s", r.ProposalId, r.KeeperId[:min(8, len(r.KeeperId))], r.Reason)
	return nil
}

// onRequestProposal re-broadcasts the original proposal to peers who missed
// it (they will short-circuit in onProposeTransfer via the seen map).
func (k *Keeper) onRequestProposal(req p2p.RequestProposal) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.props[req.ProposalId]
	if st == nil || st.transfer == nil || k.node == nil {
		return nil
	}
	k.node.Broadcast(p2p.TypeProposeTransfer, *st.transfer)
	log.Printf("[p2p] re-proposed %s to the cluster", req.ProposalId)
	return nil
}

// requestProposal asks the cluster to re-send a proposal body we missed.
func (k *Keeper) requestProposal(id string) {
	k.mu.Lock()
	if t, ok := k.reqLog[id]; ok && time.Since(t) < 2*time.Minute {
		k.mu.Unlock()
		return
	}
	k.reqLog[id] = time.Now()
	k.mu.Unlock()
	if k.node == nil {
		return
	}
	k.node.Broadcast(p2p.TypeRequestProposal, p2p.RequestProposal{ProposalId: id})
	log.Printf("[p2p] requesting proposal body for %s (we only saw the approval)", id)
}

// min is the small helper used in logs.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (k *Keeper) rawForSigningLocked(st *propState) string {
	if st.rawForSigning != "" {
		return st.rawForSigning
	}
	if st.transfer != nil {
		return st.transfer.RawTxHex
	}
	return ""
}

// --- views for the API/UI ---------------------------------------------------

// PropInput/PropOutput are UI-friendly parsed forms of a raw tx.
type PropInput struct {
	Txid          string `json:"txid"`
	Vout          uint32 `json:"vout"`
	ValueSats     int64  `json:"valueSats"`
	Confirmations int64  `json:"confirmations"`
}

type PropOutput struct {
	To          string `json:"to,omitempty"`
	OpReturnHex string `json:"opReturnHex,omitempty"`
	ValueSats   int64  `json:"valueSats"`
}

type ProposalView struct {
	Id    string `json:"proposalId"`
	Vault string `json:"vault"`

	To      string `json:"to,omitempty"`
	Amount  int64  `json:"amountSats,omitempty"`
	Memo    string `json:"memo,omitempty"`
	Gross   int64  `json:"grossSats,omitempty"`
	Fee     int64  `json:"feeSats,omitempty"`
	Txid    string `json:"txid"`
	RawHex  string
	Inputs  []PropInput  `json:"inputs"`
	Outputs []PropOutput `json:"outputs"`

	Status string `json:"status"`
	// pending            : arrived, not signed by us (verification result freshest)
	// signed-partial     : we + maybe peers signed but <2-of-3
	// verify-rejected    : failed local checks (we will never sign)
	// ready              : complete (2-of-3), awaiting manual broadcast
	// broadcasted        : on chain
	// broadcast-error    : last broadcast attempt failed

	MySigned     bool              `json:"mySigned"`
	Complete     bool              `json:"complete"`
	Broadcasted  bool              `json:"broadcasted"`
	BroadcastErr string            `json:"broadcastErr,omitempty"`
	BroadcastTx  string            `json:"broadcastTxid,omitempty"`
	SignedBy     []string          `json:"signedBy"`
	Rejects      map[string]string `json:"rejects"`
	VerifyErr    string            `json:"verifyErr,omitempty"`

	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// terminal reports whether this proposal can no longer be reused for an
// identical transfer intent.
func (st *propState) terminal() bool {
	return st.broadcasted || st.verifyErr != ""
}

func (st *propState) status() string {
	switch {
	case st.verifyErr != "":
		return "verify-rejected"
	case st.complete && st.broadcasted:
		return "broadcasted"
	case st.complete && st.broadcastErr != "":
		return "broadcast-error"
	case st.complete:
		return "ready"
	case st.mySigned:
		return "signed-partial"
	default:
		return "pending"
	}
}

// viewLocked materializes the UI view of a state (parsed inputs/outputs).
func (k *Keeper) viewLocked(st *propState) ProposalView {
	v := ProposalView{
		Id:           st.id,
		Vault:        st.vault,
		Status:       st.status(),
		MySigned:     st.mySigned,
		Complete:     st.complete,
		Broadcasted:  st.broadcasted,
		BroadcastErr: st.broadcastErr,
		BroadcastTx:  st.broadcastTxid,
		VerifyErr:    st.verifyErr,
		SignedBy:     make([]string, 0, len(st.sigs)),
		Rejects:      map[string]string{},
		Created:      st.created,
		Updated:      st.updated,
	}
	for id := range st.sigs {
		v.SignedBy = append(v.SignedBy, id)
	}
	for id, r := range st.rejects {
		v.Rejects[id] = r
	}

	p := st.transfer
	if p == nil {
		return v
	}
	v.To = p.To
	v.Amount = p.AmountSats
	v.Memo = p.Memo
	v.Gross = p.GrossSats
	v.Fee = p.FeeSats
	v.Txid = p.Txid

	raw := p.RawTxHex
	if st.signedHex != "" {
		raw = st.signedHex
	}
	v.RawHex = raw
	ins, err := chain.ParseInputsFromRawHex(raw)
	if err == nil {
		for _, in := range ins {
			v.Inputs = append(v.Inputs, PropInput{Txid: in.Txid, Vout: in.Vout})
		}
	}
	outs, err := chain.ParseOutputsFromRawHex(raw)
	if err == nil {
		for _, o := range outs {
			v.Outputs = append(v.Outputs, PropOutput{To: o.Address, OpReturnHex: o.OpReturnHex, ValueSats: o.ValueSats})
		}
	}
	return v
}

// ListProposals returns all known proposals in stable (newest first) order.
func (k *Keeper) ListProposals() []ProposalView {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]ProposalView, 0, len(k.props))
	for _, st := range k.props {
		out = append(out, k.viewLocked(st))
	}
	sortViews(out)
	return out
}

func sortViews(vs []ProposalView) {
	for i := 1; i < len(vs); i++ {
		for j := i; j > 0 && vs[j].Updated.After(vs[j-1].Updated); j-- {
			vs[j], vs[j-1] = vs[j-1], vs[j]
		}
	}
}

// GetProposal returns one proposal view.
func (k *Keeper) GetProposal(id string) (ProposalView, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.props[id]
	if st == nil {
		return ProposalView{}, fmt.Errorf("no proposal %s", id)
	}
	return k.viewLocked(st), nil
}

// ListUTXOs exposes the table of vault outputs for the UI.
func (k *Keeper) ListUTXOs(walletAddr string) ([]chain.UTXO, error) {
	if walletAddr == "" {
		walletAddr = k.cfg.VaultAddress
	}
	return k.chain.GetUTXOs(walletAddr, 1)
}

// UTXOCount returns the vault's unspent output count from the backend's O(1)
// address summary (includes the <=maturity coinbase tail).
func (k *Keeper) UTXOCount(walletAddr string) int {
	if walletAddr == "" {
		walletAddr = k.cfg.VaultAddress
	}
	st, err := k.chain.GetAddressStats(walletAddr)
	if err != nil || st == nil {
		return 0
	}
	return st.UnspentCount
}

// Balance returns the total unspent value of the vault using the backend's
// O(1) address summary. It includes the <=maturity coinbase tail; spending
// proposals still re-fetch and filter per-UTXO maturity before use.
func (k *Keeper) Balance(walletAddr string) int64 {
	if walletAddr == "" {
		walletAddr = k.cfg.VaultAddress
	}
	st, err := k.chain.GetAddressStats(walletAddr)
	if err != nil || st == nil {
		return 0
	}
	return st.UnspentValue
}

// Peers returns connected peer IDs.
func (k *Keeper) Peers() []string {
	if k.node == nil {
		return nil
	}
	return k.node.Peers()
}

// ChainTip exposes the current tip height for the status endpoint.
func (k *Keeper) ChainTip() (int64, string) {
	h, err := k.chain.GetTipHeight()
	if err != nil {
		return -1, ""
	}
	hash, _ := k.chain.GetTipHash()
	return h, hash
}

// --- persistence -------------------------------------------------------------

// persistProp is the on-disk projection of a propState (schema v2, transfers
// only; v1 sweep entries are ignored on load).
type persistProp struct {
	Id    string `json:"id"`
	Vault string `json:"vault,omitempty"`

	Transfer *p2p.TransferProposal `json:"transfer,omitempty"`

	RawForSigning string `json:"rawForSigning"`
	SignedHex     string `json:"signedHex"`
	MySigned      bool   `json:"mySigned"`
	Complete      bool   `json:"complete"`
	VerifyErr     string `json:"verifyErr,omitempty"`

	Broadcasted   bool   `json:"broadcasted"`
	BroadcastTxid string `json:"broadcastTxid,omitempty"`
	BroadcastErr  string `json:"broadcastErr,omitempty"`

	Sigs    []string          `json:"sigs"`
	Rejects map[string]string `json:"rejects"`

	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

type persistFile struct {
	Version int           `json:"version"`
	Props   []persistProp `json:"proposals"`
}

// persistLocked writes the state file atomically (tmp + rename).
func (k *Keeper) persistLocked() {
	if k.cfg.StateFile == "" {
		return
	}
	f := persistFile{Version: 3}
	for _, st := range k.props {
		p := persistProp{
			Id:            st.id,
			Vault:         st.vault,
			Transfer:      st.transfer,
			RawForSigning: st.rawForSigning,
			SignedHex:     st.signedHex,
			MySigned:      st.mySigned,
			Complete:      st.complete,
			VerifyErr:     st.verifyErr,
			Broadcasted:   st.broadcasted,
			BroadcastTxid: st.broadcastTxid,
			BroadcastErr:  st.broadcastErr,
			Sigs:          []string{},
			Rejects:       map[string]string{},
			Created:       st.created,
			Updated:       st.updated,
		}
		for id := range st.sigs {
			p.Sigs = append(p.Sigs, id)
		}
		for id, r := range st.rejects {
			p.Rejects[id] = r
		}
		f.Props = append(f.Props, p)
	}
	data, err := json.Marshal(f)
	if err != nil {
		log.Printf("[state] marshal: %v", err)
		return
	}
	tmp := k.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("[state] write: %v", err)
		return
	}
	if err := os.Rename(tmp, k.cfg.StateFile); err != nil {
		log.Printf("[state] rename: %v", err)
	}
}

func (k *Keeper) loadLocked() error {
	if k.cfg.StateFile == "" {
		return nil
	}
	data, err := os.ReadFile(k.cfg.StateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("state read: %w", err)
	}
	var f persistFile
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("[state] corrupt state file ignored: %v", err)
		return nil
	}
	if k.props == nil {
		k.props = make(map[string]*propState)
	}
	now := time.Now()
	skipped := 0
	for _, p := range f.Props {
		if p.Id == "" || p.Transfer == nil {
			skipped++ // legacy sweep entry or corrupt record
			continue
		}
		vault := p.Vault
		if vault == "" {
			vault = k.cfg.VaultAddress
		}
		st := &propState{
			id:            p.Id,
			vault:         vault,
			transfer:      p.Transfer,
			rawForSigning: p.RawForSigning,
			signedHex:     p.SignedHex,
			mySigned:      p.MySigned,
			complete:      p.Complete,
			verifyErr:     p.VerifyErr,
			broadcasted:   p.Broadcasted,
			broadcastTxid: p.BroadcastTxid,
			broadcastErr:  p.BroadcastErr,
			sigs:          map[string]bool{},
			rejects:       map[string]string{},
			created:       p.Created,
		}
		for _, s := range p.Sigs {
			st.sigs[s] = true
		}
		for id, r := range p.Rejects {
			st.rejects[id] = r
		}
		if st.created.IsZero() {
			st.created = now
		}
		st.updated = p.Updated
		if st.updated.IsZero() {
			st.updated = now
		}
		k.props[st.id] = st
	}
	if skipped > 0 {
		log.Printf("[state] loaded %d proposals from %s (skipped %d legacy entries)", len(k.props), k.cfg.StateFile, skipped)
	} else {
		log.Printf("[state] loaded %d proposals from %s", len(k.props), k.cfg.StateFile)
	}
	return nil
}
