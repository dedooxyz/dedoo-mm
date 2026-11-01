package processor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dedooxyz/dedoo-mm/internal/chain"

	"github.com/btcsuite/btcd/btcec/v2"
)

// signingKey is one WIF held by this node. A key can sign for every wallet
// whose redeem script contains its public key.
type signingKey struct {
	wif   string
	priv  *btcec.PrivateKey
	env   bool // loaded from KEEPER_WIF
	added time.Time
}

// KeyView is the public description of a signing key (never the WIF).
type KeyView struct {
	Pubkey string    `json:"pubkey"`
	Env    bool      `json:"env"`
	Added  time.Time `json:"added"`
}

// AccountView is one multisig wallet managed by this node.
type AccountView struct {
	Id       string `json:"id"`
	Name     string `json:"name"`
	Chain    string `json:"chain"`
	ChainKey string `json:"chainKey"`

	Address      string   `json:"address"`
	RedeemScript string   `json:"redeemScript"`
	Required     int      `json:"required"`
	Pubkeys      []string `json:"pubkeys,omitempty"`
	Order        []int    `json:"order,omitempty"`

	AddrPubkeyVersion int    `json:"addrPubkeyVersion"`
	AddrScriptVersion int    `json:"addrScriptVersion"`
	WifVersion        int    `json:"wifVersion"`
	CoinbaseMaturity  int64  `json:"coinbaseMaturity"`
	ElectrsURL        string `json:"electrsUrl,omitempty"`

	Active    bool      `json:"active"`
	HasKey    bool      `json:"hasKey"`
	KeyPubkey string    `json:"keyPubkey,omitempty"`
	Created   time.Time `json:"created"`
}

// CreateAccountRequest derives and saves a wallet on the node's fixed chain.
type CreateAccountRequest struct {
	Name  string `json:"name"`
	Chain string `json:"chain"`

	ChainName         string `json:"chainName"`
	AddrPubkeyVersion *int   `json:"addrPubkeyVersion"`
	AddrScriptVersion *int   `json:"addrScriptVersion"`
	WifVersion        *int   `json:"wifVersion"`
	CoinbaseMaturity  *int64 `json:"coinbaseMaturity"`
	ElectrsURL        string `json:"electrsUrl"`

	Pubkeys      []string `json:"pubkeys"`
	Order        []int    `json:"order"`
	Required     int      `json:"required"`
	RedeemScript string   `json:"redeemScript"`

	// Optional: add this WIF as a node signing key while creating the wallet.
	WIF string `json:"wif"`
}

type wallet struct {
	view AccountView
}

type storedAccount struct {
	AccountView
}

type accountsFile struct {
	Version  int             `json:"version"`
	Accounts []storedAccount `json:"accounts"`
}

type storedKey struct {
	WIF   string    `json:"wif"`
	Added time.Time `json:"added"`
}

type keysFile struct {
	Version int         `json:"version"`
	Keys    []storedKey `json:"keys"`
}

func accountID(address string) string {
	sum := sha256.Sum256([]byte(address))
	return "acct_" + hex.EncodeToString(sum[:])[:12]
}

func chainKeyFor(p chain.NetParams) string {
	for _, info := range chain.ChainInfos() {
		if info.Name == p.Name {
			return info.Name
		}
	}
	return "custom"
}

// --- wallet lookup ----------------------------------------------------------

func (k *Keeper) accountByAddressLocked(addr string) *wallet {
	return k.wallets[addr]
}

func (k *Keeper) accountByIDLocked(id string) *wallet {
	if w := k.wallets[id]; w != nil {
		return w
	}
	for _, w := range k.wallets {
		if w.view.Id == id {
			return w
		}
	}
	return nil
}

// walletKeyLocked returns the signing key matching this wallet, if any.
func (k *Keeper) walletKeyLocked(w *wallet) (*signingKey, string) {
	if w == nil {
		return nil, ""
	}
	redeemBytes, err := hex.DecodeString(w.view.RedeemScript)
	if err != nil {
		return nil, ""
	}
	pubs := chain.ParsePubkeysFromRedeem(redeemBytes)
	for _, key := range k.keys {
		pub := key.priv.PubKey().SerializeCompressed()
		for _, p := range pubs {
			if string(p) == string(pub) {
				return key, hex.EncodeToString(pub)
			}
		}
	}
	return nil, ""
}

func (k *Keeper) walletViewLocked(w *wallet) AccountView {
	v := w.view
	if _, pub := k.walletKeyLocked(w); pub != "" {
		v.HasKey = true
		v.KeyPubkey = pub
	}
	return v
}

// --- wallets ----------------------------------------------------------------

func (k *Keeper) ListAccounts() []AccountView {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]AccountView, 0, len(k.wallets))
	for _, w := range k.wallets {
		out = append(out, k.walletViewLocked(w))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		return out[i].Created.Before(out[j].Created)
	})
	return out
}

func (k *Keeper) GetAccount(idOrAddress string) (AccountView, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	w := k.accountByIDLocked(idOrAddress)
	if w == nil {
		return AccountView{}, fmt.Errorf("no wallet %s", idOrAddress)
	}
	return k.walletViewLocked(w), nil
}

func (k *Keeper) CreateAccount(req CreateAccountRequest) (AccountView, bool, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return AccountView{}, false, fmt.Errorf("wallet name is required")
	}
	if len(name) > 64 {
		return AccountView{}, false, fmt.Errorf("wallet name is too long")
	}
	if err := k.validateChainRequest(req); err != nil {
		return AccountView{}, false, err
	}
	params := k.cfg.NetParams

	required := req.Required
	var redeem string
	providedRedeem := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(req.RedeemScript), "0x"))
	if providedRedeem != "" {
		if _, err := hex.DecodeString(providedRedeem); err != nil {
			return AccountView{}, false, fmt.Errorf("redeemScript is not valid hex")
		}
		m, err := chain.ParseMultisigThreshold(providedRedeem)
		if err != nil {
			return AccountView{}, false, fmt.Errorf("redeemScript: %w", err)
		}
		if required == 0 {
			required = m
		} else if required != m {
			return AccountView{}, false, fmt.Errorf("required (%d) does not match redeem script threshold (%d)", required, m)
		}
		redeem = providedRedeem
	}
	if len(req.Pubkeys) > 0 {
		if len(req.Pubkeys) != 3 {
			return AccountView{}, false, fmt.Errorf("exactly 3 public keys are required")
		}
		for i, pub := range req.Pubkeys {
			b, err := hex.DecodeString(strings.TrimSpace(pub))
			if err != nil || len(b) != 33 {
				return AccountView{}, false, fmt.Errorf("public key %d must be 33-byte hex", i+1)
			}
		}
		order := req.Order
		if len(order) == 0 {
			order = []int{0, 1, 2}
		}
		if required == 0 {
			required = 2
		}
		built, err := chain.BuildMultisigRedeemScript(req.Pubkeys, order, required)
		if err != nil {
			return AccountView{}, false, err
		}
		if redeem != "" && redeem != built {
			return AccountView{}, false, fmt.Errorf("redeemScript does not match the supplied public keys/order")
		}
		redeem = built
	}
	if redeem == "" {
		return AccountView{}, false, fmt.Errorf("provide 3 public keys or a redeem script")
	}
	if required < 1 || required > 3 {
		return AccountView{}, false, fmt.Errorf("required signatures must be 1..3")
	}
	address, err := chain.P2SHAddressForVersion(redeem, params.AddrScriptVersion)
	if err != nil {
		return AccountView{}, false, err
	}

	// A WIF supplied with the wallet becomes a node-wide signing key.
	if strings.TrimSpace(req.WIF) != "" {
		if _, _, err := k.AddSigningKey(req.WIF); err != nil {
			return AccountView{}, false, err
		}
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	if existing := k.wallets[address]; existing != nil {
		return k.walletViewLocked(existing), false, nil
	}
	w := &wallet{view: AccountView{
		Id:                accountID(address),
		Name:              name,
		Chain:             k.cfg.ChainName,
		ChainKey:          chainKeyFor(params),
		Address:           address,
		RedeemScript:      redeem,
		Required:          required,
		Pubkeys:           cleanPubkeys(req.Pubkeys),
		Order:             append([]int(nil), req.Order...),
		AddrPubkeyVersion: int(params.AddrPubkeyVersion),
		AddrScriptVersion: int(params.AddrScriptVersion),
		WifVersion:        int(params.WifVersion),
		CoinbaseMaturity:  k.cfg.CoinbaseMaturity,
		ElectrsURL:        k.cfg.ElectrsURL,
		Created:           time.Now(),
	}}
	k.wallets[address] = w
	k.persistAccountsLocked()
	log.Printf("[accounts] saved wallet %s (%s, %s)", name, address, k.cfg.ChainName)
	return k.walletViewLocked(w), true, nil
}

// validateChainRequest enforces one chain per node.
func (k *Keeper) validateChainRequest(req CreateAccountRequest) error {
	params := k.cfg.NetParams
	if req.Chain != "" && !strings.EqualFold(strings.TrimSpace(req.Chain), "custom") {
		p, err := chain.LookupNetParams(req.Chain)
		if err != nil {
			return err
		}
		if p.Name != k.cfg.ChainName {
			return fmt.Errorf("this node is restricted to CHAIN=%s; cannot add a %s wallet", k.cfg.ChainName, p.Name)
		}
	}
	if req.ChainName != "" && !strings.EqualFold(strings.TrimSpace(req.ChainName), k.cfg.ChainName) {
		return fmt.Errorf("this node is restricted to CHAIN=%s; got %q", k.cfg.ChainName, req.ChainName)
	}
	checks := []struct {
		label string
		got   *int
		want  int
	}{
		{"addrPubkeyVersion", req.AddrPubkeyVersion, int(params.AddrPubkeyVersion)},
		{"addrScriptVersion", req.AddrScriptVersion, int(params.AddrScriptVersion)},
		{"wifVersion", req.WifVersion, int(params.WifVersion)},
	}
	for _, c := range checks {
		if c.got != nil && *c.got != c.want {
			return fmt.Errorf("%s=%d does not match this node's chain (%d)", c.label, *c.got, c.want)
		}
	}
	if req.CoinbaseMaturity != nil && *req.CoinbaseMaturity != k.cfg.CoinbaseMaturity {
		return fmt.Errorf("coinbaseMaturity=%d does not match this node (%d)", *req.CoinbaseMaturity, k.cfg.CoinbaseMaturity)
	}
	return nil
}

func (k *Keeper) DeleteAccount(id string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	w := k.accountByIDLocked(id)
	if w == nil {
		return fmt.Errorf("no saved wallet %s", id)
	}
	if w.view.Active {
		return fmt.Errorf("the active vault is configured in keeper.env and cannot be removed")
	}
	delete(k.wallets, w.view.Address)
	k.persistAccountsLocked()
	log.Printf("[accounts] removed wallet %s (%s)", w.view.Name, w.view.Address)
	return nil
}

func cleanPubkeys(pubs []string) []string {
	out := make([]string, 0, len(pubs))
	for _, p := range pubs {
		out = append(out, strings.ToLower(strings.TrimSpace(p)))
	}
	return out
}

// --- signing keys ------------------------------------------------------------

func (k *Keeper) ListSigningKeys() []KeyView {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]KeyView, 0, len(k.keys))
	for pub, key := range k.keys {
		out = append(out, KeyView{Pubkey: pub, Env: key.env, Added: key.added})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Added.Before(out[j].Added) })
	return out
}

// AddSigningKey imports a WIF. The key is usable by every wallet whose redeem
// script contains its public key. It never leaves this node.
func (k *Keeper) AddSigningKey(wif string) (KeyView, bool, error) {
	priv, _, err := chain.LoadWifKey(strings.TrimSpace(wif))
	if err != nil {
		return KeyView{}, false, fmt.Errorf("WIF: %w", err)
	}
	pub := hex.EncodeToString(priv.PubKey().SerializeCompressed())
	k.mu.Lock()
	defer k.mu.Unlock()
	if existing, ok := k.keys[pub]; ok {
		return KeyView{Pubkey: pub, Env: existing.env, Added: existing.added}, false, nil
	}
	key := &signingKey{wif: strings.TrimSpace(wif), priv: priv, added: time.Now()}
	k.keys[pub] = key
	k.persistKeysLocked()
	log.Printf("[keys] added signing key %s", pub[:16])
	return KeyView{Pubkey: pub, Added: key.added}, true, nil
}

// RemoveSigningKey deletes an imported key. The env key is managed by
// keeper.env and cannot be removed here.
func (k *Keeper) RemoveSigningKey(pub string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	key := k.keys[pub]
	if key == nil {
		return fmt.Errorf("no signing key %s", pub)
	}
	if key.env {
		return fmt.Errorf("the KEEPER_WIF key comes from keeper.env and cannot be removed here")
	}
	delete(k.keys, pub)
	k.persistKeysLocked()
	log.Printf("[keys] removed signing key %s", pub[:16])
	return nil
}

func (k *Keeper) persistKeysLocked() {
	if k.cfg.KeysFile == "" {
		return
	}
	f := keysFile{Version: 1, Keys: make([]storedKey, 0, len(k.keys))}
	for _, key := range k.keys {
		if key.env {
			continue
		}
		f.Keys = append(f.Keys, storedKey{WIF: key.wif, Added: key.added})
	}
	data, err := json.Marshal(f)
	if err != nil {
		log.Printf("[keys] marshal: %v", err)
		return
	}
	tmp := k.cfg.KeysFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("[keys] write: %v", err)
		return
	}
	if err := os.Rename(tmp, k.cfg.KeysFile); err != nil {
		log.Printf("[keys] rename: %v", err)
	}
}

func (k *Keeper) loadKeys() {
	if k.cfg.KeysFile == "" {
		return
	}
	data, err := os.ReadFile(k.cfg.KeysFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[keys] read: %v", err)
		}
		return
	}
	var f keysFile
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("[keys] corrupt file ignored: %v", err)
		return
	}
	for _, rec := range f.Keys {
		if strings.TrimSpace(rec.WIF) == "" {
			continue
		}
		priv, _, err := chain.LoadWifKey(strings.TrimSpace(rec.WIF))
		if err != nil {
			log.Printf("[keys] skipping invalid stored key: %v", err)
			continue
		}
		pub := hex.EncodeToString(priv.PubKey().SerializeCompressed())
		if _, exists := k.keys[pub]; exists {
			continue
		}
		k.keys[pub] = &signingKey{wif: strings.TrimSpace(rec.WIF), priv: priv, added: rec.Added}
	}
	log.Printf("[keys] loaded %d imported signing keys from %s", len(k.keys), k.cfg.KeysFile)
}

// --- persistence -------------------------------------------------------------

func (k *Keeper) loadAccounts() {
	if k.cfg.AccountsFile == "" {
		return
	}
	data, err := os.ReadFile(k.cfg.AccountsFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[accounts] read: %v", err)
		}
		return
	}
	var f accountsFile
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("[accounts] corrupt file ignored: %v", err)
		return
	}
	loaded := 0
	for _, rec := range f.Accounts {
		if rec.Id == "" || rec.Address == "" {
			continue
		}
		// One node = one chain: ignore wallets saved under another chain or
		// with incompatible address versions.
		if rec.Chain != "" && !strings.EqualFold(rec.Chain, k.cfg.ChainName) {
			log.Printf("[accounts] skipping wallet %s from another chain (%s)", rec.Name, rec.Chain)
			continue
		}
		if rec.AddrScriptVersion != 0 && rec.AddrScriptVersion != int(k.cfg.NetParams.AddrScriptVersion) {
			log.Printf("[accounts] skipping wallet %s with script version %d", rec.Name, rec.AddrScriptVersion)
			continue
		}
		if _, exists := k.wallets[rec.Address]; exists {
			continue
		}
		k.wallets[rec.Address] = &wallet{view: rec.AccountView}
		loaded++
	}
	log.Printf("[accounts] loaded %d saved wallets from %s", loaded, k.cfg.AccountsFile)
}

func (k *Keeper) persistAccountsLocked() {
	if k.cfg.AccountsFile == "" {
		return
	}
	f := accountsFile{Version: 2, Accounts: make([]storedAccount, 0, len(k.wallets))}
	for _, w := range k.wallets {
		if w.view.Active {
			continue
		}
		f.Accounts = append(f.Accounts, storedAccount{AccountView: w.view})
	}
	sort.Slice(f.Accounts, func(i, j int) bool { return f.Accounts[i].Created.Before(f.Accounts[j].Created) })
	data, err := json.Marshal(f)
	if err != nil {
		log.Printf("[accounts] marshal: %v", err)
		return
	}
	tmp := k.cfg.AccountsFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("[accounts] write: %v", err)
		return
	}
	if err := os.Rename(tmp, k.cfg.AccountsFile); err != nil {
		log.Printf("[accounts] rename: %v", err)
	}
}
