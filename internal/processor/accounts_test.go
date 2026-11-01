package processor

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dedooxyz/dedoo-mm/internal/chain"

	"github.com/btcsuite/btcutil/base58"
)

func TestAccountManagerLifecycle(t *testing.T) {
	keys := newTrioKeys(t)
	other := newTrioKeys(t)

	cfg := baseTestCfg(0, keys.vaultAddr, false, "")
	cfg.AccountsFile = filepath.Join(t.TempDir(), "accounts.json")
	k := newTestKeeper(t, cfg, newMockChain(), keys.privs[0], keys.pubs[0], keys.redeem)

	req := CreateAccountRequest{
		Name:     "Ops Vault",
		Chain:    "wojakcoin",
		Pubkeys:  []string{other.pubs[0], other.pubs[1], other.pubs[2]},
		Order:    []int{0, 1, 2},
		Required: 2,
	}
	acct, created, err := k.CreateAccount(req)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if !created {
		t.Fatal("expected a new account")
	}
	if acct.Address != other.vaultAddr {
		t.Fatalf("address = %s, want %s", acct.Address, other.vaultAddr)
	}
	if acct.ChainKey != "wojakcoin" || acct.Required != 2 {
		t.Fatalf("account metadata: %+v", acct)
	}
	if acct.AddrScriptVersion != 5 || acct.WifVersion != 201 {
		t.Fatalf("versions: %+v", acct)
	}

	// Duplicate address returns the existing account, not a second row.
	dup, created, err := k.CreateAccount(req)
	if err != nil || created || dup.Id != acct.Id {
		t.Fatalf("duplicate: created=%v id=%s err=%v", created, dup.Id, err)
	}

	// Active vault cannot be removed.
	if err := k.DeleteAccount("active"); err == nil {
		t.Fatal("active vault must not be removable")
	}

	list := k.ListAccounts()
	if len(list) != 2 || !list[0].Active || list[1].Id != acct.Id {
		t.Fatalf("list accounts: %+v", list)
	}

	// Persistence across restart.
	k2 := newTestKeeper(t, cfg, newMockChain(), keys.privs[0], keys.pubs[0], keys.redeem)
	list2 := k2.ListAccounts()
	if len(list2) != 2 || list2[1].Address != acct.Address {
		t.Fatalf("saved wallet lost after restart: %+v", list2)
	}

	if err := k2.DeleteAccount(acct.Id); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	k3 := newTestKeeper(t, cfg, newMockChain(), keys.privs[0], keys.pubs[0], keys.redeem)
	if got := k3.ListAccounts(); len(got) != 1 {
		t.Fatalf("deleted wallet still present: %+v", got)
	}
}

func TestAccountManagerRejectsOtherChain(t *testing.T) {
	keys := newTrioKeys(t)
	other := newTrioKeys(t)
	cfg := baseTestCfg(0, keys.vaultAddr, false, "")
	cfg.AccountsFile = filepath.Join(t.TempDir(), "accounts.json")
	k := newTestKeeper(t, cfg, newMockChain(), keys.privs[0], keys.pubs[0], keys.redeem)

	pub, script, wif := 111, 5, 239
	_, _, err := k.CreateAccount(CreateAccountRequest{
		Name:              "JKC testnet vault",
		Chain:             "custom",
		ChainName:         "junkcoin-testnet",
		AddrPubkeyVersion: &pub,
		AddrScriptVersion: &script,
		WifVersion:        &wif,
		Pubkeys:           []string{other.pubs[0], other.pubs[1], other.pubs[2]},
		Required:          2,
	})
	if err == nil || !strings.Contains(err.Error(), "restricted to CHAIN=wojakcoin") {
		t.Fatalf("expected one-chain restriction, got %v", err)
	}

	// A custom profile that matches the node chain is allowed.
	cfg2 := baseTestCfg(0, keys.vaultAddr, false, "")
	cfg2.ChainName = "junkcoin-testnet"
	cfg2.NetParams = chain.NetParams{
		Name: "junkcoin-testnet", AddrPubkeyVersion: 111, AddrScriptVersion: 5,
		WifVersion: 239, CoinbaseMaturity: 30,
	}
	cfg2.AccountsFile = filepath.Join(t.TempDir(), "accounts.json")
	k2 := newTestKeeper(t, cfg2, newMockChain(), keys.privs[0], keys.pubs[0], keys.redeem)
	acct, _, err := k2.CreateAccount(CreateAccountRequest{
		Name: "JKC Ops", Chain: "custom", ChainName: "junkcoin-testnet",
		AddrPubkeyVersion: &pub, AddrScriptVersion: &script, WifVersion: &wif,
		Pubkeys: []string{other.pubs[0], other.pubs[1], other.pubs[2]}, Required: 2,
	})
	if err != nil {
		t.Fatalf("matching custom chain rejected: %v", err)
	}
	if acct.Chain != "junkcoin-testnet" || acct.ChainKey != "custom" {
		t.Fatalf("custom chain metadata: %+v", acct)
	}
}

func TestSigningKeySharedAcrossWallets(t *testing.T) {
	keys := newTrioKeys(t)
	other := newTrioKeys(t)
	cfg := baseTestCfg(0, keys.vaultAddr, false, "")
	cfg.AccountsFile = filepath.Join(t.TempDir(), "accounts.json")
	cfg.KeysFile = filepath.Join(t.TempDir(), "keys.json")
	k := newTestKeeper(t, cfg, newMockChain(), keys.privs[0], keys.pubs[0], keys.redeem)

	// Two wallets built from the same 3 keys in different order.
	w1, _, err := k.CreateAccount(CreateAccountRequest{
		Name: "Order 012", Chain: "wojakcoin",
		Pubkeys: []string{other.pubs[0], other.pubs[1], other.pubs[2]}, Order: []int{0, 1, 2}, Required: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	w2, _, err := k.CreateAccount(CreateAccountRequest{
		Name: "Order 210", Chain: "wojakcoin",
		Pubkeys: []string{other.pubs[0], other.pubs[1], other.pubs[2]}, Order: []int{2, 1, 0}, Required: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w1.HasKey || w2.HasKey {
		t.Fatal("wallets must start without a key")
	}

	wif := base58.CheckEncode(append(other.privs[0].Serialize(), 0x01), chain.WojakCoinParams.WifVersion)
	if _, created, err := k.AddSigningKey(wif); err != nil || !created {
		t.Fatalf("AddSigningKey: created=%v err=%v", created, err)
	}
	for _, id := range []string{w1.Address, w2.Address} {
		a, err := k.GetAccount(id)
		if err != nil {
			t.Fatal(err)
		}
		if !a.HasKey {
			t.Fatalf("wallet %s should be signable by the shared key", a.Name)
		}
	}
	// The env key is protected; the imported key can be removed.
	envKeys := k.ListSigningKeys()
	var imported string
	for _, key := range envKeys {
		if !key.Env {
			imported = key.Pubkey
		}
	}
	if imported == "" {
		t.Fatal("imported key missing")
	}
	if err := k.RemoveSigningKey(k.pubHex); err == nil {
		t.Fatal("env key must not be removable")
	}
	if err := k.RemoveSigningKey(imported); err != nil {
		t.Fatalf("remove imported key: %v", err)
	}
	if a, _ := k.GetAccount(w1.Address); a.HasKey {
		t.Fatal("wallet should become watch-only after key removal")
	}
}

func TestAccountManagerValidation(t *testing.T) {
	keys := newTrioKeys(t)
	cfg := baseTestCfg(0, keys.vaultAddr, false, "")
	cfg.AccountsFile = filepath.Join(t.TempDir(), "accounts.json")
	k := newTestKeeper(t, cfg, newMockChain(), keys.privs[0], keys.pubs[0], keys.redeem)

	if _, _, err := k.CreateAccount(CreateAccountRequest{
		Name: "bad", Chain: "wojakcoin",
		Pubkeys: []string{keys.pubs[0], keys.pubs[1]},
	}); err == nil {
		t.Fatal("expected error for 2 pubkeys")
	}

	other := newTrioKeys(t)
	// Redeem script threshold must match Required.
	if _, _, err := k.CreateAccount(CreateAccountRequest{
		Name: "mismatch", Chain: "wojakcoin",
		RedeemScript: other.redeem, Required: 3,
	}); err == nil {
		t.Fatal("expected threshold mismatch error")
	}

	// Pubkeys and redeem script must agree.
	if _, _, err := k.CreateAccount(CreateAccountRequest{
		Name:         "mismatch2",
		Chain:        "wojakcoin",
		Pubkeys:      []string{other.pubs[0], other.pubs[1], other.pubs[2]},
		Order:        []int{2, 1, 0},
		Required:     2,
		RedeemScript: other.redeem,
	}); err == nil {
		t.Fatal("expected pubkey/redeem mismatch error")
	}
}
