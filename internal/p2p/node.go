// Package p2p implements go-libp2p-based inter-keeper coordination.
// Protocol: /wojak-keeper/v3 — Noise-encrypted streams over TCP/QUIC.
// Bootstrap VPS runs Circuit Relay v2 + DHT server; keeper nodes use
// hole punching with relay fallback and Kademlia DHT peer discovery.
package p2p

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"github.com/multiformats/go-multiaddr"
)

const (
	// KeeperProtocol is the custom libp2p protocol ID for inter-keeper messages.
	// v2: adds reject + recovery messages and the unified proposal flow.
	KeeperProtocol = "/wojak-keeper/v3"

	maxMsgBytes = 8 << 20 // 8 MB; a 100-input transfer tx hex fits comfortably

	keyFilePerm    = 0o600
	streamDeadline = 60 * time.Second
	connectTimeout = 30 * time.Second
	defaultTick    = 30 * time.Second
	helloTick      = 60 * time.Second
	discoveryTick  = 20 * time.Second
)

// Wire is the framing envelope for all inter-keeper P2P messages.
type Wire struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Message type constants.
const (
	TypeProposeTransfer   = "propose_transfer"
	TypeTransferApprove   = "transfer_approve"
	TypeTransferBroadcast = "transfer_broadcast"
	TypeReject            = "reject"
	TypeRequestProposal   = "request_proposal"
	TypeHello             = "hello"
)

// TransferProposal is a manual vault transfer/consolidation proposal
// built by one keeper and offered to the other two for approval.
type TransferProposal struct {
	ProposalId string   `json:"proposalId"`
	Vault      string   `json:"vault"` // P2SH address of the wallet
	To         string   `json:"to"`
	AmountSats int64    `json:"amountSats"`
	Memo       string   `json:"memo"`
	GrossSats  int64    `json:"grossSats"` // sum of inputs (leader's view)
	FeeSats    int64    `json:"feeSats"`   // informational
	RawTxHex   string   `json:"rawTxHex"`
	Txid       string   `json:"txid"`
	SignedBy   []string `json:"signedBy"`
}

// TransferApprove carries the merged (enriched) tx hex from one approving
// keeper, so receivers can merge additional signatures without re-walking
// history.
type TransferApprove struct {
	ProposalId string `json:"proposalId"`
	KeeperId   string `json:"keeperId"`
	SigHex     string `json:"sigHex"`
}

// BroadcastMsg tells peers a proposal was already pushed to the chain, so
// every UI flips from READY to BROADCASTED.
type BroadcastMsg struct {
	ProposalId string `json:"proposalId"`
	Txid       string `json:"txid"`
}

// RejectMsg is one keeper's explicit rejection of a proposal. It is
// informational: 2-of-3 still governs, but every node UI surfaces it.
type RejectMsg struct {
	ProposalId string `json:"proposalId"`
	KeeperId   string `json:"keeperId"`
	Reason     string `json:"reason"`
}

// RequestProposal asks the cluster to re-broadcast the body of a proposal
// whose id appeared in approve/reject traffic the node never received.
type RequestProposal struct {
	ProposalId string `json:"proposalId"`
}

// Hello is a lightweight hello for connectivity + config-checksum check.
type Hello struct {
	KeeperIndex int    `json:"keeperIndex"`
	PeerID      string `json:"peerId"`
	VaultHash   string `json:"vaultHash"` // sha256 of redeemScript+addrs
}

// Options configures a keeper P2P node.
type Options struct {
	KeyFile       string
	BindHost      string
	BindPort      int
	IsBootstrap   bool
	BootstrapAddr string   // relay VPS multiaddr (optional)
	PeerAddrs     []string // static peer multiaddrs
	SelfIndex     int
	VaultHash     string // consensus checksum
	UsePublicDHT  bool   // join the public IPFS DHT for rendezvous discovery
	ConnectTick   time.Duration
	BlockDial     time.Duration // per-dial timeout override (tests use short)
}

// Node is the libp2p layer for the trio.
type Node struct {
	h   host.Host
	d   *dht.IpfsDHT
	ctx context.Context

	peerMu        sync.RWMutex
	peerVaultHash map[peer.ID]string
	// knownPeers is guarded by peerMu; AddStaticPeers and DHT discovery
	// mutate it while connectLoop / getPeers read it concurrently.
	knownPeers []peer.AddrInfo

	selfIndex int
	vaultHash string
	peerAddrs []string

	handlers Handlers
}

// Callbacks — set by coordinator before traffic flows; called on messages.
type Handlers struct {
	OnProposeTransfer func(TransferProposal) error
	OnTransferApprove func(TransferApprove) error
	OnBroadcast       func(BroadcastMsg) error
	OnReject          func(RejectMsg) error
	OnRequestProposal func(RequestProposal) error
	OnHello           func(Hello) error
}

// NewNode creates and starts a libp2p node.
func NewNode(ctx context.Context, o Options) (*Node, error) {
	privKey, err := loadOrGenKey(o.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("p2p identity key: %w", err)
	}

	listenTCP := fmt.Sprintf("/ip4/%s/tcp/%d", o.BindHost, o.BindPort)
	listenQUIC := fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", o.BindHost, o.BindPort)

	var defaultBootstraps []peer.AddrInfo
	if o.UsePublicDHT {
		defaultBootstraps = dht.GetDefaultBootstrapPeerAddrInfos()
	}

	opts := []libp2p.Option{
		libp2p.Identity(privKey),
		libp2p.ListenAddrStrings(listenTCP, listenQUIC),
		libp2p.DefaultSecurity, // Noise + TLS
		libp2p.DefaultMuxers,   // Yamux
		libp2p.DefaultTransports,
		libp2p.NATPortMap(),
	}

	if o.IsBootstrap {
		opts = append(opts, libp2p.EnableRelayService(), libp2p.EnableNATService())
	} else {
		opts = append(opts, libp2p.EnableHolePunching())
		// Configure AutoRelay: prefer the operator's relay, then public DHT relays.
		var relayPeers []peer.AddrInfo
		if o.BootstrapAddr != "" {
			if info, err := parseMultiaddr(o.BootstrapAddr); err == nil {
				relayPeers = append(relayPeers, *info)
			} else {
				log.Printf("[p2p] WARNING: invalid bootstrap addr %q: %v", o.BootstrapAddr, err)
			}
		}
		if len(relayPeers) == 0 && o.UsePublicDHT {
			relayPeers = defaultBootstraps
		}
		if len(relayPeers) > 0 {
			opts = append(opts, libp2p.EnableAutoRelayWithStaticRelays(relayPeers))
		}
	}

	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("libp2p.New: %w", err)
	}

	log.Printf("[p2p] Host PeerID: %s", h.ID())
	for _, a := range h.Addrs() {
		log.Printf("[p2p]   listen: %s/p2p/%s", a, h.ID())
	}

	d, err := dht.New(ctx, h, dht.Mode(dht.ModeClient))
	if err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("kademlia DHT: %w", err)
	}
	if err := d.Bootstrap(ctx); err != nil {
		log.Printf("[p2p] DHT bootstrap: %v", err)
	}

	var known []peer.AddrInfo
	if o.BootstrapAddr != "" {
		if info, err := parseMultiaddr(o.BootstrapAddr); err == nil {
			known = append(known, *info)
		}
	}
	for _, a := range o.PeerAddrs {
		if info, err := parseMultiaddr(a); err != nil {
			log.Printf("[p2p] WARNING: invalid peer addr %q: %v", a, err)
		} else {
			known = append(known, *info)
		}
	}

	n := &Node{
		h:             h,
		d:             d,
		ctx:           ctx,
		knownPeers:    known,
		peerAddrs:     o.PeerAddrs,
		selfIndex:     o.SelfIndex,
		vaultHash:     o.VaultHash,
		peerVaultHash: make(map[peer.ID]string),
	}
	h.SetStreamHandler(protocol.ID(KeeperProtocol), n.handleStream)

	// Rendezvous discovery on the DHT (finds other keepers by consensus hash).
	if !o.IsBootstrap && o.UsePublicDHT {
		routingDiscovery := drouting.NewRoutingDiscovery(d)
		rendezvousNS := fmt.Sprintf("wojak-keeper-%s", o.VaultHash[:16])
		dutil.Advertise(ctx, routingDiscovery, rendezvousNS)
		go n.discoveryLoop(ctx, routingDiscovery, rendezvousNS)
	}

	connectTick := o.ConnectTick
	if connectTick <= 0 {
		connectTick = defaultTick
	}
	dialTimeout := o.BlockDial
	if dialTimeout <= 0 {
		dialTimeout = connectTimeout
	}

	if !o.IsBootstrap {
		go n.connectLoop(connectTick, dialTimeout)
		go n.helloLoop()
	}

	return n, nil
}

// ID returns the PeerID string.
func (n *Node) ID() string { return n.h.ID().String() }

// Peers returns the string PeerIDs of all currently connected peers.
func (n *Node) Peers() []string {
	var out []string
	for _, p := range n.h.Network().Peers() {
		if p == n.h.ID() {
			continue
		}
		out = append(out, p.String())
	}
	return out
}

// AdvertiseAddrs returns multiaddrs (".../p2p/<id>") this node can be dialed
// on over TCP — used by tests and by operators wiring a static trio.
func (n *Node) AdvertiseAddrs() []string {
	id := n.h.ID().String()
	var out []string
	for _, a := range n.h.Addrs() {
		if a == nil {
			continue
		}
		out = append(out, fmt.Sprintf("%s/p2p/%s", a, id))
	}
	return out
}

// SetHandlers attaches payload callbacks. Safe to call once before traffic.
func (n *Node) SetHandlers(hh Handlers) {
	n.handlers = hh
}

// addKnownPeer registers a peer only if its ID is not already known.
// Reports whether the peer was newly added. Safe for concurrent use.
func (n *Node) addKnownPeer(info peer.AddrInfo) bool {
	n.peerMu.Lock()
	defer n.peerMu.Unlock()
	for _, existing := range n.knownPeers {
		if existing.ID == info.ID {
			return false
		}
	}
	n.knownPeers = append(n.knownPeers, info)
	return true
}

// knownPeersSnapshot returns a copy of the static/discovered peer list.
// Safe for concurrent use; callers may dial from their own goroutines.
func (n *Node) knownPeersSnapshot() []peer.AddrInfo {
	n.peerMu.RLock()
	defer n.peerMu.RUnlock()
	out := make([]peer.AddrInfo, len(n.knownPeers))
	copy(out, n.knownPeers)
	return out
}

// AddStaticPeers registers peer multiaddrs after startup and dials them.
// Useful for wiring a fixed trio without depending on the DHT.
func (n *Node) AddStaticPeers(addrs []string, dialTimeout time.Duration) {
	for _, a := range addrs {
		info, err := parseMultiaddr(a)
		if err != nil {
			log.Printf("[p2p] invalid static peer addr %q: %v", a, err)
			continue
		}
		if !n.addKnownPeer(*info) {
			continue
		}
		go func(target peer.AddrInfo) {
			ctx, cancel := context.WithTimeout(n.ctx, dialTimeout)
			defer cancel()
			if err := n.h.Connect(ctx, target); err != nil {
				log.Printf("[p2p] static connect to %s: %v", target.ID, err)
			} else {
				log.Printf("[p2p] connected static peer %s", target.ID)
			}
		}(*info)
	}
}

// Close shuts down the libp2p host and DHT.
func (n *Node) Close() error {
	_ = n.d.Close()
	return n.h.Close()
}

// SendTo sends a single typed message to the given peer.
func (n *Node) SendTo(peerID peer.ID, msgType string, data interface{}) error {
	if n == nil {
		return nil
	}
	body, err := json.Marshal(data)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(Wire{Type: msgType, Data: body})
	if err != nil {
		return err
	}
	s, err := n.h.NewStream(n.ctx, peerID, protocol.ID(KeeperProtocol))
	if err != nil {
		return err
	}
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(streamDeadline))
	if _, err := s.Write(payload); err != nil {
		return err
	}
	if err := s.CloseWrite(); err != nil {
		return err
	}
	return nil
}

// getPeers returns peer IDs of all known peers except self.
func (n *Node) getPeers() []peer.ID {
	if n == nil {
		return nil
	}
	var out []peer.ID
	seen := make(map[peer.ID]bool)
	for _, ai := range n.knownPeersSnapshot() {
		if ai.ID == n.h.ID() || seen[ai.ID] {
			continue
		}
		seen[ai.ID] = true
		out = append(out, ai.ID)
	}
	// also include any currently connected peers
	for _, p := range n.h.Network().Peers() {
		if p == n.h.ID() || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// Broadcast sends a message to every known peer (best effort, parallel).
func (n *Node) Broadcast(msgType string, data interface{}) {
	if n == nil {
		return
	}
	for _, pid := range n.getPeers() {
		pid := pid
		go func() {
			if err := n.SendTo(pid, msgType, data); err != nil {
				log.Printf("[p2p] broadcast %s -> %s failed: %v", msgType, pid, err)
			}
		}()
	}
}

func (n *Node) handleStream(s network.Stream) {
	defer s.Close()
	_ = s.SetReadDeadline(time.Now().Add(streamDeadline))

	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		nr, err := s.Read(tmp)
		buf = append(buf, tmp[:nr]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return
		}
		if len(buf) > maxMsgBytes {
			log.Printf("[p2p] incoming message too large, dropping stream")
			return
		}
	}

	var wire Wire
	if err := json.Unmarshal(buf, &wire); err != nil {
		log.Printf("[p2p] bad message from %s: %v", s.Conn().RemotePeer(), err)
		return
	}

	remotePeer := s.Conn().RemotePeer()

	if wire.Type == TypeHello {
		var h Hello
		if err := json.Unmarshal(wire.Data, &h); err == nil {
			n.peerMu.Lock()
			n.peerVaultHash[remotePeer] = h.VaultHash
			n.peerMu.Unlock()
			if n.handlers.OnHello != nil {
				_ = n.handlers.OnHello(h)
			}
		}
		return
	}

	// For all operational messages, verify the peer's consensus checksum
	// if we ever learned it. Peers that have not found a hello yet still
	// fail-open (the keeper also rejects on application layer).
	n.peerMu.RLock()
	remoteHash, hasHello := n.peerVaultHash[remotePeer]
	n.peerMu.RUnlock()

	if hasHello && remoteHash != n.vaultHash {
		log.Printf("[p2p] REJECTED message %s from %s: consensus mismatch (peer=%s != local=%s)",
			wire.Type, remotePeer, remoteHash, n.vaultHash)
		return
	}

	_ = gracefulCall(func() error {
		switch wire.Type {
		case TypeProposeTransfer:
			var p TransferProposal
			if err := json.Unmarshal(wire.Data, &p); err != nil {
				return nil
			}
			if n.handlers.OnProposeTransfer != nil {
				return n.handlers.OnProposeTransfer(p)
			}
		case TypeTransferApprove:
			var m TransferApprove
			if err := json.Unmarshal(wire.Data, &m); err != nil {
				return nil
			}
			if n.handlers.OnTransferApprove != nil {
				return n.handlers.OnTransferApprove(m)
			}
		case TypeTransferBroadcast:
			var m BroadcastMsg
			if err := json.Unmarshal(wire.Data, &m); err != nil {
				return nil
			}
			if n.handlers.OnBroadcast != nil {
				return n.handlers.OnBroadcast(m)
			}
		case TypeReject:
			var m RejectMsg
			if err := json.Unmarshal(wire.Data, &m); err != nil {
				return nil
			}
			if n.handlers.OnReject != nil {
				return n.handlers.OnReject(m)
			}
		case TypeRequestProposal:
			var m RequestProposal
			if err := json.Unmarshal(wire.Data, &m); err != nil {
				return nil
			}
			if n.handlers.OnRequestProposal != nil {
				return n.handlers.OnRequestProposal(m)
			}
		}
		return nil
	})
}

// gracefulCall keeps the callback surface uniform; currently a passthrough.
func gracefulCall(fn func() error) error { return fn() }

func (n *Node) connectLoop(tick time.Duration, dialTimeout time.Duration) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		n.connectToKnownPeers(dialTimeout)
		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (n *Node) connectToKnownPeers(dialTimeout time.Duration) {
	for _, ai := range n.knownPeersSnapshot() {
		ai := ai
		if ai.ID == n.h.ID() {
			continue
		}
		if n.h.Network().Connectedness(ai.ID) == network.Connected {
			continue
		}
		go func() {
			ctx, cancel := context.WithTimeout(n.ctx, dialTimeout)
			defer cancel()
			if err := n.h.Connect(ctx, ai); err != nil {
				log.Printf("[p2p] connect to %s: %v", ai.ID, err)
			}
		}()
	}
}

func (n *Node) discoveryLoop(ctx context.Context, routingDiscovery *drouting.RoutingDiscovery, ns string) {
	ticker := time.NewTicker(discoveryTick)
	defer ticker.Stop()

	n.findPeers(ctx, routingDiscovery, ns)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.findPeers(ctx, routingDiscovery, ns)
		}
	}
}

func (n *Node) findPeers(ctx context.Context, routingDiscovery *drouting.RoutingDiscovery, ns string) {
	findCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	peerChan, err := routingDiscovery.FindPeers(findCtx, ns)
	if err != nil {
		return
	}

	for p := range peerChan {
		if p.ID == n.h.ID() || len(p.Addrs) == 0 {
			continue
		}
		if n.addKnownPeer(p) {
			log.Printf("[p2p] discovered new keeper peer via DHT: %s", p.ID)
		}
	}
}

func (n *Node) helloLoop() {
	ticker := time.NewTicker(helloTick)
	defer ticker.Stop()
	for {
		n.sendHello()
		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (n *Node) sendHello() {
	msg := Hello{KeeperIndex: n.selfIndex, PeerID: n.ID(), VaultHash: n.vaultHash}
	n.Broadcast(TypeHello, msg)
}

func loadOrGenKey(keyFile string) (libp2pcrypto.PrivKey, error) {
	if b, err := os.ReadFile(keyFile); err == nil {
		return libp2pcrypto.UnmarshalPrivateKey(b)
	}
	priv, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, err
	}
	b, err := libp2pcrypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyFile, b, keyFilePerm); err != nil {
		return nil, err
	}
	return priv, nil
}

func parseMultiaddr(addr string) (*peer.AddrInfo, error) {
	ma, err := multiaddr.NewMultiaddr(addr)
	if err != nil {
		return nil, err
	}
	return peer.AddrInfoFromP2pAddr(ma)
}
