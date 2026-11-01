package config

import (
	"strings"
	"testing"

	"github.com/dedooxyz/dedoo-mm/internal/chain"
)

func TestLoadBootstrapDefaults(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.IsBootstrap {
		t.Error("expected bootstrap mode")
	}
	if c.VaultAddress != "" {
		t.Errorf("vault must be env-supplied, got default %q", c.VaultAddress)
	}
}

func TestChainProfileWojakcoinDefault(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ChainName != "wojakcoin" || c.NetParams.Name != "wojakcoin" {
		t.Fatalf("chain = %q/%q, want wojakcoin", c.ChainName, c.NetParams.Name)
	}
	if c.VaultAddress != "" {
		t.Fatalf("vault must not have a built-in default, got %q", c.VaultAddress)
	}
	if c.CoinbaseMaturity != 20 {
		t.Fatalf("maturity = %d, want 20", c.CoinbaseMaturity)
	}
	if c.MaxTxInputs != 100 {
		t.Fatalf("max inputs = %d, want 100", c.MaxTxInputs)
	}
	if chain.AddrPubkeyVersion != 73 || chain.AddrScriptVersion != 5 || chain.WifVersion != 201 {
		t.Fatalf("globals = %d/%d/%d, want 73/5/201", chain.AddrPubkeyVersion, chain.AddrScriptVersion, chain.WifVersion)
	}
}

func TestChainAliasMainnet(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	t.Setenv("CHAIN", "mainnet")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ChainName != "wojakcoin" {
		t.Fatalf("alias mainnet resolved to %q", c.ChainName)
	}
}

func TestDefaultElectrsURLPerChain(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ElectrsURL != "https://wojak-api.s3na.xyz" {
		t.Fatalf("WJK electrs default = %q", c.ElectrsURL)
	}

	t.Setenv("CHAIN", "junkcoin")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load JKC: %v", err)
	}
	if c.ElectrsURL != "https://junk-api.s3na.xyz" {
		t.Fatalf("JKC electrs default = %q", c.ElectrsURL)
	}

	// Explicit override wins.
	t.Setenv("ELECTRS_URL", "https://my-esplora.example/api")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load override: %v", err)
	}
	if c.ElectrsURL != "https://my-esplora.example/api" {
		t.Fatalf("override ignored: %q", c.ElectrsURL)
	}
}

func TestChainProfileJunkcoin(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	t.Setenv("CHAIN", "junkcoin")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ChainName != "junkcoin" {
		t.Fatalf("chain = %q", c.ChainName)
	}
	if c.VaultAddress != "" {
		t.Fatalf("JKC must have no default vault, got %q", c.VaultAddress)
	}
	if c.CoinbaseMaturity != 70 {
		t.Fatalf("JKC maturity = %d, want 70", c.CoinbaseMaturity)
	}
	if chain.AddrPubkeyVersion != 16 || chain.AddrScriptVersion != 5 || chain.WifVersion != 144 {
		t.Fatalf("globals = %d/%d/%d, want 16/5/144", chain.AddrPubkeyVersion, chain.AddrScriptVersion, chain.WifVersion)
	}
}

func TestJunkcoinManagerMode(t *testing.T) {
	t.Setenv("KEEPER_MODE", "keeper")
	t.Setenv("CHAIN", "junkcoin")
	t.Setenv("KEEPER_WIF", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("manager mode must allow no vault/WIF: %v", err)
	}
	if c.VaultAddress != "" || c.KeeperWif != "" {
		t.Fatalf("manager mode must stay empty: %+v", c)
	}
}

func TestJunkcoinExplicitVaultAccepted(t *testing.T) {
	t.Setenv("KEEPER_MODE", "keeper")
	t.Setenv("CHAIN", "junkcoin")
	t.Setenv("KEEPER_WIF", "dummy-wif")
	// A P2SH (version 5) address is valid on both chains.
	t.Setenv("VAULT_ADDRESS", "3CHkEXVtx484ymadFo29rhkm7e3qtpNi2T")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.VaultAddress != "3CHkEXVtx484ymadFo29rhkm7e3qtpNi2T" {
		t.Fatalf("vault = %q", c.VaultAddress)
	}
}

func TestJunkcoinRejectsWojakP2pkhVault(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	t.Setenv("CHAIN", "junkcoin")
	// Version-73 address (WojakCoin P2PKH) must not be accepted as a vault.
	t.Setenv("VAULT_ADDRESS", "WQEGp13ekBXRHjioBvQRNAMYrXRf5dJAMU")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "P2SH version 5") {
		t.Fatalf("expected P2SH-version error, got %v", err)
	}
}

func TestJunkcoinMaturityFloor(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	t.Setenv("CHAIN", "junkcoin")
	t.Setenv("COINBASE_MATURITY", "20")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CoinbaseMaturity != 70 {
		t.Fatalf("maturity = %d, want clamp to 70", c.CoinbaseMaturity)
	}
}

func TestUnknownChainRejected(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	t.Setenv("CHAIN", "dogecoin")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "unknown chain") {
		t.Fatalf("expected unknown-chain error, got %v", err)
	}
}

func TestCustomChainFromEnv(t *testing.T) {
	// A chain with zero compiled-in parameters: everything comes from .env.
	t.Setenv("KEEPER_MODE", "bootstrap")
	t.Setenv("CHAIN", "custom")
	t.Setenv("CHAIN_NAME", "myfork")
	t.Setenv("ADDR_PUBKEY_VERSION", "110")
	t.Setenv("ADDR_SCRIPT_VERSION", "111")
	t.Setenv("WIF_VERSION", "112")
	t.Setenv("COINBASE_MATURITY", "30")
	t.Setenv("ELECTRS_URL", "https://esplora.myfork.example")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ChainName != "myfork" || c.NetParams.Name != "myfork" {
		t.Fatalf("chain name = %q/%q", c.ChainName, c.NetParams.Name)
	}
	if c.NetParams.AddrPubkeyVersion != 110 || c.NetParams.AddrScriptVersion != 111 ||
		c.NetParams.WifVersion != 112 || c.CoinbaseMaturity != 30 {
		t.Fatalf("custom params = %+v maturity=%d", c.NetParams, c.CoinbaseMaturity)
	}
	if c.ElectrsURL != "https://esplora.myfork.example" {
		t.Fatalf("electrs url = %q", c.ElectrsURL)
	}
	if chain.AddrPubkeyVersion != 110 || chain.AddrScriptVersion != 111 || chain.WifVersion != 112 {
		t.Fatalf("globals = %d/%d/%d", chain.AddrPubkeyVersion, chain.AddrScriptVersion, chain.WifVersion)
	}
}

func TestCustomChainRequiresEncoding(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	t.Setenv("CHAIN", "custom")
	t.Setenv("COINBASE_MATURITY", "30")
	t.Setenv("ELECTRS_URL", "https://esplora.example")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "ADDR_PUBKEY_VERSION is required") {
		t.Fatalf("expected required-encoding error, got %v", err)
	}
}

func TestCustomChainRequiresElectrsURL(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	t.Setenv("CHAIN", "custom")
	t.Setenv("ADDR_PUBKEY_VERSION", "110")
	t.Setenv("ADDR_SCRIPT_VERSION", "111")
	t.Setenv("WIF_VERSION", "112")
	t.Setenv("COINBASE_MATURITY", "30")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "ELECTRS_URL is required") {
		t.Fatalf("expected electrs-url error, got %v", err)
	}
}

func TestEncodingOverrideForCustomFork(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	t.Setenv("CHAIN", "junkcoin")
	t.Setenv("ADDR_PUBKEY_VERSION", "110")
	t.Setenv("ADDR_SCRIPT_VERSION", "111")
	t.Setenv("WIF_VERSION", "112")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.NetParams.AddrPubkeyVersion != 110 || c.NetParams.AddrScriptVersion != 111 || c.NetParams.WifVersion != 112 {
		t.Fatalf("overrides not applied: %+v", c.NetParams)
	}
	if chain.AddrPubkeyVersion != 110 || chain.AddrScriptVersion != 111 || chain.WifVersion != 112 {
		t.Fatalf("globals not updated: %d/%d/%d", chain.AddrPubkeyVersion, chain.AddrScriptVersion, chain.WifVersion)
	}
}

func TestLegacyEnvAliases(t *testing.T) {
	t.Setenv("KEEPER_MODE", "keeper")
	t.Setenv("KEEPER_WIF", "dummy-wif")
	// Old deployments used LP_VAULT_ADDRESS / SWEEP_MAX_UTXOS / BP_KEY_ORDER.
	t.Setenv("LP_VAULT_ADDRESS", "3CHkEXVtx484ymadFo29rhkm7e3qtpNi2T")
	t.Setenv("SWEEP_MAX_UTXOS", "42")
	t.Setenv("BP_KEY_ORDER", "2,1,0")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.VaultAddress != "3CHkEXVtx484ymadFo29rhkm7e3qtpNi2T" {
		t.Fatalf("LP_VAULT_ADDRESS alias ignored: %q", c.VaultAddress)
	}
	if c.MaxTxInputs != 42 {
		t.Fatalf("SWEEP_MAX_UTXOS alias ignored: %d", c.MaxTxInputs)
	}
	if len(c.KeyOrder) != 3 || c.KeyOrder[0] != 2 {
		t.Fatalf("BP_KEY_ORDER alias ignored: %v", c.KeyOrder)
	}
}

func TestConsensusChecksum(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	t.Setenv("VAULT_ADDRESS", "3CHkEXVtx484ymadFo29rhkm7e3qtpNi2T")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	base := c.ConsensusChecksum("52ae")
	if base == c.ConsensusChecksum("53ae") {
		t.Fatal("checksum must change with the redeem script")
	}
	t.Setenv("VAULT_ADDRESS", "35SuVeH65D983sqpoJqEK2TuEvfo2aaDtQ")
	c2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if base == c2.ConsensusChecksum("52ae") {
		t.Fatal("checksum must change with the vault address")
	}
}

func TestManagerModeAllowsNoWif(t *testing.T) {
	t.Setenv("KEEPER_MODE", "keeper")
	c, err := Load()
	if err != nil {
		t.Fatalf("manager mode must start without KEEPER_WIF: %v", err)
	}
	if c.KeeperWif != "" || c.VaultAddress != "" {
		t.Fatalf("expected empty wallet config, got WIF=%q vault=%q", c.KeeperWif, c.VaultAddress)
	}
}

func TestApiAuthConfigValidation(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	t.Setenv("API_AUTH_USER", "holder")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("expected paired-auth error, got %v", err)
	}
	t.Setenv("API_AUTH_PASS", "short")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "at least 8") {
		t.Fatalf("expected password-length error, got %v", err)
	}
	t.Setenv("API_AUTH_PASS", "long-enough-pass")
	if _, err := Load(); err != nil {
		t.Fatalf("valid auth config rejected: %v", err)
	}
}

func TestMaxTxInputsValidation(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	t.Setenv("MAX_TX_INPUTS", "601")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MAX_TX_INPUTS") {
		t.Fatalf("expected MAX_TX_INPUTS error, got %v", err)
	}
	t.Setenv("MAX_TX_INPUTS", "600")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxTxInputs != 600 {
		t.Fatalf("max inputs = %d, want 600", c.MaxTxInputs)
	}
}

func TestRequiredSigsValidation(t *testing.T) {
	t.Setenv("KEEPER_MODE", "bootstrap")
	t.Setenv("REQUIRED_SIGS", "4")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REQUIRED_SIGS") {
		t.Fatalf("expected REQUIRED_SIGS error, got %v", err)
	}
	t.Setenv("REQUIRED_SIGS", "3")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.RequiredSigs != 3 {
		t.Fatalf("required sigs = %d, want 3", c.RequiredSigs)
	}
}
