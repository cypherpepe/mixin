package kernel

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MixinNetwork/mixin/common"
	"github.com/MixinNetwork/mixin/config"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/mixin/kernel/internal/clock"
	"github.com/MixinNetwork/mixin/logger"
	"github.com/MixinNetwork/mixin/network"
	"github.com/MixinNetwork/mixin/p2p"
	"github.com/MixinNetwork/mixin/storage"
	"github.com/dgraph-io/ristretto"
)

type Node struct {
	IdForNetwork crypto.Hash
	Signer       common.Address
	isRelayer    bool

	Listener            string
	LegacyPeer          *network.Peer
	LegacySyncPoints    *legacySyncMap
	LegacySyncPointsMap map[crypto.Hash]*network.SyncPoint

	Peer          *p2p.Peer
	TopoCounter   *TopologicalSequence
	SyncPoints    *syncMap
	SyncPointsMap map[crypto.Hash]*p2p.SyncPoint

	GraphTimestamp uint64
	Epoch          uint64
	LastMint       uint64

	chains                     *chainsMap
	allNodesSortedWithState    []*CNode
	nodeStateSequences         []*NodeStateSequence
	acceptedNodeStateSequences []*NodeStateSequence
	chain                      *Chain

	genesisNodesMap map[crypto.Hash]bool
	genesisNodes    []crypto.Hash
	startAt         time.Time
	networkId       crypto.Hash
	persistStore    storage.Store
	cacheStore      *ristretto.Cache
	custom          *config.Custom
	configDir       string
	addr            string

	done chan struct{}
	elc  chan struct{}
	mlc  chan struct{}
	cqc  chan struct{}
}

type NodeStateSequence struct {
	Timestamp         uint64
	NodesWithoutState []*CNode
}

type CNode struct {
	IdForNetwork   crypto.Hash
	Signer         common.Address
	Payee          common.Address
	Transaction    crypto.Hash
	Timestamp      uint64
	State          string
	ConsensusIndex int
}

func SetupNode(custom *config.Custom, persistStore storage.Store, cacheStore *ristretto.Cache, addr, dir string) (*Node, error) {
	var node = &Node{
		SyncPoints:       &syncMap{mutex: new(sync.RWMutex), m: make(map[crypto.Hash]*p2p.SyncPoint)},
		LegacySyncPoints: &legacySyncMap{mutex: new(sync.RWMutex), m: make(map[crypto.Hash]*network.SyncPoint)},
		chains:           &chainsMap{m: make(map[crypto.Hash]*Chain)},
		genesisNodesMap:  make(map[crypto.Hash]bool),
		persistStore:     persistStore,
		cacheStore:       cacheStore,
		custom:           custom,
		configDir:        dir,
		addr:             addr,
		startAt:          clock.Now(),
		done:             make(chan struct{}),
		elc:              make(chan struct{}),
		mlc:              make(chan struct{}),
		cqc:              make(chan struct{}),
	}

	node.loadNodeConfig()

	mint := node.lastMintDistribution()
	node.LastMint = mint.Batch

	gns, err := common.ReadGenesis(dir + "/genesis.json")
	if err != nil {
		return nil, fmt.Errorf("ReadGenesis(%s) => %v", dir, err)
	}
	err = node.LoadGenesis(gns)
	if err != nil {
		return nil, fmt.Errorf("LoadGenesis(%s) => %v", dir, err)
	}
	node.TopoCounter = node.getTopologyCounter(persistStore)

	logger.Println("Validating graph entries...")
	start := clock.Now()
	total, invalid, err := node.persistStore.ValidateGraphEntries(node.networkId, 10)
	if err != nil {
		return nil, fmt.Errorf("ValidateGraphEntries(%s) => %v", node.networkId, err)
	} else if invalid > 0 {
		return nil, fmt.Errorf("validate graph with %d/%d invalid entries", invalid, total)
	}
	logger.Printf("Validate graph with %d total entries in %s\n", total, clock.Now().Sub(start).String())

	err = node.LoadConsensusNodes()
	if err != nil {
		return nil, fmt.Errorf("LoadConsensusNodes() => %v", err)
	}

	err = node.LoadAllChainsAndGraphTimestamp(node.persistStore, node.networkId)
	if err != nil {
		return nil, fmt.Errorf("LoadAllChainsAndGraphTimestamp() => %v", err)
	}
	node.chain = node.BootChain(node.IdForNetwork)

	logger.Printf("Listen:\t%s\n", addr)
	logger.Printf("Signer:\t%s\n", node.Signer.String())
	logger.Printf("Network:\t%s\n", node.networkId.String())
	logger.Printf("Node Id:\t%s\n", node.IdForNetwork.String())
	logger.Printf("Topology:\t%d\n", node.TopoCounter.seq)
	return node, nil
}

func (node *Node) loadNodeConfig() {
	var addr common.Address
	addr.PrivateSpendKey = node.custom.Node.Signer
	addr.PublicSpendKey = addr.PrivateSpendKey.Public()
	addr.PrivateViewKey = addr.PublicSpendKey.DeterministicHashDerive()
	addr.PublicViewKey = addr.PrivateViewKey.Public()
	node.Signer = addr
	node.isRelayer = node.custom.P2P.Relayer
	node.Listener = node.custom.LegacyNetwork.Listener
}

func (node *Node) buildNodeStateSequences(allNodesSortedWithState []*CNode, acceptedOnly bool) []*NodeStateSequence {
	nodeStateSequences := make([]*NodeStateSequence, len(allNodesSortedWithState))
	for i, n := range allNodesSortedWithState {
		nodes := node.nodeSequenceWithoutState(n.Timestamp+1, acceptedOnly)
		seq := &NodeStateSequence{
			Timestamp:         n.Timestamp,
			NodesWithoutState: nodes,
		}
		nodeStateSequences[i] = seq
	}
	return nodeStateSequences
}

func (node *Node) NodesListWithoutState(threshold uint64, acceptedOnly bool) []*CNode {
	sequences := node.nodeStateSequences
	if acceptedOnly {
		sequences = node.acceptedNodeStateSequences
	}
	for i := len(sequences); i > 0; i-- {
		seq := sequences[i-1]
		if seq.Timestamp < threshold {
			return seq.NodesWithoutState
		}
	}
	return nil
}

func (node *Node) nodeSequenceWithoutState(threshold uint64, acceptedOnly bool) []*CNode {
	filter := make(map[crypto.Hash]*CNode)
	for _, n := range node.allNodesSortedWithState {
		if n.Timestamp >= threshold {
			break
		}
		filter[n.IdForNetwork] = n
	}
	nodes := make([]*CNode, 0)
	for _, n := range filter {
		if !acceptedOnly || n.State == common.NodeStateAccepted {
			nodes = append(nodes, &CNode{
				IdForNetwork: n.IdForNetwork,
				Signer:       n.Signer,
				Payee:        n.Payee,
				Transaction:  n.Transaction,
				Timestamp:    n.Timestamp,
				State:        n.State,
			})
		}
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Timestamp < nodes[j].Timestamp {
			return true
		}
		if nodes[i].Timestamp > nodes[j].Timestamp {
			return false
		}
		a := nodes[i].IdForNetwork
		b := nodes[j].IdForNetwork
		return a.String() < b.String()
	})
	for index, i := 0, 0; i < len(nodes); i++ {
		cn := nodes[i]
		cn.ConsensusIndex = index
		switch cn.State {
		case common.NodeStateAccepted, common.NodeStatePledging:
			index++
		}
	}
	return nodes
}

func (node *Node) PledgingNode(timestamp uint64) *CNode {
	nodes := node.NodesListWithoutState(timestamp, false)
	if len(nodes) == 0 {
		return nil
	}
	cn := nodes[len(nodes)-1]
	if cn.State == common.NodeStatePledging {
		return cn
	}
	return nil
}

func (node *Node) GetAcceptedOrPledgingNode(id crypto.Hash) *CNode {
	nodes := node.NodesListWithoutState(uint64(clock.Now().UnixNano()), false)
	for _, cn := range nodes {
		if cn.IdForNetwork == id && (cn.State == common.NodeStateAccepted || cn.State == common.NodeStatePledging) {
			return cn
		}
	}
	return nil
}

func (node *Node) GetRemovedOrCancelledNode(id crypto.Hash, timestamp uint64) *CNode {
	nodes := node.NodesListWithoutState(timestamp, false)
	for _, cn := range nodes {
		if cn.IdForNetwork == id && (cn.State == common.NodeStateRemoved || cn.State == common.NodeStateCancelled) {
			return cn
		}
	}
	return nil
}

// An accepted node can sign transactions only when it satisfies either:
// 1. It is a genesis node.
// 2. It has been accepted more than 12 hours.
func (node *Node) ConsensusReady(cn *CNode, timestamp uint64) bool {
	if cn.State != common.NodeStateAccepted {
		return false
	}
	if node.genesisNodesMap[cn.IdForNetwork] {
		return true
	}
	if cn.Timestamp+uint64(config.KernelNodeAcceptPeriodMinimum) < timestamp {
		return true
	}
	return false
}

func (node *Node) ConsensusThreshold(timestamp uint64, final bool) int {
	consensusBase := 0
	nodes := node.NodesListWithoutState(timestamp, false)
	for _, cn := range nodes {
		threshold := config.SnapshotReferenceThreshold * config.SnapshotRoundGap
		if threshold > uint64(3*time.Minute) {
			panic("should never be here")
		}
		switch cn.State {
		case common.NodeStatePledging:
			// FIXME the pledge transaction may be broadcasted very late
			// at this situation, the node should be treated as evil
			if config.KernelNodeAcceptPeriodMinimum < time.Hour {
				panic("should never be here")
			}
			t := uint64(config.KernelNodeAcceptPeriodMinimum) - threshold*3
			if !final && cn.Timestamp+t < timestamp {
				consensusBase++
			}
		case common.NodeStateAccepted:
			if node.genesisNodesMap[cn.IdForNetwork] || cn.Timestamp+threshold < timestamp {
				consensusBase++
			}
		}
	}
	if consensusBase < config.KernelMinimumNodesCount {
		logger.Debugf("invalid consensus base %d %d %d\n", timestamp, consensusBase, config.KernelMinimumNodesCount)
		return 1000
	}
	return consensusBase*2/3 + 1
}

func (node *Node) LoadConsensusNodes() error {
	threshold := uint64(clock.Now().UnixNano()) * 2
	nodes := node.persistStore.ReadAllNodes(threshold, true)
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Timestamp < nodes[j].Timestamp {
			return true
		}
		if nodes[i].Timestamp > nodes[j].Timestamp {
			return false
		}
		a := nodes[i].IdForNetwork(node.networkId)
		b := nodes[j].IdForNetwork(node.networkId)
		return a.String() < b.String()
	})
	cnodes := make([]*CNode, len(nodes))
	for i, n := range nodes {
		cnodes[i] = &CNode{
			IdForNetwork: n.IdForNetwork(node.networkId),
			Signer:       n.Signer,
			Payee:        n.Payee,
			Transaction:  n.Transaction,
			Timestamp:    n.Timestamp,
			State:        n.State,
		}
		logger.Printf("LoadConsensusNode %v\n", cnodes[i])
	}
	node.allNodesSortedWithState = cnodes
	node.nodeStateSequences = node.buildNodeStateSequences(cnodes, false)
	node.acceptedNodeStateSequences = node.buildNodeStateSequences(cnodes, true)
	return nil
}

func (node *Node) SnapshotVersion() uint8 {
	return common.SnapshotVersionCommonEncoding
}

// this is needed to handle mainnet transaction version upgrading fork
func (node *Node) NewTransaction(assetId crypto.Hash) *common.Transaction {
	return common.NewTransactionV5(assetId)
}

func (node *Node) addRelayersFromConfig() error {
	node.Peer = p2p.NewPeer(node, node.IdForNetwork, node.addr, node.isRelayer)

	for _, s := range node.custom.P2P.Seeds {
		parts := strings.Split(s, "@")
		if len(parts) != 2 {
			return fmt.Errorf("invalid peer %s", s)
		}
		nid, err := crypto.HashFromString(parts[0])
		if err != nil {
			return fmt.Errorf("invalid peer id %s", s)
		}
		if nid == node.IdForNetwork {
			continue
		}
		go node.Peer.ConnectRelayer(nid, parts[1])
	}
	return nil
}

func (node *Node) listenConsumers() {
	if !node.isRelayer {
		return
	}
	err := node.Peer.ListenConsumers()
	if err != nil {
		panic(err)
	}
}

func (node *Node) BuildAuthenticationMessage(relayerId crypto.Hash) []byte {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, uint64(clock.Now().Unix()))
	data = append(data, relayerId[:]...)
	data = append(data, node.Signer.PublicSpendKey[:]...)
	if node.isRelayer {
		data = append(data, 1)
	} else {
		data = append(data, 0)
	}
	dh := crypto.Blake3Hash(data)
	sig := node.Signer.PrivateSpendKey.Sign(dh)
	data = append(data, sig[:]...)
	return data
}

func (node *Node) AuthenticateAs(recipientId crypto.Hash, msg []byte, timeoutSec int64) (*p2p.AuthToken, error) {
	if len(msg) != 137 {
		return nil, fmt.Errorf("peer authentication message malformatted %d", len(msg))
	}
	ts := binary.BigEndian.Uint64(msg[:8])
	if timeoutSec > 0 && math.Abs(float64(clock.Now().Unix())-float64(ts)) > float64(timeoutSec) {
		return nil, fmt.Errorf("peer authentication message timeout %d %d", ts, clock.Now().Unix())
	}

	var relayerId crypto.Hash
	copy(relayerId[:], msg[8:40])
	if relayerId != recipientId {
		return nil, fmt.Errorf("peer authentication is not for me %s", relayerId)
	}

	var signer common.Address
	copy(signer.PublicSpendKey[:], msg[40:72])
	signer.PublicViewKey = signer.PublicSpendKey.DeterministicHashDerive().Public()
	peerId := signer.Hash().ForNetwork(node.networkId)
	if peerId == recipientId {
		return nil, fmt.Errorf("peer is self %s", peerId)
	}

	var sig crypto.Signature
	copy(sig[:], msg[73:137])
	mh := crypto.Blake3Hash(msg[:73])
	if !signer.PublicSpendKey.Verify(mh, sig) {
		return nil, fmt.Errorf("peer authentication message signature invalid %s", peerId)
	}
	token := &p2p.AuthToken{
		PeerId:    peerId,
		Timestamp: ts,
		IsRelayer: msg[72] == byte(1),
		Data:      bytes.Clone(msg),
	}
	return token, nil
}

func (node *Node) PingNeighborsFromConfig() error {
	node.LegacyPeer = network.NewPeer(node, node.IdForNetwork, node.addr, true, true)

	for _, s := range node.custom.LegacyNetwork.Peers {
		if s == node.Listener {
			continue
		}
		node.LegacyPeer.PingNeighbor(s)
	}
	return nil
}

func (node *Node) UpdateNeighbors(neighbors []string) error {
	for _, in := range neighbors {
		if in == node.Listener {
			continue
		}
		node.LegacyPeer.PingNeighbor(in)
	}
	return nil
}

func (node *Node) ListenNeighbors() {
	if node.custom.LegacyNetwork.Listener != "" {
		err := node.LegacyPeer.ListenNeighbors()
		if err != nil {
			panic(err)
		}
	}
}

func (node *Node) NetworkId() crypto.Hash {
	return node.networkId
}

func (node *Node) Uptime() time.Duration {
	return clock.Now().Sub(node.startAt)
}

func (node *Node) GetCacheStore() *ristretto.Cache {
	return node.cacheStore
}

func (node *Node) BuildLegacyGraph() []*network.SyncPoint {
	node.chains.RLock()
	defer node.chains.RUnlock()

	points := make([]*network.SyncPoint, 0)
	for _, chain := range node.chains.m {
		if chain.State == nil {
			continue
		}
		f := chain.State.FinalRound
		points = append(points, &network.SyncPoint{
			NodeId: chain.ChainId,
			Hash:   f.Hash,
			Number: f.Number,
			Pool: map[string]int{
				"index": chain.FinalIndex,
				"count": chain.FinalCount,
			},
		})
	}
	return points
}

func (node *Node) BuildGraph() []*p2p.SyncPoint {
	node.chains.RLock()
	defer node.chains.RUnlock()

	points := make([]*p2p.SyncPoint, 0)
	for _, chain := range node.chains.m {
		if chain.State == nil {
			continue
		}
		f := chain.State.FinalRound
		points = append(points, &p2p.SyncPoint{
			NodeId: chain.ChainId,
			Hash:   f.Hash,
			Number: f.Number,
			Pool: map[string]int{
				"index": chain.FinalIndex,
				"count": chain.FinalCount,
			},
		})
	}
	return points
}

func (node *Node) BuildLegacyAuthenticationMessage() []byte {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, uint64(clock.Now().Unix()))
	data = append(data, node.Signer.PublicSpendKey[:]...)
	dh := crypto.Blake3Hash(data)
	sig := node.Signer.PrivateSpendKey.Sign(dh)
	data = append(data, sig[:]...)
	return append(data, []byte(node.Listener)...)
}

func (node *Node) Authenticate(msg []byte) (crypto.Hash, string, error) {
	if len(msg) < 8+len(crypto.Hash{})+len(crypto.Signature{}) {
		return crypto.Hash{}, "", fmt.Errorf("peer authentication message malformated %d", len(msg))
	}
	ts := binary.BigEndian.Uint64(msg[:8])
	if clock.Now().Unix()-int64(ts) > 3 {
		return crypto.Hash{}, "", fmt.Errorf("peer authentication message timeout %d %d", ts, clock.Now().Unix())
	}

	var signer common.Address
	copy(signer.PublicSpendKey[:], msg[8:40])
	signer.PublicViewKey = signer.PublicSpendKey.DeterministicHashDerive().Public()
	peerId := signer.Hash().ForNetwork(node.networkId)
	if peerId == node.IdForNetwork {
		return crypto.Hash{}, "", fmt.Errorf("peer authentication invalid consensus peer %s", peerId)
	}
	peer := node.GetAcceptedOrPledgingNode(peerId)

	if peer != nil && peer.Signer.Hash() != signer.Hash() {
		return crypto.Hash{}, "", fmt.Errorf("peer authentication invalid consensus peer %s", peerId)
	}

	var sig crypto.Signature
	copy(sig[:], msg[40:40+len(sig)])
	mh := crypto.Blake3Hash(msg[:40])
	if !signer.PublicSpendKey.Verify(mh, sig) {
		return crypto.Hash{}, "", fmt.Errorf("peer authentication message signature invalid %s", peerId)
	}

	listener := string(msg[40+len(sig):])
	return peerId, listener, nil
}

func (node *Node) SendTransactionToPeer(peerId, hash crypto.Hash) error {
	tx, _, err := node.checkTxInStorage(hash)
	if err != nil || tx == nil {
		return err
	}
	return node.Peer.SendTransactionMessage(peerId, tx)
}

func (node *Node) CachePutTransaction(peerId crypto.Hash, tx *common.VersionedTransaction) error {
	return node.persistStore.CachePutTransaction(tx)
}

func (node *Node) ReadAllNodesWithoutState() []crypto.Hash {
	var all []crypto.Hash
	nodes := node.NodesListWithoutState(uint64(clock.Now().UnixNano()), false)
	for _, cn := range nodes {
		all = append(all, cn.IdForNetwork)
	}
	return all
}

func (node *Node) ReadSnapshotsSinceTopology(offset, count uint64) ([]*common.SnapshotWithTopologicalOrder, error) {
	return node.persistStore.ReadSnapshotsSinceTopology(offset, count)
}

func (node *Node) ReadSnapshotsForNodeRound(nodeIdWithNetwork crypto.Hash, round uint64) ([]*common.SnapshotWithTopologicalOrder, error) {
	return node.persistStore.ReadSnapshotsForNodeRound(nodeIdWithNetwork, round)
}

func (node *Node) sendGraphToConcensusNodes() {
	graphTicker := time.NewTicker(time.Duration(config.SnapshotRoundGap / 2))
	defer graphTicker.Stop()

	for {
		nodes := node.NodesListWithoutState(uint64(clock.Now().UnixNano()), true)
		for _, cn := range nodes {
			node.Peer.SendGraphMessage(cn.IdForNetwork)
		}
		<-graphTicker.C
	}
}

func (node *Node) UpdateSyncPoint(peerId crypto.Hash, points []*p2p.SyncPoint) {
	for _, p := range points {
		if p.NodeId == node.IdForNetwork {
			node.SyncPoints.Set(peerId, p)
		}
	}
	node.SyncPointsMap = node.SyncPoints.Map()
}

func (node *Node) UpdateLegacySyncPoint(peerId crypto.Hash, points []*network.SyncPoint) {
	for _, p := range points {
		if p.NodeId == node.IdForNetwork {
			node.LegacySyncPoints.Set(peerId, p)
		}
	}
	node.LegacySyncPointsMap = node.LegacySyncPoints.Map()
}

func (node *Node) CheckBroadcastedToPeers() bool {
	return node.CheckBroadcastedToP2PPeers() || node.CheckBroadcastedToLegacyPeers()
}

func (node *Node) CheckBroadcastedToP2PPeers() bool {
	spm := node.SyncPointsMap
	if len(spm) == 0 || node.chain.State == nil {
		return false
	}

	final, count := node.chain.State.FinalRound.Number, 1
	threshold := node.ConsensusThreshold(uint64(clock.Now().UnixNano()), false)
	nodes := node.NodesListWithoutState(uint64(clock.Now().UnixNano()), true)
	for _, cn := range nodes {
		remote := spm[cn.IdForNetwork]
		if remote == nil {
			continue
		}
		if remote.Number+1 >= final {
			count += 1
		}
	}
	return count >= threshold
}

func (node *Node) CheckBroadcastedToLegacyPeers() bool {
	spm := node.LegacySyncPointsMap
	if len(spm) == 0 || node.chain.State == nil {
		return false
	}

	final, count := node.chain.State.FinalRound.Number, 1
	threshold := node.ConsensusThreshold(uint64(clock.Now().UnixNano()), false)
	nodes := node.NodesListWithoutState(uint64(clock.Now().UnixNano()), true)
	for _, cn := range nodes {
		remote := spm[cn.IdForNetwork]
		if remote == nil {
			continue
		}
		if remote.Number+1 >= final {
			count += 1
		}
	}
	return count >= threshold
}

func (node *Node) CheckCatchUpWithPeers() bool {
	return node.CheckCatchUpWithP2PPeers() || node.CheckCatchUpWithLegacyPeers()
}

func (node *Node) CheckCatchUpWithP2PPeers() bool {
	spm := node.SyncPointsMap
	if len(spm) == 0 || node.chain.State == nil {
		return false
	}

	threshold := node.ConsensusThreshold(uint64(clock.Now().UnixNano()), false)
	cache, updated := node.chain.State.CacheRound, 1
	final := node.chain.State.FinalRound.Number

	nodes := node.NodesListWithoutState(uint64(clock.Now().UnixNano()), true)
	for _, cn := range nodes {
		remote := spm[cn.IdForNetwork]
		if remote == nil {
			continue
		}
		updated = updated + 1
		if remote.Number <= final {
			continue
		}
		if remote.Number > final+1 {
			logger.Verbosef("CheckCatchUpWithPeers local(%d)+1 < remote(%s:%d)\n", final, cn.IdForNetwork, remote.Number)
			return false
		}
		if cache == nil {
			logger.Verbosef("CheckCatchUpWithPeers local cache nil\n")
			return false
		}
		cf := cache.asFinal()
		if cf == nil {
			logger.Verbosef("CheckCatchUpWithPeers local cache empty\n")
			return false
		}
		if cf.Hash != remote.Hash {
			logger.Verbosef("CheckCatchUpWithPeers local(%s) != remote(%s)\n",
				cf.Hash, remote.Hash)
			return false
		}
		if now := uint64(clock.Now().UnixNano()); cf.Start+config.SnapshotRoundGap*100 > now {
			logger.Verbosef("CheckCatchUpWithPeers local start(%d)+%d > now(%d)\n",
				cf.Start, config.SnapshotRoundGap*100, now)
			return false
		}
	}

	if updated < threshold {
		logger.Verbosef("CheckCatchUpWithPeers updated(%d) < threshold(%d)\n", updated, threshold)
	}
	return updated >= threshold
}

func (node *Node) CheckCatchUpWithLegacyPeers() bool {
	spm := node.LegacySyncPointsMap
	if len(spm) == 0 || node.chain.State == nil {
		return false
	}

	threshold := node.ConsensusThreshold(uint64(clock.Now().UnixNano()), false)
	cache, updated := node.chain.State.CacheRound, 1
	final := node.chain.State.FinalRound.Number

	nodes := node.NodesListWithoutState(uint64(clock.Now().UnixNano()), true)
	for _, cn := range nodes {
		remote := spm[cn.IdForNetwork]
		if remote == nil {
			continue
		}
		updated = updated + 1
		if remote.Number <= final {
			continue
		}
		if remote.Number > final+1 {
			logger.Verbosef("CheckCatchUpWithPeers local(%d)+1 < remote(%s:%d)\n", final, cn.IdForNetwork, remote.Number)
			return false
		}
		if cache == nil {
			logger.Verbosef("CheckCatchUpWithPeers local cache nil\n")
			return false
		}
		cf := cache.asFinal()
		if cf == nil {
			logger.Verbosef("CheckCatchUpWithPeers local cache empty\n")
			return false
		}
		if cf.Hash != remote.Hash {
			logger.Verbosef("CheckCatchUpWithPeers local(%s) != remote(%s)\n",
				cf.Hash, remote.Hash)
			return false
		}
		if now := uint64(clock.Now().UnixNano()); cf.Start+config.SnapshotRoundGap*100 > now {
			logger.Verbosef("CheckCatchUpWithPeers local start(%d)+%d > now(%d)\n",
				cf.Start, config.SnapshotRoundGap*100, now)
			return false
		}
	}

	if updated < threshold {
		logger.Verbosef("CheckCatchUpWithPeers updated(%d) < threshold(%d)\n", updated, threshold)
	}
	return updated >= threshold
}

func (node *Node) waitOrDone(wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-node.done:
		return true
	case <-timer.C:
		return false
	}
}

type syncMap struct {
	mutex *sync.RWMutex
	m     map[crypto.Hash]*p2p.SyncPoint
}

func (s *syncMap) Set(k crypto.Hash, p *p2p.SyncPoint) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.m[k] = p
}

func (s *syncMap) Map() map[crypto.Hash]*p2p.SyncPoint {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	m := make(map[crypto.Hash]*p2p.SyncPoint)
	for k, p := range s.m {
		m[k] = p
	}
	return m
}

type legacySyncMap struct {
	mutex *sync.RWMutex
	m     map[crypto.Hash]*network.SyncPoint
}

func (s *legacySyncMap) Set(k crypto.Hash, p *network.SyncPoint) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.m[k] = p
}

func (s *legacySyncMap) Map() map[crypto.Hash]*network.SyncPoint {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	m := make(map[crypto.Hash]*network.SyncPoint)
	for k, p := range s.m {
		m[k] = p
	}
	return m
}
