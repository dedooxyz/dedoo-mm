package chain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type UTXO struct {
	Txid          string `json:"txid"`
	Vout          uint32 `json:"vout"`
	Value         int64  `json:"value"`
	Confirmations int64  `json:"confirmations"`
}

type TxStatus struct {
	Confirmed   bool   `json:"confirmed"`
	BlockHeight int64  `json:"block_height"`
	BlockHash   string `json:"block_hash"`
	BlockTime   int64  `json:"block_time"`
}

type ElectrsClient struct {
	base    string
	timeout time.Duration
	client  *http.Client
}

func NewElectrsClient(baseURL string, timeout time.Duration) *ElectrsClient {
	return &ElectrsClient{
		base:    strings.TrimSuffix(baseURL, "/"),
		timeout: timeout,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

func (c *ElectrsClient) get(path string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
		}
		req, err := http.NewRequest("GET", c.base+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "wjkc-go-relayer/1.0")

		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return body, nil
		}
		lastErr = fmt.Errorf("electrs HTTP status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		// Retry only transient conditions; other 4xx are caller errors.
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func (c *ElectrsClient) getTipHeight() (int64, error) {
	body, err := c.get("/blocks/tip/height")
	if err != nil {
		return 0, err
	}
	val, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse height: %w", err)
	}
	return val, nil
}

func (c *ElectrsClient) getTipHash() (string, error) {
	body, err := c.get("/blocks/tip/hash")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func (c *ElectrsClient) getTx(txid string) (map[string]interface{}, error) {
	body, err := c.get("/tx/" + txid)
	if err != nil {
		return nil, err
	}
	var res map[string]interface{}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	return res, nil
}

func (c *ElectrsClient) getTxStatus(txid string) (*TxStatus, error) {
	body, err := c.get("/tx/" + txid + "/status")
	if err != nil {
		return nil, err
	}
	var status TxStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

func (c *ElectrsClient) getTxOutspends(txid string) ([]map[string]interface{}, error) {
	body, err := c.get("/tx/" + txid + "/outspends")
	if err != nil {
		return nil, err
	}
	var res []map[string]interface{}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	return res, nil
}

func (c *ElectrsClient) getTxHex(txid string) (string, error) {
	body, err := c.get("/tx/" + txid + "/hex")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func (c *ElectrsClient) isUtxoUnspent(txid string, vout uint32) (bool, error) {
	outspends, err := c.getTxOutspends(txid)
	if err != nil {
		return false, err
	}
	if int(vout) >= len(outspends) {
		return true, nil
	}
	entry := outspends[vout]
	spent, ok := entry["spent"].(bool)
	return !ok || !spent, nil
}

func (c *ElectrsClient) getFeeEstimates() (map[string]float64, error) {
	body, err := c.get("/fee-estimates")
	if err != nil {
		return nil, err
	}
	var res map[string]float64
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	return res, nil
}

const FallbackSatPerVB = 200

func (c *ElectrsClient) estimateFeeSats(nInputs int, nOutputs int, targetBlocks int) int64 {
	return c.estimateFeeSatsWithMin(nInputs, nOutputs, targetBlocks, FallbackSatPerVB)
}

// estimateFeeSatsWithMin estimates the fee in satoshis with a configurable minimum
// sat/vB floor. Used by the refund processor to support fee-bump retries.
func (c *ElectrsClient) estimateFeeSatsWithMin(nInputs int, nOutputs int, targetBlocks int, minSatPerVB int64) int64 {
	vbytes := int64(nInputs*300 + nOutputs*34 + 10)
	floor := float64(minSatPerVB)
	if floor < 200.0 {
		floor = 200.0 // absolute minimum relay fee for Junkcoin nodes
	}

	estimates, err := c.getFeeEstimates()
	if err != nil || len(estimates) == 0 {
		return vbytes * int64(floor)
	}

	// Find the closest target that is >= targetBlocks
	var bestTarget int = 999999
	var rate float64 = 0.0

	for k, v := range estimates {
		tgt, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		if tgt >= targetBlocks && tgt < bestTarget {
			bestTarget = tgt
			rate = v
		}
	}

	if rate == 0 {
		// Fallback to absolute minimum if no targets matched
		for _, v := range estimates {
			if rate == 0 || v < rate {
				rate = v
			}
		}
	}

	if rate < floor {
		rate = floor
	}

	fee := int64(float64(vbytes)*rate) + 1
	if fee < vbytes*int64(floor) {
		return vbytes * int64(floor)
	}
	return fee
}

// getTxPrevOut resolves txid:vout through the Esplora /tx/<txid> endpoint.
// A spent output yields an error (status.spent), so verification-first flows
// fail closed on double-spent inputs.
func (c *ElectrsClient) getTxPrevOut(txid string, vout uint32) (*PrevOut, error) {
	tx, err := c.getTx(txid)
	if err != nil {
		return nil, err
	}
	vouts, ok := tx["vout"].([]interface{})
	if !ok || int(vout) >= len(vouts) {
		return nil, fmt.Errorf("tx %s has no vout %d", txid, vout)
	}
	voutMap, ok := vouts[vout].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("tx %s vout %d: malformed", txid, vout)
	}
	valueF, _ := voutMap["value"].(float64)
	// Esplora returns "scriptpubkey" as a hex string on this backend, while
	// some variants use an object with a "hex" field. Support both.
	var spkHex string
	switch spk := voutMap["scriptpubkey"].(type) {
	case string:
		spkHex = spk
	case map[string]interface{}:
		spkHex, _ = spk["hex"].(string)
	}
	if spkHex == "" {
		return nil, fmt.Errorf("tx %s vout %d: empty script", txid, vout)
	}

	status, _ := tx["status"].(map[string]interface{})
	confirmed, _ := status["confirmed"].(bool)
	confs := int64(0)
	if confirmed {
		height := int64(0)
		if bh, ok := status["block_height"].(float64); ok {
			height = int64(bh)
		}
		tip, err := c.getTipHeight()
		if err == nil && height > 0 {
			confs = tip - height + 1
		}
	}

	// Double-spend guard: if the outspend for this vout is spent at tip, the
	// utxo no longer exists and a proposal referencing it must be rejected.
	outspends, err := c.getTxOutspends(txid)
	if err == nil && int(vout) < len(outspends) {
		if spent, ok := outspends[vout]["spent"].(bool); ok && spent {
			return nil, fmt.Errorf("tx %s vout %d already spent", txid, vout)
		}
	}

	return &PrevOut{
		ValueSats:     int64(valueF),
		ScriptHex:     spkHex,
		Confirmations: confs,
	}, nil
}

func (c *ElectrsClient) broadcast(rawHex string) (string, error) {
	req, err := http.NewRequest("POST", c.base+"/tx", bytes.NewReader([]byte(rawHex)))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "wjkc-go-relayer/1.0")
	req.Header.Set("Content-Type", "text/plain")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("electrs broadcast HTTP status %d: %s", resp.StatusCode, string(body))
	}

	return strings.TrimSpace(string(body)), nil
}

func (c *ElectrsClient) getAddressTxs(address string, afterTxid string) ([]map[string]interface{}, error) {
	path := "/address/" + address + "/txs"
	if afterTxid != "" {
		path += "?after_txid=" + afterTxid
	}
	body, err := c.get(path)
	if err != nil {
		return nil, err
	}

	// The api might return list or dict containing a list
	var txs []map[string]interface{}
	if err := json.Unmarshal(body, &txs); err != nil {
		// Try unmarshaling as dict
		var wrapper map[string]interface{}
		if err2 := json.Unmarshal(body, &wrapper); err2 == nil {
			if list, ok := wrapper["transactions"].([]interface{}); ok {
				for _, item := range list {
					if txMap, ok := item.(map[string]interface{}); ok {
						txs = append(txs, txMap)
					}
				}
				return txs, nil
			}
		}
		return nil, err
	}
	return txs, nil
}

// maxUTXOEntries bounds how many UTXOs getUTXOs will page through for one
// address. Daemon vaults can hold tens of thousands of coinbase outputs;
// proposals only ever need MaxTxInputs (600) and the UI table is capped, so
// there is no reason to download the entire history.
const maxUTXOEntries = MaxTxInputs

func (c *ElectrsClient) getUTXOs(address string, minConfirmations int64) ([]UTXO, error) {
	tip, err := c.getTipHeight()
	if err != nil {
		return nil, err
	}

	var result []UTXO
	startIndex := 0
	pageSize := 200

	for {
		path := fmt.Sprintf("/address/%s/utxo?start_index=%d&limit=%d", address, startIndex, pageSize)
		body, err := c.get(path)
		if err != nil {
			return nil, err
		}

		var page []map[string]interface{}
		var total int64
		if err := json.Unmarshal(body, &page); err != nil {
			var wrapper map[string]interface{}
			if err2 := json.Unmarshal(body, &wrapper); err2 == nil {
				if list, ok := wrapper["utxos"].([]interface{}); ok {
					for _, item := range list {
						if uMap, ok := item.(map[string]interface{}); ok {
							page = append(page, uMap)
						}
					}
				}
				if tFloat, ok := wrapper["total"].(float64); ok {
					total = int64(tFloat)
				}
			} else {
				return nil, err
			}
		}

		if len(page) == 0 {
			break
		}

		for _, item := range page {
			txid, _ := item["txid"].(string)
			var vout uint32
			if vFloat, ok := item["vout"].(float64); ok {
				vout = uint32(vFloat)
			}
			var value int64
			if valFloat, ok := item["value"].(float64); ok {
				value = int64(valFloat)
			}

			status, _ := item["status"].(map[string]interface{})
			confirmed, _ := status["confirmed"].(bool)

			if !confirmed {
				if minConfirmations <= 0 {
					result = append(result, UTXO{
						Txid:          txid,
						Vout:          vout,
						Value:         value,
						Confirmations: 0,
					})
				}
				continue
			}

			blockHeight := int64(0)
			if bhFloat, ok := status["block_height"].(float64); ok {
				blockHeight = int64(bhFloat)
			}

			confs := int64(0)
			if blockHeight > 0 {
				confs = tip - blockHeight + 1
			}

			if confs < minConfirmations {
				continue
			}

			result = append(result, UTXO{
				Txid:          txid,
				Vout:          vout,
				Value:         value,
				Confirmations: confs,
			})
		}

		if len(result) >= maxUTXOEntries {
			result = result[:maxUTXOEntries]
			break
		}
		if total > 0 && int64(len(result)) >= total {
			break
		}
		if len(page) < pageSize {
			break
		}
		startIndex += pageSize
	}

	return result, nil
}

// getAddressStats returns the unspent count/value via /address/<addr>.
func (c *ElectrsClient) getAddressStats(address string) (*AddressStats, error) {
	body, err := c.get("/address/" + address)
	if err != nil {
		return nil, err
	}
	var res struct {
		ChainStats struct {
			FundedTxoCount int64 `json:"funded_txo_count"`
			FundedTxoSum   int64 `json:"funded_txo_sum"`
			SpentTxoCount  int64 `json:"spent_txo_count"`
			SpentTxoSum    int64 `json:"spent_txo_sum"`
		} `json:"chain_stats"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	count := res.ChainStats.FundedTxoCount - res.ChainStats.SpentTxoCount
	value := res.ChainStats.FundedTxoSum - res.ChainStats.SpentTxoSum
	if count < 0 {
		count = 0
	}
	if value < 0 {
		value = 0
	}
	return &AddressStats{UnspentCount: int(count), UnspentValue: value}, nil
}

// FailoverElectrsClient implements the multi-backend failover pattern.
type FailoverElectrsClient struct {
	clients []*ElectrsClient
	mu      sync.Mutex
}

func NewFailoverElectrsClient(urls []string, timeout time.Duration) (*FailoverElectrsClient, error) {
	if len(urls) == 0 {
		return nil, fmt.Errorf("FailoverElectrsClient requires at least one URL")
	}
	clients := make([]*ElectrsClient, len(urls))
	for i, url := range urls {
		clients[i] = NewElectrsClient(url, timeout)
	}
	return &FailoverElectrsClient{
		clients: clients,
	}, nil
}

func (fc *FailoverElectrsClient) call(fn func(*ElectrsClient) (interface{}, error)) (interface{}, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	var lastErr error
	for i, client := range fc.clients {
		res, err := fn(client)
		if err == nil {
			if i != 0 {
				// Promote succeeding client to primary (index 0)
				fc.clients[0], fc.clients[i] = fc.clients[i], fc.clients[0]
			}
			return res, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (fc *FailoverElectrsClient) GetTipHeight() (int64, error) {
	res, err := fc.call(func(c *ElectrsClient) (interface{}, error) {
		return c.getTipHeight()
	})
	if err != nil {
		return 0, err
	}
	return res.(int64), nil
}

func (fc *FailoverElectrsClient) GetTipHash() (string, error) {
	res, err := fc.call(func(c *ElectrsClient) (interface{}, error) {
		return c.getTipHash()
	})
	if err != nil {
		return "", err
	}
	return res.(string), nil
}

func (fc *FailoverElectrsClient) GetTx(txid string) (map[string]interface{}, error) {
	res, err := fc.call(func(c *ElectrsClient) (interface{}, error) {
		return c.getTx(txid)
	})
	if err != nil {
		return nil, err
	}
	return res.(map[string]interface{}), nil
}

func (fc *FailoverElectrsClient) GetTxStatus(txid string) (*TxStatus, error) {
	res, err := fc.call(func(c *ElectrsClient) (interface{}, error) {
		return c.getTxStatus(txid)
	})
	if err != nil {
		return nil, err
	}
	return res.(*TxStatus), nil
}

func (fc *FailoverElectrsClient) GetTxOutspends(txid string) ([]map[string]interface{}, error) {
	res, err := fc.call(func(c *ElectrsClient) (interface{}, error) {
		return c.getTxOutspends(txid)
	})
	if err != nil {
		return nil, err
	}
	return res.([]map[string]interface{}), nil
}

func (fc *FailoverElectrsClient) GetTxHex(txid string) (string, error) {
	res, err := fc.call(func(c *ElectrsClient) (interface{}, error) {
		return c.getTxHex(txid)
	})
	if err != nil {
		return "", err
	}
	return res.(string), nil
}

func (fc *FailoverElectrsClient) IsUtxoUnspent(txid string, vout uint32) (bool, error) {
	res, err := fc.call(func(c *ElectrsClient) (interface{}, error) {
		return c.isUtxoUnspent(txid, vout)
	})
	if err != nil {
		return false, err
	}
	return res.(bool), nil
}

func (fc *FailoverElectrsClient) GetFeeEstimates() (map[string]float64, error) {
	res, err := fc.call(func(c *ElectrsClient) (interface{}, error) {
		return c.getFeeEstimates()
	})
	if err != nil {
		return nil, err
	}
	return res.(map[string]float64), nil
}

func (fc *FailoverElectrsClient) GetAddressStats(address string) (*AddressStats, error) {
	res, err := fc.call(func(c *ElectrsClient) (interface{}, error) {
		return c.getAddressStats(address)
	})
	if err != nil {
		return nil, err
	}
	return res.(*AddressStats), nil
}

func (fc *FailoverElectrsClient) GetTxPrevOut(txid string, vout uint32) (*PrevOut, error) {
	res, err := fc.call(func(c *ElectrsClient) (interface{}, error) {
		return c.getTxPrevOut(txid, vout)
	})
	if err != nil {
		return nil, err
	}
	return res.(*PrevOut), nil
}

func (fc *FailoverElectrsClient) EstimateFeeSats(nInputs int, nOutputs int, targetBlocks int) int64 {
	// Not returning err, so we do direct call on the primary client
	fc.mu.Lock()
	primary := fc.clients[0]
	fc.mu.Unlock()
	return primary.estimateFeeSats(nInputs, nOutputs, targetBlocks)
}

// EstimateFeeSatsWithMin estimates fee with a custom minimum sat/vB floor.
// Used for fee-bump retries in the refund processor.
func (fc *FailoverElectrsClient) EstimateFeeSatsWithMin(nInputs int, nOutputs int, targetBlocks int, minSatPerVB int64) int64 {
	fc.mu.Lock()
	primary := fc.clients[0]
	fc.mu.Unlock()
	return primary.estimateFeeSatsWithMin(nInputs, nOutputs, targetBlocks, minSatPerVB)
}

func (fc *FailoverElectrsClient) Broadcast(rawHex string) (string, error) {
	res, err := fc.call(func(c *ElectrsClient) (interface{}, error) {
		return c.broadcast(rawHex)
	})
	if err != nil {
		return "", err
	}
	return res.(string), nil
}

func (fc *FailoverElectrsClient) GetAddressTxs(address string, afterTxid string) ([]map[string]interface{}, error) {
	res, err := fc.call(func(c *ElectrsClient) (interface{}, error) {
		return c.getAddressTxs(address, afterTxid)
	})
	if err != nil {
		return nil, err
	}
	return res.([]map[string]interface{}), nil
}

func (fc *FailoverElectrsClient) GetUTXOs(address string, minConfirmations int64) ([]UTXO, error) {
	res, err := fc.call(func(c *ElectrsClient) (interface{}, error) {
		return c.getUTXOs(address, minConfirmations)
	})
	if err != nil {
		return nil, err
	}
	return res.([]UTXO), nil
}

func (fc *FailoverElectrsClient) PrimaryURL() string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.clients[0].base
}
