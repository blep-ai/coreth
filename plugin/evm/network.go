// (c) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package evm

import (
	"time"

	"github.com/ava-labs/avalanchego/cache"
	"github.com/ava-labs/avalanchego/ids"

	commonEng "github.com/ava-labs/avalanchego/snow/engine/common"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/ava-labs/coreth/core"
	"github.com/ava-labs/coreth/core/types"
	"github.com/ava-labs/coreth/plugin/evm/message"

	coreth "github.com/ava-labs/coreth/chain"
)

const (
	// We allow [recentCacheSize] to be fairly large because we only store hashes
	// in the cache, not entire transactions.
	recentCacheSize = 512
)

type Network interface {
	// Message handling
	AppRequestFailed(nodeID ids.ShortID, requestID uint32) error
	AppRequest(nodeID ids.ShortID, requestID uint32, msgBytes []byte) error
	AppResponse(nodeID ids.ShortID, requestID uint32, msgBytes []byte) error
	AppGossip(nodeID ids.ShortID, msgBytes []byte) error

	// Gossip entrypoints
	GossipAtomicTx(tx *Tx) error
	GossipEthTxs(txs []*types.Transaction) error
}

func (vm *VM) AppRequest(nodeID ids.ShortID, requestID uint32, request []byte) error {
	return vm.network.AppRequest(nodeID, requestID, request)
}

func (vm *VM) AppResponse(nodeID ids.ShortID, requestID uint32, response []byte) error {
	return vm.network.AppResponse(nodeID, requestID, response)
}

func (vm *VM) AppRequestFailed(nodeID ids.ShortID, requestID uint32) error {
	return vm.network.AppRequestFailed(nodeID, requestID)
}

func (vm *VM) AppGossip(nodeID ids.ShortID, msg []byte) error {
	return vm.network.AppGossip(nodeID, msg)
}

// NewNetwork creates a new Network based on the [vm.chainConfig].
func (vm *VM) NewNetwork(appSender commonEng.AppSender) Network {
	if vm.chainConfig.ApricotPhase4BlockTimestamp != nil {
		return vm.newPushNetwork(
			time.Unix(vm.chainConfig.ApricotPhase4BlockTimestamp.Int64(), 0),
			appSender,
			vm.chain,
			vm.mempool,
		)
	}

	return &noopNetwork{}
}

type pushNetwork struct {
	gossipActivationTime time.Time

	appSender commonEng.AppSender
	chain     *coreth.ETHChain
	mempool   *Mempool

	gossipHandler message.Handler

	// [recentAtomicTxs] and [recentEthTxs] prevent us from over-gossiping the
	// same transaction in a short period of time.
	recentAtomicTxs *cache.LRU
	recentEthTxs    *cache.LRU
}

func (vm *VM) newPushNetwork(
	activationTime time.Time,
	appSender commonEng.AppSender,
	chain *coreth.ETHChain,
	mempool *Mempool,
) Network {
	net := &pushNetwork{
		gossipActivationTime: activationTime,
		appSender:            appSender,
		chain:                chain,
		mempool:              mempool,
		recentAtomicTxs:      &cache.LRU{Size: recentCacheSize},
		recentEthTxs:         &cache.LRU{Size: recentCacheSize},
	}
	net.gossipHandler = &GossipHandler{
		vm:  vm,
		net: net,
	}
	return net
}

func (n *pushNetwork) AppRequestFailed(nodeID ids.ShortID, requestID uint32) error {
	return nil
}

func (n *pushNetwork) AppRequest(nodeID ids.ShortID, requestID uint32, msgBytes []byte) error {
	return nil
}

func (n *pushNetwork) AppResponse(nodeID ids.ShortID, requestID uint32, msgBytes []byte) error {
	return nil
}

func (n *pushNetwork) AppGossip(nodeID ids.ShortID, msgBytes []byte) error {
	return n.handle(
		n.gossipHandler,
		"Gossip",
		nodeID,
		0,
		msgBytes,
	)
}

func (n *pushNetwork) GossipAtomicTx(tx *Tx) error {
	txID := tx.ID()
	if time.Now().Before(n.gossipActivationTime) {
		log.Debug(
			"not gossiping atomic tx before the gossiping activation time",
			"txID", txID,
		)
		return nil
	}

	// Don't gossip transaction if it has been recently gossiped.
	if _, has := n.recentAtomicTxs.Get(txID); has {
		return nil
	}
	n.recentAtomicTxs.Put(txID, nil)

	msg := message.AtomicTx{
		Tx: tx.Bytes(),
	}
	msgBytes, err := message.Build(&msg)
	if err != nil {
		return err
	}

	log.Debug(
		"gossiping atomic tx",
		"txID", txID,
	)
	return n.appSender.SendAppGossip(msgBytes)
}

func (n *pushNetwork) sendEthTxs(txs []*types.Transaction) error {
	if len(txs) == 0 {
		return nil
	}

	txBytes, err := rlp.EncodeToBytes(txs)
	if err != nil {
		log.Warn(
			"failed to encode eth transactions",
			"len(txs)", len(txs),
			"err", err,
		)
		return nil
	}
	msg := message.EthTxs{
		Txs: txBytes,
	}
	msgBytes, err := message.Build(&msg)
	if err != nil {
		return err
	}

	log.Debug(
		"gossiping eth txs",
		"len(txs)", len(txs),
		"size(txs)", len(msg.Txs),
	)
	return n.appSender.SendAppGossip(msgBytes)
}

// GossipEthTxs gossips the provided [txs] as soon as possible to reduce the
// time to finality. In the future, we could attempt to be more conservative
// with the number of messages we send and attempt to periodically send
// a batch of messages.
func (n *pushNetwork) GossipEthTxs(txs []*types.Transaction) error {
	if time.Now().Before(n.gossipActivationTime) {
		log.Debug(
			"not gossiping eth txs before the gossiping activation time",
			"len(txs)", len(txs),
		)
		return nil
	}

	pool := n.chain.GetTxPool()
	selectedTxs := make([]*types.Transaction, 0)
	for _, tx := range txs {
		txHash := tx.Hash()
		txStatus := pool.Status([]common.Hash{txHash})[0]
		if txStatus != core.TxStatusPending {
			continue
		}

		if _, has := n.recentEthTxs.Get(txHash); has {
			continue
		}
		n.recentEthTxs.Put(txHash, nil)

		selectedTxs = append(selectedTxs, tx)
	}

	if len(selectedTxs) == 0 {
		return nil
	}

	// Attempt to gossip [selectedTxs]
	msgTxs := make([]*types.Transaction, 0)
	msgTxsSize := common.StorageSize(0)
	for _, tx := range selectedTxs {
		size := tx.Size()
		if msgTxsSize+size > message.EthMsgSoftCapSize {
			if err := n.sendEthTxs(msgTxs); err != nil {
				return err
			}
			msgTxs = msgTxs[:0]
			msgTxsSize = 0
		}
		msgTxs = append(msgTxs, tx)
		msgTxsSize += size
	}

	// Send any remaining [msgTxs]
	return n.sendEthTxs(msgTxs)
}

func (n *pushNetwork) handle(
	handler message.Handler,
	handlerName string,
	nodeID ids.ShortID,
	requestID uint32,
	msgBytes []byte,
) error {
	log.Debug(
		"App message handler called",
		"handler", handlerName,
		"peerID", nodeID,
		"requestID", requestID,
		"len(msg)", len(msgBytes),
	)

	if time.Now().Before(n.gossipActivationTime) {
		log.Debug("App message called before activation time")
		return nil
	}

	msg, err := message.Parse(msgBytes)
	if err != nil {
		log.Debug("dropping App message due to failing to parse message")
		return nil
	}

	return msg.Handle(handler, nodeID, requestID)
}

type GossipHandler struct {
	message.NoopHandler

	vm  *VM
	net *pushNetwork
}

func (h *GossipHandler) HandleAtomicTx(nodeID ids.ShortID, _ uint32, msg *message.AtomicTx) error {
	log.Debug(
		"AppGossip called with AtomicTx",
		"peerID", nodeID,
	)

	if len(msg.Tx) == 0 {
		log.Debug(
			"AppGossip received empty AtomicTx Message",
			"peerID", nodeID,
		)
		return nil
	}

	// In the case that the gossip message contains a transaction,
	// attempt to parse it and add it as a remote.
	tx := Tx{}
	if _, err := Codec.Unmarshal(msg.Tx, &tx); err != nil {
		log.Trace(
			"AppGossip provided invalid tx",
			"err", err,
		)
		return nil
	}
	unsignedBytes, err := Codec.Marshal(codecVersion, &tx.UnsignedAtomicTx)
	if err != nil {
		log.Warn(
			"AppGossip failed to marshal unsigned tx",
			"err", err,
		)
		return nil
	}
	tx.Initialize(unsignedBytes, msg.Tx)

	txID := tx.ID()
	if _, dropped, found := h.net.mempool.GetTx(txID); found || dropped {
		return nil
	}

	if err := h.vm.issueTx(&tx, false /*=local*/); err != nil {
		log.Trace(
			"AppGossip provided invalid transaction",
			"peerID", nodeID,
			"err", err,
		)
	}

	return nil
}

func (h *GossipHandler) HandleEthTxs(nodeID ids.ShortID, _ uint32, msg *message.EthTxs) error {
	log.Debug(
		"AppGossip called with EthTxs",
		"peerID", nodeID,
		"size(txs)", len(msg.Txs),
	)

	if len(msg.Txs) == 0 {
		log.Debug(
			"AppGossip received empty EthTxs Message",
			"peerID", nodeID,
		)
		return nil
	}

	// The maximum size of this encoded object is enforced by the codec.
	txs := make([]*types.Transaction, 0)
	if err := rlp.DecodeBytes(msg.Txs, &txs); err != nil {
		log.Trace(
			"AppGossip provided invalid txs",
			"peerID", nodeID,
			"err", err,
		)
		return nil
	}
	errs := h.net.chain.GetTxPool().AddRemotes(txs)
	for i, err := range errs {
		if err != nil {
			log.Debug(
				"AppGossip failed to add to mempool",
				"err", err,
				"tx", txs[i].Hash(),
			)
		}
	}
	return nil
}

// noopNetwork should be used when gossip communication is not supported
type noopNetwork struct{}

func (n *noopNetwork) AppRequestFailed(nodeID ids.ShortID, requestID uint32) error {
	return nil
}
func (n *noopNetwork) AppRequest(nodeID ids.ShortID, requestID uint32, msgBytes []byte) error {
	return nil
}
func (n *noopNetwork) AppResponse(nodeID ids.ShortID, requestID uint32, msgBytes []byte) error {
	return nil
}
func (n *noopNetwork) AppGossip(nodeID ids.ShortID, msgBytes []byte) error {
	return nil
}
func (n *noopNetwork) GossipAtomicTx(tx *Tx) error {
	return nil
}
func (n *noopNetwork) GossipEthTxs(txs []*types.Transaction) error {
	return nil
}
