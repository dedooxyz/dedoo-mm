// Command dedoo-mm runs one node of a 2-of-3 multisig keeper trio.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/dedooxyz/dedoo-mm/internal/api"
	"github.com/dedooxyz/dedoo-mm/internal/chain"
	"github.com/dedooxyz/dedoo-mm/internal/config"
	"github.com/dedooxyz/dedoo-mm/internal/p2p"
	"github.com/dedooxyz/dedoo-mm/internal/processor"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := run(); err != nil {
		log.Fatalf("[main] %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Derive or resolve the vault redeem script before P2P setup.
	redeemHex := cfg.RedeemScriptHex
	if len(cfg.PubKeys) == 3 {
		redeemHex, err = chain.BuildMultisigRedeemScript(cfg.PubKeys, cfg.KeyOrder, cfg.RequiredSigs)
		if err != nil {
			return fmt.Errorf("derive redeem script: %w", err)
		}
		log.Printf("[main] derived vault redeem: %s", redeemHex)
	}
	if !cfg.IsBootstrap && cfg.VaultAddress != "" && redeemHex == "" {
		return fmt.Errorf("set REDEEM_SCRIPT_HEX (or CEREMONY_PUBKEYS)")
	}
	if !cfg.IsBootstrap && cfg.VaultAddress == "" {
		log.Printf("[main] no env vault configured; starting as wallet manager (add wallets/keys in the UI)")
	}
	// The vault address and redeem script are independent .env values; prove
	// they describe the same P2SH wallet before signing anything.
	if !cfg.IsBootstrap && cfg.VaultAddress != "" {
		derived, err := chain.P2SHAddressFromRedeemScript(redeemHex)
		if err != nil {
			return fmt.Errorf("redeem script: %w", err)
		}
		if derived != cfg.VaultAddress {
			return fmt.Errorf("VAULT_ADDRESS %s does not match the redeem script (derived P2SH %s)", cfg.VaultAddress, derived)
		}
		threshold, err := chain.ParseMultisigThreshold(redeemHex)
		if err != nil {
			return fmt.Errorf("redeem script: %w", err)
		}
		if threshold != cfg.RequiredSigs {
			return fmt.Errorf("REQUIRED_SIGS=%d does not match the redeem script threshold %d", cfg.RequiredSigs, threshold)
		}
		log.Printf("[main] vault address matches redeem script: %s (%d-of-3)", derived, threshold)
	}

	// Consensus checksum across chain profile, vault address and redeem script.
	consensusChecksum := cfg.P2PChecksum()
	log.Printf("[main] network consensus checksum: %s", consensusChecksum[:16])

	node, err := p2p.NewNode(
		ctx,
		p2p.Options{
			KeyFile:       cfg.KeyFile,
			BindHost:      cfg.BindHost,
			BindPort:      cfg.BindPort,
			IsBootstrap:   cfg.IsBootstrap,
			BootstrapAddr: cfg.BootstrapAddr,
			PeerAddrs:     cfg.PeerAddrs,
			SelfIndex:     cfg.SelfIndex,
			VaultHash:     consensusChecksum,
			UsePublicDHT:  cfg.UsePublicDHT,
		},
	)
	if err != nil {
		return fmt.Errorf("p2p: %w", err)
	}
	defer func() {
		_ = node.Close()
		log.Printf("[main] shutting down")
	}()

	if cfg.IsBootstrap {
		log.Printf("[main] bootstrap relay mode; no signing")
		<-ctx.Done()
		return nil
	}

	k, err := processor.NewKeeper(cfg)
	if err != nil {
		return fmt.Errorf("keeper: %w", err)
	}

	k.SetRedeemScript(redeemHex)
	if err := k.LoadState(); err != nil {
		log.Printf("[main] state load: %v (continuing with empty state)", err)
	}
	k.SetP2PNode(node)

	apiServer := api.NewServer(k)
	if cfg.ApiUser != "" {
		apiServer.SetBasicAuth(cfg.ApiUser, cfg.ApiPass)
		log.Printf("[api] HTTP Basic Auth enabled for user %q", cfg.ApiUser)
	}
	go func() {
		if err := apiServer.ListenAndServe(ctx, cfg.ApiListen); err != nil {
			log.Printf("[api] %v", err)
		}
	}()

	log.Printf("[main] keeper #%d running on %s (api %s, vault %s)",
		cfg.SelfIndex, node.ID(), cfg.ApiListen, cfg.VaultAddress)
	<-ctx.Done()
	return nil
}
