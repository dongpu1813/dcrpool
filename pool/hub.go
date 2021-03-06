// Copyright (c) 2019 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package pool

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"decred.org/dcrwallet/rpc/walletrpc"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrutil/v3"
	chainjson "github.com/decred/dcrd/rpc/jsonrpc/types/v2"
	"github.com/decred/dcrd/rpcclient/v6"
	"github.com/decred/dcrd/wire"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc"
)

const (
	// MaxReorgLimit is an estimated maximum chain reorganization limit.
	// That is, it is highly improbable for the the chain to reorg beyond six
	// blocks from the chain tip.
	MaxReorgLimit = 6

	// getworkDataLen is the length of the data field of the getwork RPC.
	// It consists of the serialized block header plus the internal blake256
	// padding.  The internal blake256 padding consists of a single 1 bit
	// followed by zeros and a final 1 bit in order to pad the message out
	// to 56 bytes followed by length of the message in bits encoded as a
	// big-endian uint64 (8 bytes).  Thus, the resulting length is a
	// multiple of the blake256 block size (64 bytes).  Given the padding
	// requires at least a 1 bit and 64 bits for the padding, the following
	// converts the block header length and hash block size to bits in order
	// to ensure the correct number of hash blocks are calculated and then
	// multiplies the result by the block hash block size in bytes.
	getworkDataLen = (1 + ((wire.MaxBlockHeaderPayload*8 + 65) /
		(chainhash.HashBlockSize * 8))) * chainhash.HashBlockSize

	// NewParent is the reason given when a work notification is generated
	// because there is a new chain tip.
	NewParent = "newparent"
	// NewVotes is the reason given when a work notification is generated
	// because new votes were received.
	NewVotes = "newvotes"
	// NewTxns is the reason given when a work notification is generated
	// because new transactions were received.
	NewTxns = "newtxns"
)

// CacheUpdateEvent represents the a cache update event message.
type CacheUpdateEvent int

// Constants for the type of template regeneration event messages.
const (
	// Confirmed indicates an accepted work has been updated as
	// confirmed mined.
	Confirmed CacheUpdateEvent = iota

	// Unconfirmed indicates a previously confimed mined work has been
	// updated to unconfirmed due to a reorg.
	Unconfirmed

	// ConnectedClient indicates a new client has connected to the pool.
	ConnectedClient

	// DisconnectedClient indicates a client has disconnected from the pool.
	DisconnectedClient

	// ClaimedShare indicates work quotas for participating clients have
	// been updated.
	ClaimedShare

	// DividendsPaid indicates dividends due participating miners have been
	// paid.
	DividendsPaid
)

var (
	// soloMaxGenTime is the threshold (in seconds) at which pool clients will
	// generate a valid share when solo pool mode is activated. This is set to a
	// high value to reduce the number of round trips to the pool by connected
	// pool clients since pool shares are a non factor in solo pool mode.
	soloMaxGenTime = time.Second * 28
)

// WalletConnection defines the functionality needed by a wallet
// grpc connection for the pool.
type WalletConnection interface {
	SignTransaction(context.Context, *walletrpc.SignTransactionRequest, ...grpc.CallOption) (*walletrpc.SignTransactionResponse, error)
	PublishTransaction(context.Context, *walletrpc.PublishTransactionRequest, ...grpc.CallOption) (*walletrpc.PublishTransactionResponse, error)
}

// NodeConnection defines the functionality needed by a mining node
// connection for the pool.
type NodeConnection interface {
	GetTxOut(context.Context, *chainhash.Hash, uint32, bool) (*chainjson.GetTxOutResult, error)
	CreateRawTransaction(context.Context, []chainjson.TransactionInput, map[dcrutil.Address]dcrutil.Amount, *int64, *int64) (*wire.MsgTx, error)
	GetWorkSubmit(context.Context, string) (bool, error)
	GetWork(context.Context) (*chainjson.GetWorkResult, error)
	GetBlockVerbose(context.Context, *chainhash.Hash, bool) (*chainjson.GetBlockVerboseResult, error)
	GetBlock(context.Context, *chainhash.Hash) (*wire.MsgBlock, error)
	NotifyWork(context.Context) error
	NotifyBlocks(context.Context) error
	Shutdown()
}

// HubConfig represents configuration details for the hub.
type HubConfig struct {
	ActiveNet             *chaincfg.Params
	DB                    *bolt.DB
	PoolFee               float64
	MaxGenTime            time.Duration
	PaymentMethod         string
	LastNPeriod           time.Duration
	WalletPass            string
	SoloPool              bool
	PoolFeeAddrs          []dcrutil.Address
	AdminPass             string
	Secret                string
	NonceIterations       float64
	MinerPorts            map[string]uint32
	MaxConnectionsPerHost uint32
	WalletAccount         uint32
}

// Hub maintains the set of active clients and facilitates message broadcasting
// to all active clients.
type Hub struct {
	clients int32 // update atomically.

	db             *bolt.DB
	cfg            *HubConfig
	limiter        *RateLimiter
	nodeConn       NodeConnection
	walletClose    func() error
	walletConn     WalletConnection
	notifClient    walletrpc.WalletService_ConfirmationNotificationsClient
	poolDiffs      *DifficultySet
	paymentMgr     *PaymentMgr
	chainState     *ChainState
	connections    map[string]uint32
	connectionsMtx sync.RWMutex
	cancel         context.CancelFunc
	endpoints      []*Endpoint
	blake256Pad    []byte
	wg             *sync.WaitGroup
	cacheCh        chan CacheUpdateEvent
}

// SignalCache sends the provided cache update event to the gui cache.
func (h *Hub) SignalCache(event CacheUpdateEvent) {
	select {
	case h.cacheCh <- event:
	default:
		// Non-breaking send fallthrough.
	}
}

// FetchCacheChannel returns the gui cache signal chanel.
func (h *Hub) FetchCacheChannel() chan CacheUpdateEvent {
	return h.cacheCh
}

// SetNodeConnection sets the mining node connection.
func (h *Hub) SetNodeConnection(conn NodeConnection) {
	h.nodeConn = conn
}

// SetWalletConnection sets the wallet connection and it's associated close.
func (h *Hub) SetWalletConnection(conn WalletConnection, close func() error) {
	h.walletConn = conn
	h.walletClose = close
}

// SetTxConfNotifClient sets the wallet transaction confirmation notification
// client.
func (h *Hub) SetTxConfNotifClient(conn walletrpc.WalletService_ConfirmationNotificationsClient) {
	h.notifClient = conn
}

// persistPoolMode saves the pool mode to the db.
func (h *Hub) persistPoolMode(tx *bolt.Tx, mode uint32) error {
	pbkt := tx.Bucket(poolBkt)
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, mode)
	return pbkt.Put(soloPool, b)
}

// generateBlake256Pad creates the extra padding needed for work
// submissions over the getwork RPC.
func generateBlake256Pad() []byte {
	blake256Pad := make([]byte, getworkDataLen-wire.MaxBlockHeaderPayload)
	blake256Pad[0] = 0x80
	blake256Pad[len(blake256Pad)-9] |= 0x01
	binary.BigEndian.PutUint64(blake256Pad[len(blake256Pad)-8:],
		wire.MaxBlockHeaderPayload*8)
	return blake256Pad
}

// NewHub initializes the mining pool hub.
func NewHub(cancel context.CancelFunc, hcfg *HubConfig) (*Hub, error) {
	h := &Hub{
		cfg:         hcfg,
		db:          hcfg.DB,
		limiter:     NewRateLimiter(),
		wg:          new(sync.WaitGroup),
		connections: make(map[string]uint32),
		cacheCh:     make(chan CacheUpdateEvent, bufferSize),
		cancel:      cancel,
	}
	h.blake256Pad = generateBlake256Pad()
	powLimit := new(big.Rat).SetInt(h.cfg.ActiveNet.PowLimit)
	maxGenTime := h.cfg.MaxGenTime
	if h.cfg.SoloPool {
		maxGenTime = soloMaxGenTime
	}

	log.Infof("Maximum work submission generation time at "+
		"pool difficulty is %s.", maxGenTime)

	var err error
	h.poolDiffs, err = NewDifficultySet(h.cfg.ActiveNet, powLimit, maxGenTime)
	if err != nil {
		return nil, err
	}

	pCfg := &PaymentMgrConfig{
		DB:                     h.db,
		ActiveNet:              h.cfg.ActiveNet,
		PoolFee:                h.cfg.PoolFee,
		LastNPeriod:            h.cfg.LastNPeriod,
		SoloPool:               h.cfg.SoloPool,
		PaymentMethod:          h.cfg.PaymentMethod,
		PoolFeeAddrs:           h.cfg.PoolFeeAddrs,
		WalletAccount:          h.cfg.WalletAccount,
		WalletPass:             h.cfg.WalletPass,
		GetBlockConfirmations:  h.getBlockConfirmations,
		GetTxConfNotifications: h.getTxConfNotifications,
		FetchTxCreator:         func() TxCreator { return h.nodeConn },
		FetchTxBroadcaster:     func() TxBroadcaster { return h.walletConn },
	}
	h.paymentMgr, err = NewPaymentMgr(pCfg)
	if err != nil {
		return nil, err
	}

	sCfg := &ChainStateConfig{
		DB:                          h.db,
		SoloPool:                    h.cfg.SoloPool,
		PayDividends:                h.paymentMgr.payDividends,
		PendingPaymentsAtHeight:     h.paymentMgr.pendingPaymentsAtHeight,
		PendingPaymentsForBlockHash: h.paymentMgr.pendingPaymentsForBlockHash,
		GeneratePayments:            h.paymentMgr.generatePayments,
		GetBlock:                    h.getBlock,
		GetBlockConfirmations:       h.getBlockConfirmations,
		Cancel:                      h.cancel,
		SignalCache:                 h.SignalCache,
		HubWg:                       h.wg,
	}
	h.chainState = NewChainState(sCfg)

	if !h.cfg.SoloPool {
		log.Infof("Payment method is %s.", strings.ToUpper(hcfg.PaymentMethod))
	} else {
		log.Infof("Solo pool mode active.")
	}

	err = h.db.Update(func(tx *bolt.Tx) error {
		mode := uint32(0)
		if h.cfg.SoloPool {
			mode = 1
		}
		return h.persistPoolMode(tx, mode)
	})
	if err != nil {
		return nil, err
	}
	return h, nil
}

// submitWork sends solved block data to the consensus daemon for evaluation.
func (h *Hub) submitWork(ctx context.Context, data *string) (bool, error) {
	if h.nodeConn == nil {
		return false, MakeError(ErrOther, "node connection unset", nil)
	}

	return h.nodeConn.GetWorkSubmit(ctx, *data)
}

// getWork fetches available work from the consensus daemon.
func (h *Hub) getWork(ctx context.Context) (string, string, error) {
	if h.nodeConn == nil {
		return "", "", MakeError(ErrOther, "node connection unset", nil)
	}

	work, err := h.nodeConn.GetWork(ctx)
	if err != nil {
		return "", "", err
	}
	return work.Data, work.Target, err
}

// getTxConfNotifications streams transaction confirmation notifications for
// the provided transaction hashes.
func (h *Hub) getTxConfNotifications(txHashes []*chainhash.Hash, stopAfter int32) (func() (*walletrpc.ConfirmationNotificationsResponse, error), error) {
	hashes := make([][]byte, 0, len(txHashes))
	for _, hash := range txHashes {
		hashes = append(hashes, hash[:])
	}

	req := &walletrpc.ConfirmationNotificationsRequest{
		TxHashes:  hashes,
		StopAfter: stopAfter,
	}

	err := h.notifClient.Send(req)
	if err != nil {
		return nil, err
	}

	return h.notifClient.Recv, nil
}

// getBlockConfirmation returns the number of block confirmations for the
// provided block height.
func (h *Hub) getBlockConfirmations(ctx context.Context, hash *chainhash.Hash) (int64, error) {
	info, err := h.nodeConn.GetBlockVerbose(ctx, hash, false)
	if err != nil {
		return 0, err
	}
	return info.Confirmations, nil
}

// WithinLimit returns if a client is within its request limits.
func (h *Hub) WithinLimit(ip string, clientType int) bool {
	return h.limiter.withinLimit(ip, clientType)
}

// FetchLastWorkHeight returns the last work height of the pool.
func (h *Hub) FetchLastWorkHeight() uint32 {
	return h.chainState.fetchLastWorkHeight()
}

// FetchLastPaymentHeight returns the last payment height of the pool.
func (h *Hub) FetchLastPaymentHeight() uint32 {
	return h.paymentMgr.fetchLastPaymentHeight()
}

// getBlock fetches the blocks associated with the provided block hash.
func (h *Hub) getBlock(ctx context.Context, blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	if h.nodeConn == nil {
		return nil, MakeError(ErrOther, "node connection unset", nil)
	}

	return h.nodeConn.GetBlock(ctx, blockHash)
}

// fetchHostConnections returns the client connection count for the
// provided host.
func (h *Hub) fetchHostConnections(host string) uint32 {
	h.connectionsMtx.RLock()
	defer h.connectionsMtx.RUnlock()
	return h.connections[host]
}

// addConnection records a new client connection for the provided host.
func (h *Hub) addConnection(host string) {
	h.connectionsMtx.Lock()
	h.connections[host]++
	h.connectionsMtx.Unlock()
	atomic.AddInt32(&h.clients, 1)
}

// removeConnection removes a client connection for the provided host.
func (h *Hub) removeConnection(host string) {
	h.connectionsMtx.Lock()
	h.connections[host]--
	h.connectionsMtx.Unlock()
	atomic.AddInt32(&h.clients, -1)
}

// processWork parses work received and dispatches a work notification to all
// connected pool clients.
func (h *Hub) processWork(headerE string) {
	heightD, err := hex.DecodeString(headerE[256:264])
	if err != nil {
		log.Errorf("failed to decode block height %s: %v", string(heightD), err)
		return
	}
	height := binary.LittleEndian.Uint32(heightD)
	log.Tracef("New work at height #%d received: %s", height, headerE)
	h.chainState.setLastWorkHeight(height)
	if !h.HasClients() {
		return
	}

	blockVersion := headerE[:8]
	prevBlock := headerE[8:72]
	genTx1 := headerE[72:288]
	nBits := headerE[232:240]
	nTime := headerE[272:280]
	genTx2 := headerE[352:360]
	job, err := NewJob(headerE, height)
	if err != nil {
		log.Errorf("failed to create job: %v", err)
		return
	}
	err = job.Create(h.db)
	if err != nil {
		log.Errorf("failed to persist job: %v", err)
		return
	}
	workNotif := WorkNotification(job.UUID, prevBlock, genTx1, genTx2,
		blockVersion, nBits, nTime, true)
	for _, endpoint := range h.endpoints {
		endpoint.clientsMtx.Lock()
		for _, client := range endpoint.clients {
			select {
			case client.ch <- workNotif:
			default:
			}
		}
		endpoint.clientsMtx.Unlock()
	}
}

// Listen creates listeners for all supported pool clients.
func (h *Hub) Listen() error {
	for miner, port := range h.cfg.MinerPorts {
		diffInfo, err := h.poolDiffs.fetchMinerDifficulty(miner)
		if err != nil {
			return err
		}
		eCfg := &EndpointConfig{
			ActiveNet:             h.cfg.ActiveNet,
			DB:                    h.db,
			SoloPool:              h.cfg.SoloPool,
			Blake256Pad:           h.blake256Pad,
			NonceIterations:       h.cfg.NonceIterations,
			MaxConnectionsPerHost: h.cfg.MaxConnectionsPerHost,
			HubWg:                 h.wg,
			SubmitWork:            h.submitWork,
			FetchCurrentWork:      h.chainState.fetchCurrentWork,
			WithinLimit:           h.limiter.withinLimit,
			AddConnection:         h.addConnection,
			RemoveConnection:      h.removeConnection,
			FetchHostConnections:  h.fetchHostConnections,
			MaxGenTime:            h.cfg.MaxGenTime,
			SignalCache:           h.SignalCache,
		}
		endpoint, err := NewEndpoint(eCfg, diffInfo, port, miner)
		if err != nil {
			desc := fmt.Sprintf("unable to create %s listener", miner)
			return MakeError(ErrOther, desc, err)
		}
		h.endpoints = append(h.endpoints, endpoint)
	}
	return nil
}

// CloseListeners terminates listeners created by endpoints of the hub. This
// should only be used in the pool's shutdown process the hub is not running.
func (h *Hub) CloseListeners() {
	for _, e := range h.endpoints {
		e.listener.Close()
	}
}

// CreateNotificationHandlers returns handlers for block and work notifications.
func (h *Hub) CreateNotificationHandlers() *rpcclient.NotificationHandlers {
	return &rpcclient.NotificationHandlers{
		OnBlockConnected: func(headerB []byte, transactions [][]byte) {
			h.chainState.connCh <- &blockNotification{
				Header: headerB,
				Done:   make(chan bool),
			}
		},
		OnBlockDisconnected: func(headerB []byte) {
			h.chainState.discCh <- &blockNotification{
				Header: headerB,
				Done:   make(chan bool),
			}
		},
		OnWork: func(headerB []byte, target []byte, reason string) {
			currWork := hex.EncodeToString(headerB)
			switch reason {
			case NewTxns:
				h.chainState.setCurrentWork(currWork)

			case NewParent, NewVotes:
				h.chainState.setCurrentWork(currWork)
				h.processWork(currWork)
			}
		},
	}
}

// FetchWork queries the mining node for work. This should be called
// immediately the pool starts to avoid for a work notification.
func (h *Hub) FetchWork(ctx context.Context) error {
	work, _, err := h.getWork(ctx)
	if err != nil {
		desc := "unable to fetch current work"
		return MakeError(ErrOther, desc, err)
	}
	h.chainState.setCurrentWork(work)
	return nil
}

// HasClients asserts the mining pool has clients.
func (h *Hub) HasClients() bool {
	return atomic.LoadInt32(&h.clients) > 0
}

// backup persists a copy of the database to file  on shutdown.
func (h *Hub) backup(ctx context.Context) {
	<-ctx.Done()
	log.Tracef("backing up db.")
	backupPath := filepath.Join(filepath.Dir(h.db.Path()), backupFile)
	err := backup(h.db, backupPath)
	if err != nil {
		log.Errorf("unable to backup db: %v", err)
	}
	h.wg.Done()
}

// shutdown tears down the hub and releases resources used.
func (h *Hub) shutdown() {
	if !h.cfg.SoloPool {
		if h.walletClose != nil {
			h.walletClose()
		}
	}
	if h.nodeConn != nil {
		h.nodeConn.Shutdown()
	}
	if h.notifClient != nil {
		_ = h.notifClient.CloseSend()
	}
	h.db.Close()
}

// Run handles the process lifecycles of the pool hub.
func (h *Hub) Run(ctx context.Context) {
	for _, e := range h.endpoints {
		go e.run(ctx)
		h.wg.Add(1)
	}
	go h.chainState.handleChainUpdates(ctx)
	h.wg.Add(1)

	go h.backup(ctx)
	h.wg.Add(1)

	h.wg.Wait()
	h.shutdown()
}

// FetchClients returns all connected pool clients.
func (h *Hub) FetchClients() []*Client {
	clients := make([]*Client, 0)
	for _, endpoint := range h.endpoints {
		endpoint.clientsMtx.Lock()
		for _, c := range endpoint.clients {
			clients = append(clients, c)
		}
		endpoint.clientsMtx.Unlock()
	}
	return clients
}

// FetchPendingPayments fetches all unpaid payments.
func (h *Hub) FetchPendingPayments() ([]*Payment, error) {
	return h.paymentMgr.pendingPayments()
}

// FetchArchivedPayments fetches all paid payments.
func (h *Hub) FetchArchivedPayments() ([]*Payment, error) {
	return h.paymentMgr.archivedPayments()
}

// FetchMinedWork returns work data associated with all blocks mined by the pool
// regardless of whether they are confirmed or not.
//
// List is ordered, most recent comes first.
func (h *Hub) FetchMinedWork() ([]*AcceptedWork, error) {
	return ListMinedWork(h.db)
}

// Quota details the portion of mining rewrds due an account for work
// contributed to the pool.
type Quota struct {
	AccountID  string
	Percentage *big.Rat
}

// FetchWorkQuotas returns the reward distribution to pool accounts
// based on work contributed per the payment scheme used by the pool.
func (h *Hub) FetchWorkQuotas() ([]*Quota, error) {
	if h.cfg.SoloPool {
		return nil, nil
	}
	var percentages map[string]*big.Rat
	var err error
	if h.cfg.PaymentMethod == PPS {
		percentages, err = h.paymentMgr.PPSSharePercentages(time.Now().UnixNano())
	}
	if h.cfg.PaymentMethod == PPLNS {
		percentages, err = h.paymentMgr.PPLNSSharePercentages()
	}
	if err != nil {
		return nil, err
	}

	quotas := make([]*Quota, 0)
	for key, value := range percentages {
		quotas = append(quotas, &Quota{
			AccountID:  key,
			Percentage: value,
		})
	}
	return quotas, nil
}

// AccountExists checks if the provided account id references a pool account.
func (h *Hub) AccountExists(accountID string) bool {
	_, err := FetchAccount(h.db, []byte(accountID))
	if err != nil {
		log.Tracef("Unable to fetch account for id: %s", accountID)
		return false
	}
	return true
}

// CSRFSecret fetches a persisted secret or generates a new one.
func (h *Hub) CSRFSecret() ([]byte, error) {
	var secret []byte
	err := h.db.Update(func(tx *bolt.Tx) error {
		pbkt := tx.Bucket(poolBkt)
		if pbkt == nil {
			desc := fmt.Sprintf("bucket %s not found", string(poolBkt))
			return MakeError(ErrBucketNotFound, desc, nil)
		}
		v := pbkt.Get(csrfSecret)
		if v != nil {
			secret = make([]byte, len(v))
			copy(secret, v)
			return nil
		}

		var err error
		secret = make([]byte, 32)
		_, err = rand.Read(secret)
		if err != nil {
			return err
		}
		err = pbkt.Put(csrfSecret, secret)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return secret, nil
}

// BackupDB streams a backup of the database over an http response.
func (h *Hub) BackupDB(w http.ResponseWriter) error {
	err := h.db.View(func(tx *bolt.Tx) error {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="backup.db"`)
		w.Header().Set("Content-Length", strconv.Itoa(int(tx.Size())))
		_, err := tx.WriteTo(w)
		return err
	})
	return err
}
