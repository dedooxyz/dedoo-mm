package processor

// PublicConfig is the operator-facing subset of the configuration shown in
// the UI. Secrets (KEEPER_WIF) are never exposed.
type PublicConfig struct {
	ChainName        string `json:"chainName"`
	Backend          string `json:"backend"`
	VaultAddress     string `json:"vaultAddress"`
	RequiredSigs     int    `json:"requiredSigs"`
	MaxTxInputs      int    `json:"maxTxInputs"`
	MinTransferSats  int64  `json:"minTransferSats"`
	CoinbaseMaturity int64  `json:"coinbaseMaturity"`
	ApiListen        string `json:"apiListen"`
}

func (k *Keeper) PublicConfig() PublicConfig {
	return PublicConfig{
		ChainName:        k.cfg.ChainName,
		Backend:          k.cfg.Backend,
		VaultAddress:     k.cfg.VaultAddress,
		RequiredSigs:     k.cfg.RequiredSigs,
		MaxTxInputs:      k.cfg.MaxTxInputs,
		MinTransferSats:  k.cfg.MinTransferSats,
		CoinbaseMaturity: k.cfg.CoinbaseMaturity,
		ApiListen:        k.cfg.ApiListen,
	}
}
