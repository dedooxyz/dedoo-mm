package chain

import (
	"fmt"
	"strings"
)

// NetParams captures the base58/WIF encoding and maturity parameters that
// differ between the chains this keeper supports. The on-chain P2SH/P2PKH
// locking scripts are identical; only the textual address encoding, the
// private-key encoding and the consensus maturity window differ.
type NetParams struct {
	// Name is the canonical chain identifier (also the CHAIN config value).
	Name string
	// Aliases are additional accepted CHAIN spellings (case-insensitive).
	Aliases []string

	// AddrPubkeyVersion is the base58 version of P2PKH addresses.
	AddrPubkeyVersion byte
	// AddrScriptVersion is the base58 version of P2SH addresses (vaults).
	AddrScriptVersion byte
	// WifVersion is the base58 version of WIF private keys.
	WifVersion byte

	// CoinbaseMaturity is the consensus coinbase-output maturity (blocks).
	// The keeper never spends vault outputs below this depth.
	CoinbaseMaturity int64
	// DefaultP2PPort is the coin daemon's network port (informational).
	DefaultP2PPort int
	// DefaultElectrsURL is the chain's public Esplora/electrs endpoint used
	// when ELECTRS_URL is not set.
	DefaultElectrsURL string
}

var (
	// WojakCoinParams is the WojakCoin mainnet profile (WJK), verified against
	// WojakCoinProj/wojakcore src/chainparams.cpp and a live coinbase:
	// miner 45 WJK / LP 4 WJK (3BNf3...) / dev 1 WJK (38gyw...).
	WojakCoinParams = NetParams{
		Name:              "wojakcoin",
		Aliases:           []string{"wojak", "wjk", "mainnet"},
		AddrPubkeyVersion: 73,
		AddrScriptVersion: 5,
		WifVersion:        201,
		CoinbaseMaturity:  20, // consensus/consensus.h: COINBASE_MATURITY = 20
		DefaultP2PPort:    20759,
		DefaultElectrsURL: "https://wojak-api.s3na.xyz",
	}

	// JunkcoinParams is the Junkcoin mainnet profile (JKC), per
	// junkcoin chainparams.cpp: PUBKEY_ADDRESS=16, SCRIPT_ADDRESS=5,
	// SECRET_KEY=144, nCoinbaseMaturity=70, nDefaultPort=9771. JKC has no
	// keeper vaults, so the operator must supply both addresses explicitly.
	JunkcoinParams = NetParams{
		Name:              "junkcoin",
		Aliases:           []string{"junk", "jkc"},
		AddrPubkeyVersion: 16,
		AddrScriptVersion: 5,
		WifVersion:        144,
		CoinbaseMaturity:  70,
		DefaultP2PPort:    9771,
		DefaultElectrsURL: "https://junk-api.s3na.xyz",
	}
)

// SupportedChains lists the canonical chain names accepted by CHAIN.
func SupportedChains() []string {
	return []string{WojakCoinParams.Name, JunkcoinParams.Name}
}

// LookupNetParams resolves a CHAIN value (canonical name or alias,
// case-insensitive) to its network parameters.
func LookupNetParams(name string) (NetParams, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, p := range []NetParams{WojakCoinParams, JunkcoinParams} {
		if n == p.Name {
			return p, nil
		}
		for _, a := range p.Aliases {
			if n == a {
				return p, nil
			}
		}
	}
	return NetParams{}, fmt.Errorf("unknown chain %q (supported: %s)", name, strings.Join(SupportedChains(), ", "))
}

// Address encoding globals, consumed by DecodeAddress/EncodeAddress, WIF
// loading and the transfer destination-version guards. They default to
// WojakCoin mainnet and are switched at startup via SelectNetParams.
var (
	AddrPubkeyVersion = WojakCoinParams.AddrPubkeyVersion
	AddrScriptVersion = WojakCoinParams.AddrScriptVersion
	WifVersion        = WojakCoinParams.WifVersion
)

// SelectNetParams applies p to the package's address/WIF encoding globals.
// Call once during startup (config.Load does this), before keeper traffic.
func SelectNetParams(p NetParams) {
	AddrPubkeyVersion = p.AddrPubkeyVersion
	AddrScriptVersion = p.AddrScriptVersion
	WifVersion = p.WifVersion
}

// CurrentNetParams returns the network parameters currently in effect
// (matched by WIF version; used for diagnostics and UI presentation).
func CurrentNetParams() NetParams {
	for _, p := range []NetParams{WojakCoinParams, JunkcoinParams} {
		if p.WifVersion == WifVersion && p.AddrPubkeyVersion == AddrPubkeyVersion {
			return p
		}
	}
	return NetParams{
		Name:              "custom",
		AddrPubkeyVersion: AddrPubkeyVersion,
		AddrScriptVersion: AddrScriptVersion,
		WifVersion:        WifVersion,
		CoinbaseMaturity:  20,
	}
}

// ChainInfo is the UI-facing description of a supported chain profile.
type ChainInfo struct {
	Name              string   `json:"name"`
	Aliases           []string `json:"aliases"`
	AddrPubkeyVersion int      `json:"addrPubkeyVersion"`
	AddrScriptVersion int      `json:"addrScriptVersion"`
	WifVersion        int      `json:"wifVersion"`
	CoinbaseMaturity  int64    `json:"coinbaseMaturity"`
	DefaultElectrsURL string   `json:"defaultElectrsUrl"`
}

// ChainInfos lists the built-in chain profiles for the wallet-creation form.
func ChainInfos() []ChainInfo {
	out := make([]ChainInfo, 0, 2)
	for _, p := range []NetParams{WojakCoinParams, JunkcoinParams} {
		out = append(out, ChainInfo{
			Name:              p.Name,
			Aliases:           p.Aliases,
			AddrPubkeyVersion: int(p.AddrPubkeyVersion),
			AddrScriptVersion: int(p.AddrScriptVersion),
			WifVersion:        int(p.WifVersion),
			CoinbaseMaturity:  p.CoinbaseMaturity,
			DefaultElectrsURL: p.DefaultElectrsURL,
		})
	}
	return out
}
