// Package config loads Dedoo Multisig Manager runtime configuration from env.
// One process manages exactly ONE 2-of-3 multisig vault; the chain profile
// (CHAIN=wojakcoin|junkcoin) selects address/WIF versions.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/dedooxyz/dedoo-mm/internal/chain"
)

// Mode selects the process role.
type Mode string

const (
	ModeBootstrap Mode = "bootstrap" // public relay + DHT server, no signing keys
	ModeKeeper    Mode = "keeper"    // one of the 3 multisig signers
)

// MaxTxInputsLimit is the absolute configuration ceiling for one transaction.
// The effective limit is lower and wallet-specific: the chain relays at most
// MaxStandardTxBytes (100 KB), and ~333 P2SH 2-of-3 inputs fit in that size.
const MaxTxInputsLimit = chain.MaxTxInputs

// Config is the fully-resolved keeper configuration.
type Config struct {
	// Role
	Mode      Mode
	SelfIndex int // 0,1,2; ignored for bootstrap

	LogLevel string

	// Chain observation: "electrs" (default) or "rpc"
	Backend    string // "electrs" or "rpc"
	RPCURL     string
	RPCUser    string
	RPCPass    string
	ElectrsURL string

	ChainName        string
	NetParams        chain.NetParams
	CoinbaseMaturity int64
	ConfirmDepth     int64

	// Vault: the single 2-of-3 P2SH multisig wallet managed by this trio.
	VaultAddress string

	// RequiredSigs is the M in the M-of-3 redeem script (default 2).
	RequiredSigs int

	// Transfer policy
	MaxTxInputs     int   // cap inputs per transaction
	MinTransferSats int64 // dust floor for manual transfers

	// Signing
	KeeperWif string // this node's key in the 2-of-3

	// Automation policy (human-in-the-loop by default).
	AutoSign      bool // legacy: co-sign incoming proposals immediately
	AutoBroadcast bool // legacy: broadcast as soon as 2 signatures exist

	// Redeem script of the vault: supply the hex directly, or the 3 ceremony
	// pubkeys plus the key ordering and let the daemon derive it.
	RedeemScriptHex string
	PubKeys         []string
	KeyOrder        []int

	// p2p
	IsBootstrap   bool
	BindHost      string
	BindPort      int
	BootstrapAddr string // relay VPS multiaddr
	PeerAddrs     []string
	KeyFile       string
	UsePublicDHT  bool   // rendezvous discovery over the public IPFS DHT
	P2PGroup      string // stable trio/group name for DHT namespace + hello check

	// API
	ApiListen string
	// Optional HTTP Basic Auth for the web UI/API (both or neither).
	ApiUser string
	ApiPass string

	// Persistence
	StateFile    string
	AccountsFile string
	KeysFile     string
}

// ConsensusChecksum digests the parameters every keeper in the trio must
// share: chain profile, vault address and the vault redeem script.
func (c *Config) ConsensusChecksum(redeemHex string) string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%s|%s|%s",
		c.ChainName,
		strings.TrimSpace(c.VaultAddress),
		strings.ToLower(strings.TrimSpace(redeemHex)),
	)))
	return hex.EncodeToString(h.Sum(nil))
}

// P2PChecksum is the stable group fingerprint used for hello validation and
// the DHT rendezvous namespace. It must not depend on individual wallets,
// because wallets can be added to a running node.
func (c *Config) P2PChecksum() string {
	h := sha256.New()
	h.Write([]byte(c.ChainName + "|" + c.P2PGroup))
	return hex.EncodeToString(h.Sum(nil))
}

// Load reads env into Config. CHAIN selects the network profile
// ("wojakcoin" default, or "junkcoin"); ADDR_PUBKEY_VERSION,
// ADDR_SCRIPT_VERSION and WIF_VERSION remain available as explicit overrides
// for custom forks.
func Load() (*Config, error) {
	params, err := resolveNetParams()
	if err != nil {
		return nil, err
	}
	// Apply the profile to the package-level address/WIF encoders before any
	// address validation below and before keeper startup.
	chain.SelectNetParams(params)

	cbMaturity := getInt64("COINBASE_MATURITY", params.CoinbaseMaturity)
	if cbMaturity < params.CoinbaseMaturity {
		cbMaturity = params.CoinbaseMaturity
	}
	c := &Config{
		Mode:             Mode(get("KEEPER_MODE", string(ModeKeeper))),
		SelfIndex:        getInt("KEEPER_SELF_INDEX", 0),
		LogLevel:         get("LOG_LEVEL", "info"),
		Backend:          strings.ToLower(get("BACKEND", "electrs")),
		RPCURL:           get("WJK_RPC_URL", "http://127.0.0.1:8332"),
		RPCUser:          get("WJK_RPC_USER", ""),
		RPCPass:          get("WJK_RPC_PASS", ""),
		ElectrsURL:       get("ELECTRS_URL", params.DefaultElectrsURL),
		ChainName:        params.Name,
		NetParams:        params,
		CoinbaseMaturity: cbMaturity,
		ConfirmDepth:     getInt64("CONFIRM_DEPTH", params.CoinbaseMaturity),
		VaultAddress:     get("VAULT_ADDRESS", get("LP_VAULT_ADDRESS", "")),
		RequiredSigs:     getInt("REQUIRED_SIGS", 2),
		MaxTxInputs:      getInt("MAX_TX_INPUTS", getInt("SWEEP_MAX_UTXOS", 100)),
		MinTransferSats:  getInt64("MIN_TRANSFER_SATS", getInt64("DEV_MIN_RECEIVE", 10000)),
		KeeperWif:        get("KEEPER_WIF", ""),
		AutoSign:         getBool("AUTO_SIGN", false),
		AutoBroadcast:    getBool("AUTO_BROADCAST", false),
		IsBootstrap:      getBool("P2P_RELAY", false),
		BindHost:         get("P2P_BIND", "0.0.0.0"),
		BindPort:         getInt("P2P_PORT", 4000),
		BootstrapAddr:    get("P2P_BOOTSTRAP", ""),
		KeyFile:          get("P2P_KEYFILE", "keeper-identity.bin"),
		UsePublicDHT:     getBool("P2P_PUBLIC_DHT", true),
		P2PGroup:         get("P2P_GROUP", ""),
		ApiListen:        get("API_LISTEN", "127.0.0.1:8080"),
		ApiUser:          get("API_AUTH_USER", ""),
		ApiPass:          get("API_AUTH_PASS", ""),
		StateFile:        get("STATE_FILE", "keeper-state.json"),
		AccountsFile:     get("ACCOUNTS_FILE", "keeper-accounts.json"),
		KeysFile:         get("KEYS_FILE", "keeper-keys.json"),
		RedeemScriptHex:  get("REDEEM_SCRIPT_HEX", get("VAULT_REDEEM_SCRIPT", get("LP_REDEEM_SCRIPT_HEX", ""))),
	}
	if c.Mode != ModeBootstrap {
		if c.SelfIndex < 0 || c.SelfIndex > 2 {
			return nil, fmt.Errorf("config: KEEPER_SELF_INDEX must be 0..2, got %d", c.SelfIndex)
		}
		// KEEPER_WIF / VAULT_ADDRESS may be empty: the node then starts as a
		// wallet manager and wallets/keys can be added in the UI.
	}
	if c.RequiredSigs < 1 || c.RequiredSigs > 3 {
		return nil, fmt.Errorf("config: REQUIRED_SIGS must be 1..3, got %d", c.RequiredSigs)
	}
	if c.MaxTxInputs < 1 || c.MaxTxInputs > MaxTxInputsLimit {
		return nil, fmt.Errorf("config: MAX_TX_INPUTS must be 1..%d; the effective limit is further capped by the chain's %d-byte standard transaction size", MaxTxInputsLimit, chain.MaxStandardTxBytes)
	}
	if c.P2PGroup == "" {
		c.P2PGroup = c.ChainName
	}
	if c.Backend == "electrs" && strings.TrimSpace(c.ElectrsURL) == "" {
		return nil, fmt.Errorf("config: ELECTRS_URL is required when BACKEND=electrs (CHAIN=custom has no default endpoint)")
	}
	if (c.ApiUser == "") != (c.ApiPass == "") {
		return nil, fmt.Errorf("config: API_AUTH_USER and API_AUTH_PASS must be set together")
	}
	if c.ApiPass != "" && len(c.ApiPass) < 8 {
		return nil, fmt.Errorf("config: API_AUTH_PASS must be at least 8 characters")
	}
	if err := validateVaultAddress("VAULT_ADDRESS", c.VaultAddress, params); err != nil {
		return nil, err
	}
	if c.IsBootstrap {
		c.Mode = ModeBootstrap
	}
	if c.Mode == ModeBootstrap {
		c.IsBootstrap = true
	}
	if s := get("P2P_PEERS", ""); s != "" {
		c.PeerAddrs = splitComma(s)
	}
	if s := get("CEREMONY_PUBKEYS", ""); s != "" {
		c.PubKeys = splitComma(s)
		if len(c.PubKeys) != 3 {
			return nil, fmt.Errorf("config: CEREMONY_PUBKEYS must be exactly 3 pubkeys, got %d", len(c.PubKeys))
		}
	}
	c.KeyOrder = parseOrder(get("KEY_ORDER", get("BP_KEY_ORDER", "0,1,2")))
	return c, nil
}

// resolveNetParams selects the CHAIN profile. Presets provide convenience
// defaults; CHAIN=custom defines the chain entirely from .env, and the
// encoding/maturity values remain overridable for forks of either preset.
func resolveNetParams() (chain.NetParams, error) {
	name := get("CHAIN", chain.WojakCoinParams.Name)

	var p chain.NetParams
	if strings.EqualFold(strings.TrimSpace(name), "custom") {
		p = chain.NetParams{
			Name:           get("CHAIN_NAME", "custom"),
			Aliases:        nil,
			DefaultP2PPort: getInt("P2P_COIN_PORT", 0),
		}
		// A custom chain has no compiled-in knowledge: every consensus
		// encoding value is mandatory from .env.
		for _, o := range []struct {
			key string
			dst *byte
		}{
			{"ADDR_PUBKEY_VERSION", &p.AddrPubkeyVersion},
			{"ADDR_SCRIPT_VERSION", &p.AddrScriptVersion},
			{"WIF_VERSION", &p.WifVersion},
		} {
			if err := parseByteEnv(o.key, o.dst, true); err != nil {
				return chain.NetParams{}, err
			}
		}
		if s := get("COINBASE_MATURITY", ""); s == "" {
			return chain.NetParams{}, fmt.Errorf("config: CHAIN=custom requires COINBASE_MATURITY")
		}
		maturity, err := strconv.ParseInt(get("COINBASE_MATURITY", ""), 10, 64)
		if err != nil || maturity < 1 {
			return chain.NetParams{}, fmt.Errorf("config: COINBASE_MATURITY must be >= 1, got %q", get("COINBASE_MATURITY", ""))
		}
		p.CoinbaseMaturity = maturity
		p.DefaultElectrsURL = get("ELECTRS_URL", "")
		return p, nil
	}

	preset, err := chain.LookupNetParams(name)
	if err != nil {
		return chain.NetParams{}, fmt.Errorf("config: %w", err)
	}
	p = preset
	for _, o := range []struct {
		key string
		dst *byte
	}{
		{"ADDR_PUBKEY_VERSION", &p.AddrPubkeyVersion},
		{"ADDR_SCRIPT_VERSION", &p.AddrScriptVersion},
		{"WIF_VERSION", &p.WifVersion},
	} {
		if err := parseByteEnv(o.key, o.dst, false); err != nil {
			return chain.NetParams{}, err
		}
	}
	return p, nil
}

// parseByteEnv reads an optional (or required) 0..255 integer env var.
func parseByteEnv(key string, dst *byte, required bool) error {
	s := get(key, "")
	if s == "" {
		if required {
			return fmt.Errorf("config: %s is required (CHAIN=custom)", key)
		}
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 || v > 255 {
		return fmt.Errorf("config: %s must be 0..255, got %q", key, s)
	}
	*dst = byte(v)
	return nil
}

// validateVaultAddress requires an empty or valid P2SH address of this chain.
func validateVaultAddress(name, addr string, p chain.NetParams) error {
	if strings.TrimSpace(addr) == "" {
		return nil
	}
	ver, _, err := chain.DecodeAddress(addr)
	if err != nil {
		return fmt.Errorf("config: %s %q is not a valid base58check address: %w", name, addr, err)
	}
	if ver != p.AddrScriptVersion {
		return fmt.Errorf("config: %s %q has address version %d; CHAIN=%s vaults must use P2SH version %d", name, addr, ver, p.Name, p.AddrScriptVersion)
	}
	return nil
}

func parseOrder(s string) []int {
	var out []int
	for _, p := range splitComma(s) {
		v, err := strconv.Atoi(p)
		if err == nil {
			out = append(out, v)
		}
	}
	return out
}

// splitComma splits a comma-separated list, trimming whitespace.
func splitComma(s string) []string {
	parts := strings.Split(s, ",")
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ElectrsURLs returns all configured Electrs endpoints split by comma.
func (c *Config) ElectrsURLs() []string {
	var out []string
	for _, u := range strings.Split(c.ElectrsURL, ",") {
		u = strings.TrimSpace(u)
		if u != "" {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		out = []string{chain.CurrentNetParams().DefaultElectrsURL}
	}
	return out
}

// RPCURLs returns all configured RPC endpoints split by comma.
func (c *Config) RPCURLs() []string {
	var out []string
	for _, u := range strings.Split(c.RPCURL, ",") {
		u = strings.TrimSpace(u)
		if u != "" {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		out = []string{"http://127.0.0.1:8332"}
	}
	return out
}

func get(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getInt(key string, def int) int {
	s := get(key, "")
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func getInt64(key string, def int64) int64 {
	s := get(key, "")
	if s == "" {
		return def
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return v
}

func getBool(key string, def bool) bool {
	s := strings.ToLower(get(key, ""))
	switch s {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}
