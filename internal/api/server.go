// Package api exposes the keeper REST API + web UI: status, proposal list,
// verify/approve/reject/broadcast flow, vault balance and manual transfers for
// the single 2-of-3 multisig vault managed by this node.
package api

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/dedooxyz/dedoo-mm/internal/chain"
	"github.com/dedooxyz/dedoo-mm/internal/processor"
)

//go:embed webui
var webUIFS embed.FS

// Backend is implemented by processor.Keeper.
type Backend interface {
	Self() processor.SelfView
	PublicConfig() processor.PublicConfig
	Balance(wallet string) int64
	UTXOCount(wallet string) int
	ListProposals() []processor.ProposalView
	GetProposal(id string) (processor.ProposalView, error)
	ProposeTransfer(wallet, to string, amountSats int64, memo string) (string, error)
	ApproveProposal(id string) (string, error)
	RejectProposal(id, reason string) error
	BroadcastProposal(id string) (string, error)
	ListUTXOs(wallet string) ([]chain.UTXO, error)
	Peers() []string
	ChainTip() (int64, string)

	// multisig wallet manager
	ListAccounts() []processor.AccountView
	GetAccount(idOrAddress string) (processor.AccountView, error)
	CreateAccount(req processor.CreateAccountRequest) (processor.AccountView, bool, error)
	DeleteAccount(id string) error
	ListSigningKeys() []processor.KeyView
	AddSigningKey(wif string) (processor.KeyView, bool, error)
	RemoveSigningKey(pub string) error
}

// Server is the HTTP API handler.
type Server struct {
	backend  Backend
	mux      *http.ServeMux
	authUser string
	authPass string
}

// NewServer creates the API with routes mounted.
func NewServer(b Backend) *Server {
	s := &Server{backend: b, mux: http.NewServeMux()}
	s.routes()
	return s
}

// SetBasicAuth protects every route (UI + API) with HTTP Basic Auth.
// Leaving both values empty disables authentication (bind to localhost and
// use an SSH tunnel).
func (s *Server) SetBasicAuth(user, pass string) {
	s.authUser = user
	s.authPass = pass
}

// httpHandler wraps the mux with optional Basic Auth.
func (s *Server) httpHandler() http.Handler {
	if s.authUser == "" && s.authPass == "" {
		return s.mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(u), []byte(s.authUser)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(p), []byte(s.authPass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="Dedoo Multisig Manager", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/proposals", s.handleProposals)
	s.mux.HandleFunc("GET /api/proposal/{id}", s.handleProposal)
	s.mux.HandleFunc("GET /api/proposal/{id}/raw", s.handleProposalRaw)
	s.mux.HandleFunc("POST /api/transfer", s.handleTransfer)
	s.mux.HandleFunc("POST /api/proposal/{id}/approve", s.handleApprove)
	s.mux.HandleFunc("POST /api/proposal/{id}/reject", s.handleReject)
	s.mux.HandleFunc("POST /api/proposal/{id}/broadcast", s.handleBroadcast)
	s.mux.HandleFunc("GET /api/utxos", s.handleUTXOs)
	s.mux.HandleFunc("GET /api/chains", s.handleChains)
	s.mux.HandleFunc("GET /api/accounts", s.handleAccounts)
	s.mux.HandleFunc("POST /api/accounts", s.handleCreateAccount)
	s.mux.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)
	s.mux.HandleFunc("GET /api/keys", s.handleKeys)
	s.mux.HandleFunc("POST /api/keys", s.handleAddKey)
	s.mux.HandleFunc("DELETE /api/keys/{pub}", s.handleDeleteKey)

	// web UI
	s.mux.HandleFunc("GET /", s.handleIndex)
}

// ListenAndServe blocks serving until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           s.httpHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[api] encode: %v", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, code int, err error) {
	s.writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	tip, tipHash := s.backend.ChainTip()
	self := s.backend.Self()
	cfg := s.backend.PublicConfig()
	wallet := r.URL.Query().Get("wallet")
	if wallet == "" {
		wallet = cfg.VaultAddress
	}
	if wallet == "" {
		if accounts := s.backend.ListAccounts(); len(accounts) > 0 {
			wallet = accounts[0].Address
		}
	}
	address, name, hasKey := "", "", false
	required := 0
	var balance int64
	utxoCount := 0
	if wallet != "" {
		acct, err := s.backend.GetAccount(wallet)
		if err != nil {
			s.fail(w, http.StatusNotFound, err)
			return
		}
		address, name, hasKey, required = acct.Address, acct.Name, acct.HasKey, acct.Required
		balance = s.backend.Balance(acct.Address)
		utxoCount = s.backend.UTXOCount(acct.Address)
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"self": self,
		"config": map[string]interface{}{
			"chainName":          cfg.ChainName,
			"backend":            cfg.Backend,
			"vaultAddress":       address,
			"walletName":         name,
			"walletHasKey":       hasKey,
			"requiredSignatures": required,
			"maxTxInputs":        cfg.MaxTxInputs,
			"minTransferSats":    cfg.MinTransferSats,
			"coinbaseMaturity":   cfg.CoinbaseMaturity,
		},
		"balanceSats":          balance,
		"utxoCount":            utxoCount,
		"tipHeight":            tip,
		"tipHash":              tipHash,
		"peers":                s.backend.Peers(),
		"proposalsPendingMine": s.pendingCount(address),
	})
}

func (s *Server) pendingCount(wallet string) int {
	n := 0
	for _, p := range s.backend.ListProposals() {
		if p.Status == "pending" && (wallet == "" || p.Vault == wallet) {
			n++
		}
	}
	return n
}

func (s *Server) handleProposals(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"proposals": s.backend.ListProposals()})
}

func (s *Server) handleProposal(w http.ResponseWriter, r *http.Request) {
	v, err := s.backend.GetProposal(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleProposalRaw(w http.ResponseWriter, r *http.Request) {
	v, err := s.backend.GetProposal(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(v.RawHex))
}

type transferReq struct {
	Wallet     string `json:"wallet"`
	To         string `json:"to"`
	AmountSats int64  `json:"amountSats"`
	Memo       string `json:"memo"`
}

func (s *Server) handleTransfer(w http.ResponseWriter, r *http.Request) {
	var req transferReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("bad json"))
		return
	}
	id, err := s.backend.ProposeTransfer(req.Wallet, req.To, req.AmountSats, req.Memo)
	if err != nil {
		s.fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"proposalId": id})
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	raw, err := s.backend.ApproveProposal(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"signedTxHex": raw})
}

type rejectReq struct {
	Reason string `json:"reason"`
}

func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	var req rejectReq
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err == nil && len(strings.TrimSpace(string(body))) > 0 {
			_ = json.Unmarshal(body, &req)
		}
	}
	if err := s.backend.RejectProposal(r.PathValue("id"), req.Reason); err != nil {
		s.fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

func (s *Server) handleBroadcast(w http.ResponseWriter, r *http.Request) {
	txid, err := s.backend.BroadcastProposal(r.PathValue("id"))
	if err != nil {
		// 422 keeps the "ready" state; UI shows the error message
		s.fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"txid": txid})
}

func (s *Server) handleUTXOs(w http.ResponseWriter, r *http.Request) {
	wallet := r.URL.Query().Get("wallet")
	if wallet == "" {
		wallet = s.backend.PublicConfig().VaultAddress
	}
	acct, err := s.backend.GetAccount(wallet)
	if err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	utxos, err := s.backend.ListUTXOs(acct.Address)
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	total := int64(0)
	for _, u := range utxos {
		total += u.Value
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"vault": acct.Address, "walletName": acct.Name,
		"utxos": utxos, "totalSats": total,
	})
}

// handleChains lists the chain profiles available to the wallet creator.
func (s *Server) handleChains(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"chains": chain.ChainInfos()})
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"accounts": s.backend.ListAccounts()})
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req processor.CreateAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("bad json"))
		return
	}
	acct, created, err := s.backend.CreateAccount(req)
	if err != nil {
		s.fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, map[string]interface{}{"account": acct, "existing": !created})
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.DeleteAccount(r.PathValue("id")); err != nil {
		s.fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"keys": s.backend.ListSigningKeys()})
}

func (s *Server) handleAddKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WIF string `json:"wif"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("bad json"))
		return
	}
	key, created, err := s.backend.AddSigningKey(req.WIF)
	if err != nil {
		s.fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, map[string]interface{}{"key": key, "existing": !created})
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.RemoveSigningKey(r.PathValue("pub")); err != nil {
		s.fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// handleIndex serves the embedded single-page web UI.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	sub, err := fs.Sub(webUIFS, "webui")
	if err != nil {
		http.Error(w, "ui missing", http.StatusInternalServerError)
		return
	}
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "ui missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
