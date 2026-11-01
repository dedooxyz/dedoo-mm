package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sync"
	"time"
)

// PrevOut is the spend-view of one transaction output: the value, the locking
// script hex and the confirmation depth the output currently has.
type PrevOut struct {
	ValueSats     int64  `json:"valueSats"`
	ScriptHex     string `json:"scriptHex"`
	Confirmations int64  `json:"confirmations"`
}

// AddressStats is the O(1) unspent summary of an address, served by Esplora's
// /address endpoint. Used for UI balances/counts instead of paging tens of
// thousands of UTXOs on high-traffic vault addresses.
type AddressStats struct {
	UnspentCount int   `json:"unspentCount"`
	UnspentValue int64 `json:"unspentValueSats"`
}

// ChainBackend defines the unified interface for full-node operations
// (either 100% Electrs full or 100% Core RPC full).
type ChainBackend interface {
	GetTipHeight() (int64, error)
	GetTipHash() (string, error)
	GetUTXOs(address string, minConfirmations int64) ([]UTXO, error)
	GetAddressStats(address string) (*AddressStats, error)
	GetTxPrevOut(txid string, vout uint32) (*PrevOut, error)
	EstimateFeeSatsWithMin(nInputs int, nOutputs int, targetBlocks int, minSatPerVB int64) int64
	Broadcast(rawHex string) (string, error)
}

// BitcoindClient is a minimal bitcoind/WJK JSON-RPC client.
type BitcoindClient struct {
	url        string
	user       string
	pass       string
	httpClient *http.Client
}

func NewBitcoindClient(url, user, pass string, timeout time.Duration) *BitcoindClient {
	return &BitcoindClient{
		url:        url,
		user:       user,
		pass:       pass,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// call issues a JSON-RPC request and decodes the result into out.
func (b *BitcoindClient) call(ctx context.Context, method string, params []interface{}, out interface{}) error {
	payload := map[string]interface{}{
		"jsonrpc": "1.0",
		"id":      "dedoo-mm",
		"method":  method,
		"params":  params,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(b.user, b.pass)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("rpc %s: %w", method, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rpc %s: HTTP %d: %s", method, resp.StatusCode, truncate(string(raw), 400))
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &rpc); err != nil {
		return fmt.Errorf("rpc %s: decode: %w", method, err)
	}
	if rpc.Error != nil {
		return fmt.Errorf("rpc %s: %s (code %d)", method, rpc.Error.Message, rpc.Error.Code)
	}
	if out == nil || len(rpc.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(rpc.Result, out); err != nil {
		return fmt.Errorf("rpc %s: result decode: %w", method, err)
	}
	return nil
}

// GetTipHeight returns the current best-chain block height.
func (b *BitcoindClient) GetTipHeight(ctx context.Context) (int64, error) {
	raw, err := b.getBestBlockHash(ctx)
	if err != nil {
		return 0, err
	}
	var bh struct {
		Height int64 `json:"height"`
	}
	if err := b.call(ctx, "getblockheader", []interface{}{raw, true}, &bh); err != nil {
		return 0, err
	}
	return bh.Height, nil
}

func (b *BitcoindClient) getBestBlockHash(ctx context.Context) (string, error) {
	var hash string
	if err := b.call(ctx, "getbestblockhash", nil, &hash); err != nil {
		return "", err
	}
	return hash, nil
}

// GetBlockHash returns hash at the given height.
func (b *BitcoindClient) GetBlockHash(ctx context.Context, height int64) (string, error) {
	var hash string
	if err := b.call(ctx, "getblockhash", []interface{}{height}, &hash); err != nil {
		return "", err
	}
	return hash, nil
}

// GetBlockVerbose returns the verbose block for the given hash.
func (b *BitcoindClient) GetBlockVerbose(ctx context.Context, hash string) (map[string]interface{}, error) {
	var blk map[string]interface{}
	if err := b.call(ctx, "getblock", []interface{}{hash, 2}, &blk); err != nil {
		return nil, err
	}
	return blk, nil
}

// GetRawTransactionHex fetches raw tx hex (0 = non-verbose).
func (b *BitcoindClient) GetRawTransactionHex(ctx context.Context, txid string) (string, error) {
	var hexStr string
	if err := b.call(ctx, "getrawtransaction", []interface{}{txid, 0}, &hexStr); err != nil {
		return "", err
	}
	return hexStr, nil
}

// SendRawTransaction broadcasts a hex tx.
func (b *BitcoindClient) SendRawTransaction(ctx context.Context, rawHex string) (string, error) {
	var txid string
	if err := b.call(ctx, "sendrawtransaction", []interface{}{rawHex}, &txid); err != nil {
		return "", err
	}
	return txid, nil
}

// EstimateSmartFee returns feerate in sats-per-vbyte for the target confirm wait.
func (b *BitcoindClient) EstimateSmartFee(ctx context.Context, confTarget int) (float64, error) {
	var res map[string]interface{}
	if err := b.call(ctx, "estimatesmartfee", []interface{}{confTarget}, &res); err != nil {
		return -1, err
	}
	if raw, ok := res["feerate"].(float64); ok {
		// feerate is BTC/kvB -> sat/vB
		return raw * 1e5, nil
	}
	return -1, fmt.Errorf("estimatesmartfee: no feerate in response")
}

// GetUTXOs performs a scantxoutset query for the given address.
func (b *BitcoindClient) GetUTXOs(ctx context.Context, address string, minConfirmations int64) ([]UTXO, error) {
	var res struct {
		Success  bool  `json:"success"`
		Height   int64 `json:"height"`
		Unspents []struct {
			Txid   string  `json:"txid"`
			Vout   uint32  `json:"vout"`
			Amount float64 `json:"amount"`
			Height int64   `json:"height"`
		} `json:"unspents"`
	}

	if err := b.call(ctx, "scantxoutset", []interface{}{"start", []string{fmt.Sprintf("addr(%s)", address)}}, &res); err != nil {
		return nil, err
	}

	var out []UTXO
	for _, u := range res.Unspents {
		conf := res.Height - u.Height + 1
		if conf >= minConfirmations {
			out = append(out, UTXO{
				Txid:          u.Txid,
				Vout:          u.Vout,
				Value:         int64(math.Round(u.Amount * 1e8)),
				Confirmations: conf,
			})
		}
	}
	return out, nil
}

// GetAddressStats summarizes unspent outputs for an address via scantxoutset.
// Core has no O(1) address summary, so this reuses the UTXO scan; it is only
// called for UI status, never for verification.
func (b *BitcoindClient) getAddressStats(ctx context.Context, address string) (*AddressStats, error) {
	utxos, err := b.GetUTXOs(ctx, address, 1)
	if err != nil {
		return nil, err
	}
	st := &AddressStats{UnspentCount: len(utxos)}
	for _, u := range utxos {
		st.UnspentValue += u.Value
	}
	return st, nil
}

// GetTxPrevOut returns the spend-view of txid:vout via the `gettxout` RPC.
// Unspent-only: an already-spent output returns an error, which is fine for
// proposal verification (a proposal spending a spent input must be rejected).
func (b *BitcoindClient) GetTxPrevOut(ctx context.Context, txid string, vout uint32) (*PrevOut, error) {
	var res struct {
		Value         float64 `json:"value"`
		Confirmations int64   `json:"confirmations"`
		ScriptPubKey  struct {
			Hex string `json:"hex"`
		} `json:"scriptPubKey"`
	}
	if err := b.call(ctx, "gettxout", []interface{}{txid, vout}, &res); err != nil {
		return nil, err
	}
	if res.ScriptPubKey.Hex == "" {
		return nil, fmt.Errorf("gettxout %s:%d: empty script", txid, vout)
	}
	return &PrevOut{
		ValueSats:     int64(math.Round(res.Value * 1e8)),
		ScriptHex:     res.ScriptPubKey.Hex,
		Confirmations: res.Confirmations,
	}, nil
}

// GetRawMempool lists current mempool txids.
func (b *BitcoindClient) GetRawMempool(ctx context.Context) ([]string, error) {
	var txs []string
	if err := b.call(ctx, "getrawmempool", nil, &txs); err != nil {
		return nil, err
	}
	return txs, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// FailoverBitcoindClient provides multi-node RPC redundancy with automatic failover.
type FailoverBitcoindClient struct {
	clients []*BitcoindClient
	timeout time.Duration
	mu      sync.Mutex
}

func NewFailoverBitcoindClient(urls []string, user, pass string, timeout time.Duration) (*FailoverBitcoindClient, error) {
	if len(urls) == 0 {
		return nil, fmt.Errorf("FailoverBitcoindClient requires at least one URL")
	}
	clients := make([]*BitcoindClient, len(urls))
	for i, u := range urls {
		clients[i] = NewBitcoindClient(u, user, pass, timeout)
	}
	return &FailoverBitcoindClient{
		clients: clients,
		timeout: timeout,
	}, nil
}

func (fc *FailoverBitcoindClient) call(fn func(*BitcoindClient) (interface{}, error)) (interface{}, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	var lastErr error
	for i, client := range fc.clients {
		res, err := fn(client)
		if err == nil {
			if i != 0 {
				fc.clients[0], fc.clients[i] = fc.clients[i], fc.clients[0]
			}
			return res, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (fc *FailoverBitcoindClient) GetTipHeight() (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fc.timeout)
	defer cancel()
	res, err := fc.call(func(c *BitcoindClient) (interface{}, error) {
		return c.GetTipHeight(ctx)
	})
	if err != nil {
		return 0, err
	}
	return res.(int64), nil
}

func (fc *FailoverBitcoindClient) GetTipHash() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fc.timeout)
	defer cancel()
	res, err := fc.call(func(c *BitcoindClient) (interface{}, error) {
		return c.getBestBlockHash(ctx)
	})
	if err != nil {
		return "", err
	}
	return res.(string), nil
}

func (fc *FailoverBitcoindClient) GetUTXOs(address string, minConfirmations int64) ([]UTXO, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := fc.call(func(c *BitcoindClient) (interface{}, error) {
		return c.GetUTXOs(ctx, address, minConfirmations)
	})
	if err != nil {
		return nil, err
	}
	return res.([]UTXO), nil
}

func (fc *FailoverBitcoindClient) GetAddressStats(address string) (*AddressStats, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := fc.call(func(c *BitcoindClient) (interface{}, error) {
		return c.getAddressStats(ctx, address)
	})
	if err != nil {
		return nil, err
	}
	return res.(*AddressStats), nil
}

func (fc *FailoverBitcoindClient) GetTxPrevOut(txid string, vout uint32) (*PrevOut, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fc.timeout)
	defer cancel()
	res, err := fc.call(func(c *BitcoindClient) (interface{}, error) {
		return c.GetTxPrevOut(ctx, txid, vout)
	})
	if err != nil {
		return nil, err
	}
	return res.(*PrevOut), nil
}

func (fc *FailoverBitcoindClient) EstimateFeeSatsWithMin(nInputs int, nOutputs int, targetBlocks int, minSatPerVB int64) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), fc.timeout)
	defer cancel()
	rateSatPerVB := float64(minSatPerVB)
	res, err := fc.call(func(c *BitcoindClient) (interface{}, error) {
		return c.EstimateSmartFee(ctx, targetBlocks)
	})
	if err == nil {
		if rate, ok := res.(float64); ok && rate > 0 {
			rateSatPerVB = rate
		}
	}
	if rateSatPerVB < float64(minSatPerVB) {
		rateSatPerVB = float64(minSatPerVB)
	}
	// P2SH 2-of-3 ~ 220 bytes per input, 34 bytes per output + 10 overhead
	vbytes := int64(nInputs*220 + nOutputs*34 + 10)
	fee := int64(math.Ceil(float64(vbytes) * rateSatPerVB))
	if fee < 1000 {
		return 1000
	}
	return fee
}

func (fc *FailoverBitcoindClient) Broadcast(rawHex string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fc.timeout)
	defer cancel()
	res, err := fc.call(func(c *BitcoindClient) (interface{}, error) {
		return c.SendRawTransaction(ctx, rawHex)
	})
	if err != nil {
		return "", err
	}
	return res.(string), nil
}

// DecodeRawTransactionHEX is a lower-case alias used by tests; exposes
// wire-level decode without depending on the tx_builder parse helpers that
// require the temp passphrase. Kept for parity with the relayer.
func DecodeRawTransactionHex(rawHex string) (json.RawMessage, error) {
	txBytes, err := hex.DecodeString(rawHex)
	if err != nil {
		return nil, err
	}
	// Minimal structural sanity: must have version + input count header.
	if len(txBytes) < 10 {
		return nil, fmt.Errorf("raw tx too short")
	}
	return json.Marshal(map[string]interface{}{
		"byteLength": len(txBytes),
		"hex":        hex.EncodeToString(txBytes[:64]),
	})
}

var _ = log.Printf
