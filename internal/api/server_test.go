package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wojak-keeper/internal/chain"
	"wojak-keeper/internal/processor"
)

// fakeBackend implements Backend with canned values and call recording, so
// route tests run without a p2p mesh or chain connection.
type fakeBackend struct {
	self     processor.SelfView
	cfg      processor.PublicConfig
	balance  int64
	utxoN    int
	props    map[string]processor.ProposalView
	utxos    []chain.UTXO
	failUTX  bool
	accounts []processor.AccountView
	keys     []processor.KeyView

	// recording
	lastTransferReq [4]string // wallet, to, amount, memo
	lastReject      [2]string // id, reason
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	return &fakeBackend{
		self: processor.SelfView{
			SelfIndex:     1,
			PubHex:        "aabbcc",
			PeerID:        "12D3KooFAKE",
			AutoSign:      false,
			AutoBroadcast: false,
		},
		cfg: processor.PublicConfig{
			ChainName:        "wojakcoin",
			Backend:          "electrs",
			VaultAddress:     canonicalAPIVault,
			RequiredSigs:     2,
			MaxTxInputs:      100,
			MinTransferSats:  1_000,
			CoinbaseMaturity: 20,
		},
		balance: 325_000_000_000,
		utxoN:   51_352,
		props: map[string]processor.ProposalView{
			"tr1": {
				Id:      "tr1",
				Vault:   canonicalAPIVault,
				Status:  "pending",
				To:      canonicalAPIDest,
				Amount:  20_000_000,
				RawHex:  "feedface",
				Rejects: map[string]string{},
			},
			"tr2": {
				Id:     "tr2",
				Vault:  canonicalAPIVault,
				Status: "ready",
				RawHex: "cafebabe",
			},
		},
		utxos: []chain.UTXO{
			{Txid: "a", Vout: 0, Value: 12_000, Confirmations: 300},
			{Txid: "b", Vout: 1, Value: 8_000, Confirmations: 300},
		},
	}
}

const (
	// Test-only fixtures with no on-chain history.
	canonicalAPIVault = "3CHkEXVtx484ymadFo29rhkm7e3qtpNi2T"
	canonicalAPIDest  = "WQEGp13ekBXRHjioBvQRNAMYrXRf5dJAMU"
)

func (f *fakeBackend) Self() processor.SelfView             { return f.self }
func (f *fakeBackend) PublicConfig() processor.PublicConfig { return f.cfg }
func (f *fakeBackend) Balance(wallet string) int64          { return f.balance }
func (f *fakeBackend) UTXOCount(wallet string) int          { return f.utxoN }
func (f *fakeBackend) ListProposals() []processor.ProposalView {
	out := make([]processor.ProposalView, 0, len(f.props))
	for _, v := range f.props {
		out = append(out, v)
	}
	return out
}
func (f *fakeBackend) GetProposal(id string) (processor.ProposalView, error) {
	v, ok := f.props[id]
	if !ok {
		return v, fmt.Errorf("no proposal %s", id)
	}
	return v, nil
}
func (f *fakeBackend) ProposeTransfer(wallet, to string, amountSats int64, memo string) (string, error) {
	if amountSats < f.cfg.MinTransferSats {
		return "", fmt.Errorf("below minimum")
	}
	f.lastTransferReq = [4]string{wallet, to, fmt.Sprint(amountSats), memo}
	return "trNEW", nil
}
func (f *fakeBackend) ApproveProposal(id string) (string, error) {
	if _, ok := f.props[id]; !ok {
		return "", fmt.Errorf("unknown")
	}
	return "ababab", nil
}
func (f *fakeBackend) RejectProposal(id, reason string) error {
	if _, ok := f.props[id]; !ok {
		return fmt.Errorf("unknown")
	}
	f.lastReject = [2]string{id, reason}
	return nil
}
func (f *fakeBackend) BroadcastProposal(id string) (string, error) {
	if id == "tr2" {
		return "", errors.New("chain refused")
	}
	return "aa1bb2cc", nil
}
func (f *fakeBackend) ListUTXOs(wallet string) ([]chain.UTXO, error) {
	if f.failUTX {
		return nil, errors.New("electrs down")
	}
	return f.utxos, nil
}
func (f *fakeBackend) Peers() []string           { return []string{"A", "B"} }
func (f *fakeBackend) ChainTip() (int64, string) { return 42_000, "0000000000tip" }

func (f *fakeBackend) ListAccounts() []processor.AccountView {
	return append([]processor.AccountView{{
		Id: "active", Name: "Active signing vault", Chain: "wojakcoin", ChainKey: "wojakcoin",
		Address: canonicalAPIVault, Required: 2, Active: true,
	}}, f.accounts...)
}

func (f *fakeBackend) CreateAccount(req processor.CreateAccountRequest) (processor.AccountView, bool, error) {
	if req.Name == "" {
		return processor.AccountView{}, false, errors.New("name required")
	}
	a := processor.AccountView{
		Id: "acct_test", Name: req.Name, Chain: "wojakcoin", ChainKey: "wojakcoin",
		Address: "3SavedWalletAddressForTests111111111", Required: 2,
	}
	f.accounts = append(f.accounts, a)
	return a, true, nil
}

func (f *fakeBackend) GetAccount(idOrAddress string) (processor.AccountView, error) {
	for _, a := range f.ListAccounts() {
		if a.Id == idOrAddress || a.Address == idOrAddress {
			return a, nil
		}
	}
	return processor.AccountView{}, fmt.Errorf("no wallet %s", idOrAddress)
}

func (f *fakeBackend) DeleteAccount(id string) error {
	if id == "active" {
		return errors.New("active vault cannot be removed")
	}
	for i, a := range f.accounts {
		if a.Id == id {
			f.accounts = append(f.accounts[:i], f.accounts[i+1:]...)
			return nil
		}
	}
	return errors.New("no saved wallet")
}

func (f *fakeBackend) ListSigningKeys() []processor.KeyView {
	if f.keys == nil {
		return []processor.KeyView{}
	}
	return f.keys
}
func (f *fakeBackend) AddSigningKey(wif string) (processor.KeyView, bool, error) {
	if wif == "" {
		return processor.KeyView{}, false, errors.New("wif required")
	}
	k := processor.KeyView{Pubkey: "02importedkey"}
	f.keys = append(f.keys, k)
	return k, true, nil
}
func (f *fakeBackend) RemoveSigningKey(pub string) error {
	for i, k := range f.keys {
		if k.Pubkey == pub {
			f.keys = append(f.keys[:i], f.keys[i+1:]...)
			return nil
		}
	}
	return errors.New("no key")
}

// --- helpers ----------------------------------------------------------------

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func postJSON(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return m
}

// --- tests ------------------------------------------------------------------

func newTestServer(t *testing.T) (*Server, *fakeBackend) {
	t.Helper()
	fb := newFakeBackend(t)
	return NewServer(fb), fb
}

func TestStatusRoute(t *testing.T) {
	s, _ := newTestServer(t)
	rec := get(t, s, "/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	m := decode(t, rec)
	if m["balanceSats"] != float64(325_000_000_000) {
		t.Fatalf("balance = %v", m["balanceSats"])
	}
	if m["utxoCount"] != float64(51_352) {
		t.Fatalf("utxoCount = %v", m["utxoCount"])
	}
	if got := m["tipHeight"]; got != float64(42_000) {
		t.Fatalf("tipHeight = %v", got)
	}
	if ps, ok := m["peers"].([]interface{}); !ok || len(ps) != 2 {
		t.Fatalf("peers: %v", m["peers"])
	}
	cfg := m["config"].(map[string]interface{})
	if cfg["chainName"] != "wojakcoin" || cfg["requiredSignatures"] != float64(2) ||
		cfg["vaultAddress"] != canonicalAPIVault || cfg["maxTxInputs"] != float64(100) ||
		cfg["minTransferSats"] != float64(1_000) {
		t.Fatalf("config block: %v", cfg)
	}
	self := m["self"].(map[string]interface{})
	if self["autoSign"] != false || self["peerId"] != "12D3KooFAKE" {
		t.Fatalf("self block: %v", self)
	}
	if m["proposalsPendingMine"] != float64(1) {
		t.Fatalf("pending count = %v, want 1 (tr1 pending)", m["proposalsPendingMine"])
	}
}

func TestProposalsRoutes(t *testing.T) {
	s, _ := newTestServer(t)

	rec := get(t, s, "/api/proposals")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	list, ok := decode(t, rec)["proposals"].([]interface{})
	if !ok || len(list) != 2 {
		t.Fatalf("proposals list: %v", list)
	}

	// found
	rec = get(t, s, "/api/proposal/tr1")
	if rec.Code != http.StatusOK {
		t.Fatalf("found status %d", rec.Code)
	}
	if d := decode(t, rec)["status"]; d != "pending" {
		t.Fatalf("status = %v", d)
	}

	// missing
	if rec := get(t, s, "/api/proposal/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing status %d", rec.Code)
	}

	// raw
	rec = get(t, s, "/api/proposal/tr2/raw")
	if rec.Code != http.StatusOK || rec.Body.String() != "cafebabe" {
		t.Fatalf("raw: %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("raw content-type %q", ct)
	}
	if rec := get(t, s, "/api/proposal/nope/raw"); rec.Code != http.StatusNotFound {
		t.Fatalf("raw missing status %d", rec.Code)
	}
}

func TestTransferRoute(t *testing.T) {
	s, fb := newTestServer(t)

	rec := postJSON(t, s, "/api/transfer", `{"to":"WQEGp13ekBXRHjioBvQRNAMYrXRf5dJAMU","amountSats":20000000,"memo":"api test"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if id := decode(t, rec)["proposalId"]; id != "trNEW" {
		t.Fatalf("proposalId = %v", id)
	}
	if fb.lastTransferReq != [4]string{"", canonicalAPIDest, "20000000", "api test"} {
		t.Fatalf("propose args: %v", fb.lastTransferReq)
	}

	// legacy "vault" field is accepted and ignored
	rec = postJSON(t, s, "/api/transfer", `{"vault":"dev","to":"WQEGp13ekBXRHjioBvQRNAMYrXRf5dJAMU","amountSats":20000000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy vault field should be ignored, got %d %s", rec.Code, rec.Body.String())
	}

	if rec := postJSON(t, s, "/api/transfer", `{not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json status %d", rec.Code)
	}
	if rec := postJSON(t, s, "/api/transfer", `{"to":"WQEGp13ekBXRHjioBvQRNAMYrXRf5dJAMU","amountSats":1}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("below min status %d", rec.Code)
	}
}

func TestApprovalRoutes(t *testing.T) {
	s, _ := newTestServer(t)

	rec := postJSON(t, s, "/api/proposal/tr1/approve", "")
	if rec.Code != http.StatusOK || decode(t, rec)["signedTxHex"] != "ababab" {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(t, s, "/api/proposal/nope/approve", ""); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("approve missing status %d", rec.Code)
	}
}

func TestRejectCarriesReason(t *testing.T) {
	s, fb := newTestServer(t)
	if rec := postJSON(t, s, "/api/proposal/tr1/reject", `{"reason":"fishy"}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if fb.lastReject != [2]string{"tr1", "fishy"} {
		t.Fatalf("reject args: %v", fb.lastReject)
	}
	// empty body still ok (reason "")
	if rec := postJSON(t, s, "/api/proposal/tr1/reject", ""); rec.Code != http.StatusOK {
		t.Fatalf("empty body status %d", rec.Code)
	}
	// unknown id → 422
	if rec := postJSON(t, s, "/api/proposal/zzz/reject", `{"reason":"x"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown reject status %d", rec.Code)
	}
}

func TestBroadcastRoute(t *testing.T) {
	s, _ := newTestServer(t)

	rec := postJSON(t, s, "/api/proposal/tr1/broadcast", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if txid := decode(t, rec)["txid"]; txid != "aa1bb2cc" {
		t.Fatalf("txid = %v", txid)
	}

	rec = postJSON(t, s, "/api/proposal/tr2/broadcast", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("error path status %d", rec.Code)
	}
	if msg := decode(t, rec)["error"]; msg != "chain refused" {
		t.Fatalf("error msg = %v", msg)
	}
}

func TestUTXORoute(t *testing.T) {
	s, fb := newTestServer(t)

	rec := get(t, s, "/api/utxos")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	m := decode(t, rec)
	if m["totalSats"] != float64(20_000) {
		t.Fatalf("total = %v", m["totalSats"])
	}
	if us, ok := m["utxos"].([]interface{}); !ok || len(us) != 2 {
		t.Fatalf("utxos: %v", m["utxos"])
	}
	if m["vault"] != canonicalAPIVault {
		t.Fatalf("vault echo: %v", m["vault"])
	}

	// legacy ?vault=... is harmless (single vault)
	if rec := get(t, s, "/api/utxos?vault=dev"); rec.Code != http.StatusOK {
		t.Fatalf("legacy vault param status %d", rec.Code)
	}

	fb.failUTX = true
	if rec := get(t, s, "/api/utxos"); rec.Code != http.StatusBadGateway {
		t.Fatalf("backend error status %d", rec.Code)
	}
}

func TestRemovedLegacyRoutes(t *testing.T) {
	s, _ := newTestServer(t)
	// The old LP/dev + sweep surface is gone.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/dev/balance"},
		{http.MethodGet, "/api/dev/proposals"},
		{http.MethodPost, "/api/dev/propose"},
		{http.MethodPost, "/api/dev/approve"},
		{http.MethodPost, "/api/sweep"},
	} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("%s %s must not be served anymore", tc.method, tc.path)
		}
	}
}

func TestBasicAuth(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetBasicAuth("holder", "s3cret-pass")
	h := s.httpHandler()

	// No credentials → 401 + challenge.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-cred status %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate header")
	}

	// Correct credentials → 200.
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.SetBasicAuth("holder", "s3cret-pass")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authed status %d", rec.Code)
	}

	// Wrong password → 401.
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.SetBasicAuth("holder", "wrong-pass")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-pass status %d", rec.Code)
	}
}

func TestChainsAndAccountsRoutes(t *testing.T) {
	s, _ := newTestServer(t)

	rec := get(t, s, "/api/chains")
	if rec.Code != http.StatusOK {
		t.Fatalf("chains status %d", rec.Code)
	}
	chains, ok := decode(t, rec)["chains"].([]interface{})
	if !ok || len(chains) < 2 {
		t.Fatalf("chains: %v", chains)
	}

	rec = get(t, s, "/api/accounts")
	accts, ok := decode(t, rec)["accounts"].([]interface{})
	if !ok || len(accts) != 1 {
		t.Fatalf("accounts: %v", accts)
	}
	if accts[0].(map[string]interface{})["active"] != true {
		t.Fatal("first account must be the active vault")
	}

	rec = postJSON(t, s, "/api/accounts", `{"name":"Ops Vault","chain":"wojakcoin"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create account status %d: %s", rec.Code, rec.Body.String())
	}
	if decode(t, rec)["account"].(map[string]interface{})["name"] != "Ops Vault" {
		t.Fatal("created account payload missing")
	}

	// active vault cannot be removed
	req := httptest.NewRequest(http.MethodDelete, "/api/accounts/active", nil)
	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("delete active status %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/accounts/acct_test", nil)
	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete saved status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestKeysRoutes(t *testing.T) {
	s, _ := newTestServer(t)
	rec := get(t, s, "/api/keys")
	if rec.Code != http.StatusOK {
		t.Fatalf("keys status %d", rec.Code)
	}
	if _, ok := decode(t, rec)["keys"].([]interface{}); !ok {
		t.Fatal("keys payload missing")
	}

	rec = postJSON(t, s, "/api/keys", `{"wif":""}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty wif status %d", rec.Code)
	}
	rec = postJSON(t, s, "/api/keys", `{"wif":"test-wif"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add key status %d: %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/keys/02importedkey", nil)
	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete key status %d", rec.Code)
	}
}

func TestWebUIIndex(t *testing.T) {
	s, _ := newTestServer(t)
	rec := get(t, s, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("index status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<!doctype html>") || !strings.Contains(body, "Dedoo") {
		t.Fatal("index.html missing expected markers")
	}
	// UI is English.
	for _, marker := range []string{"Dedoo", "New transfer", "Approvals", "History", "Vault"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("index.html missing layout marker %q", marker)
		}
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control %q", cc)
	}
	// unknown UI path → 404
	if rec := get(t, s, "/does-not-exist"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path status %d", rec.Code)
	}
}
