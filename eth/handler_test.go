// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"math/big"
	"sort"
	"sync"
	"testing"
	"time"
)

var (
	// testKey is a private key to use for funding a tester account.
	testKey, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")

	// testAddr is the Ethereum address of the tester account.
	testAddr = crypto.PubkeyToAddress(testKey.PublicKey)
)

// testTxPool is a mock transaction pool that blindly accepts all transactions.
// Its goal is to get around setting up a valid statedb for the balance and nonce
// checks.
type testTxPool struct {
	pool map[common.Hash]*types.Transaction // Hash map of collected transactions

	txFeed event.Feed   // Notification feed to allow waiting for inclusion
	lock   sync.RWMutex // Protects the transaction pool
}

// newTestTxPool creates a mock transaction pool.
func newTestTxPool() *testTxPool {
	return &testTxPool{
		pool: make(map[common.Hash]*types.Transaction),
	}
}

// Has returns an indicator whether txpool has a transaction
// cached with the given hash.
func (p *testTxPool) Has(hash common.Hash) bool {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.pool[hash] != nil
}

// Get retrieves the transaction from local txpool with given
// tx hash.
func (p *testTxPool) Get(hash common.Hash) *types.Transaction {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.pool[hash]
}

// AddRemotes appends a batch of transactions to the pool, and notifies any
// listeners if the addition channel is non nil
func (p *testTxPool) AddRemotes(txs []*types.Transaction) []error {
	p.lock.Lock()
	defer p.lock.Unlock()

	for _, tx := range txs {
		p.pool[tx.Hash()] = tx
	}
	p.txFeed.Send(core.NewTxsEvent{Txs: txs})
	return make([]error, len(txs))
}

// Pending returns all the transactions known to the pool
func (p *testTxPool) Pending() (map[common.Address]types.Transactions, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	batches := make(map[common.Address]types.Transactions)
	for _, tx := range p.pool {
		from, _ := types.Sender(types.HomesteadSigner{}, tx)
		batches[from] = append(batches[from], tx)
	}
	for _, batch := range batches {
		sort.Sort(types.TxByNonce(batch))
	}
	return batches, nil
}

// SubscribeNewTxsEvent should return an event subscription of NewTxsEvent and
// send events to the given channel.
func (p *testTxPool) SubscribeNewTxsEvent(ch chan<- core.NewTxsEvent) event.Subscription {
	return p.txFeed.Subscribe(ch)
}

// testHandler is a live implementation of the Ethereum protocol handler, just
// preinitialized with some sane testing defaults and the transaction pool mocked
// out.
type testHandler struct {
	db      ethdb.Database
	chain   *core.BlockChain
	txpool  *testTxPool
	handler *handler
}

// newTestHandler creates a new handler for testing purposes with no blocks.
func newTestHandler() *testHandler {
	return newTestHandlerWithBlocks(0)
}

// newTestHandlerWithBlocks creates a new handler for testing purposes, with a
// given number of initial blocks.
func newTestHandlerWithBlocks(blocks int) *testHandler {
	// Create a database pre-initialize with a genesis block
	db := rawdb.NewMemoryDatabase()
	(&core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  core.GenesisAlloc{testAddr: {Balance: big.NewInt(1000000)}},
	}).MustCommit(db)

	chain, _ := core.NewBlockChain(db, nil, params.TestChainConfig, ethash.NewFaker(), vm.Config{}, nil, nil, nil)

	bs, _ := core.GenerateChain(params.TestChainConfig, chain.Genesis(), ethash.NewFaker(), db, blocks, nil)
	if _, err := chain.InsertChain(bs); err != nil {
		panic(err)
	}
	txpool := newTestTxPool()

	handler, _ := newHandler(&handlerConfig{
		Database:   db,
		Chain:      chain,
		TxPool:     txpool,
		Network:    1,
		Sync:       downloader.FastSync,
		BloomCache: 1,
	})
	//handler.acceptTxs = 1
	handler.Start(1000)

	return &testHandler{
		db:      db,
		chain:   chain,
		txpool:  txpool,
		handler: handler,
	}
}

// close tears down the handler and all its internal constructs.
func (b *testHandler) close() {
	b.handler.Stop()
	b.chain.Stop()
}

// Quorum

//Tests that when broadcasting transactions, it sends the full transactions to all peers instead of Announcing (aka sending only hashes)
func TestBroadcastTransactionsOnQuorumAchraf(t *testing.T) {
	var (
		destinationKey, _ = crypto.GenerateKey()
		totalPeers        = 1
	)

	handler := newTestHandler()
	defer handler.close()
	handler.handler.acceptTxs = 1
	p2pSrc, p2pSink := p2p.MsgPipe()
	defer p2pSrc.Close()
	defer p2pSink.Close()

	wgPeers := sync.WaitGroup{}
	wgPeers.Add(totalPeers)
	peer1 := eth.NewPeer(65, p2p.NewPeer(enode.ID{1}, "", nil), p2pSrc, handler.txpool)
	go func() {
		<-peer1.EthPeerRegistered
		wgPeers.Done()
		t.Log("Peer 1 registered yayyyyyyyyyyyyyyyyyy")
	}()
	defer peer1.Close()
	go handler.handler.runEthPeer(peer1, func(peer *eth.Peer) error {
		return eth.Handle((*ethHandler)(handler.handler), peer)
	})

	//peer2 := eth.NewPeer(65, p2p.NewPeer(enode.ID{2}, "", nil), p2pSink, handler.txpool)
	//go func() {
	//	<-peer2.EthPeerRegistered
	//	wgPeers.Done()
	//	t.Log("Peer 2 registered yay")
	//}()
	//defer peer2.Close()
	//go handler.handler.runEthPeer(peer2, func(peer *eth.Peer) error {
	//	return eth.Handle((*ethHandler)(handler.handler), peer)
	//})

	wgPeers.Wait() // wait until all peers are synced before pushing tx to the pool

	transaction := types.NewTransaction(0, crypto.PubkeyToAddress(destinationKey.PublicKey), common.Big0, uint64(3000000), common.Big0, nil)
	transactions := types.Transactions{transaction}

	doneCh := make(chan error, totalPeers)

	wgPeers.Add(totalPeers)
	defer func() {
		wgPeers.Wait()
		close(doneCh)
	}()

	handler.txpool.AddRemotes(transactions) // this will trigger the transaction broadcast/announce
	go func(p *eth.Peer) {
		t.Log("Entered peer 1 listening for message")
		doneCh <- p.ExpectPeerMessage(eth.TransactionsMsg, transactions)
		//if err := p.ExpectPeerMessage(eth.TransactionsMsg, transactions); err != nil {
		//	t.Fatalf("Message mismatch peer 1: %v", err)
		//}
		//doneCh <- nil
		t.Log("Peer 1 received message", doneCh)
		wgPeers.Done()
	}(peer1)
	//go func(p *eth.Peer) {
	//	t.Log("Entered peer 2 listening for message")
	//	doneCh <- p.ExpectPeerMessage(eth.TransactionsMsg, transactions)
	//	//if err := p.ExpectPeerMessage(eth.TransactionsMsg, transactions); err != nil {
	//	//	t.Fatalf("Message mismatch peer 2: %v", err)
	//	//}
	//	//doneCh <- nil
	//	t.Log("Peer 2 received message mmmmmmmmmmmmmmm", doneCh)
	//	wgPeers.Done()
	//}(peer2)

	var received int
	for {
		select {
		case err := <-doneCh:
			if err != nil {
				t.Fatalf("broadcast failed: %v", err)
				return
			}
			received++
			if received == totalPeers {
				// We found the right number
				return
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("timeout: broadcast count mismatch: have %d, want %d", received, totalPeers)
			return
		}
	}
}

/*
func TestBroadcastTransactionsOnQuorum(t *testing.T) {
	var (
		evmux             = new(event.TypeMux)
		pow               = ethash.NewFaker()
		db                = rawdb.NewMemoryDatabase()
		config            = &params.ChainConfig{}
		gspec             = &core.Genesis{Config: config}
		destinationKey, _ = crypto.GenerateKey()
		totalPeers        = 100
	)
	gspec.MustCommit(db)
	blockchain, _ := core.NewBlockChain(db, nil, config, pow, vm.Config{}, nil, nil, nil)
	txPool := &testTxPool{pool: make(map[common.Hash]*types.Transaction)}

	pm, err := NewProtocolManager(config, nil, downloader.FullSync, DefaultConfig.NetworkId, evmux, txPool, pow, blockchain, db, 1, nil, false)
	if err != nil {
		t.Fatalf("failed to start test protocol manager: %v", err)
	}
	pm.Start(totalPeers)
	defer pm.Stop()

	var peers []*testPeer
	wgPeers := sync.WaitGroup{}
	wgPeers.Add(totalPeers)
	for i := 0; i < totalPeers; i++ {
		peer, _ := eth.NewPeer(65, p2p.NewPeer(enode.ID{i}, "", nil),)//, eth65, pm, true)
		go func() {
			<-peer.EthPeerRegistered
			wgPeers.Done()
		}()
		defer peer.close()

		peers = append(peers, peer)
	}
	wgPeers.Wait() // wait until all peers are synced before pushing tx to the pool

	transaction := types.NewTransaction(0, crypto.PubkeyToAddress(destinationKey.PublicKey), common.Big0, uint64(3000000), common.Big0, nil)
	transactions := types.Transactions{transaction}

	txPool.AddRemotes(transactions) // this will trigger the transaction broadcast/announce

	doneCh := make(chan error, totalPeers)

	wgPeers.Add(totalPeers)
	defer func() {
		wgPeers.Wait()
		close(doneCh)
	}()

	for _, peer := range peers {
		go func(p *eth.Peer) {
			doneCh <- p.ExpectPeerMessage(eth.TransactionsMsg, transactions)
			wgPeers.Done()
		}(peer)
	}
	var received int
	for {
		select {
		case err := <-doneCh:
			if err != nil {
				t.Fatalf("broadcast failed: %v", err)
				return
			}
			received++
			if received == totalPeers {
				// We found the right number
				return
			}
		case <-time.After(2 * time.Second):
			t.Errorf("timeout: broadcast count mismatch: have %d, want %d", received, totalPeers)
			return
		}
	}
}
*/
/*
// testPeer is a simulated peer to allow testing direct network calls.
type testPeer struct {
	*eth.Peer

	net p2p.MsgReadWriter // Network layer reader/writer to simulate remote messaging
	app *p2p.MsgPipeRW    // Application layer reader/writer to simulate the local side
}

// newTestPeer creates a new peer registered at the given data backend.
func newTestPeer(name string, version uint, backend *testBackend) (*testPeer, <-chan error) {
	// Create a message pipe to communicate through
	app, net := p2p.MsgPipe()

	// Start the peer on a new thread
	var id enode.ID
	rand.Read(id[:])

	peer := eth.NewPeer(version, p2p.NewPeer(id, name, nil), net, backend.TxPool())
	errc := make(chan error, 1)
	go func() {
		errc <- backend.RunPeer(peer, func(peer *eth.Peer) error {
			return eth.Handle(backend, peer)
		})
	}()
	return &testPeer{app: app, net: net, Peer: peer}, errc
}

// close terminates the local side of the peer, notifying the remote protocol
// manager of termination.
func (p *testPeer) close() {
	p.Peer.Close()
	p.app.Close()
}


// testBackend is a mock implementation of the live Ethereum message handler. Its
// purpose is to allow testing the request/reply workflows and wire serialization
// in the `eth` protocol without actually doing any data processing.
type testBackend struct {
	db     ethdb.Database
	chain  *core.BlockChain
	//_txpool *core.TxPool
	txpool *testTxPool
}

// newTestBackend creates an empty chain and wraps it into a mock backend.
func newTestBackend(blocks int) *testBackend {
	return newTestBackendWithGenerator(blocks, nil)
}

// newTestBackend creates a chain with a number of explicitly defined blocks and
// wraps it into a mock backend.
func newTestBackendWithGenerator(blocks int, generator func(int, *core.BlockGen)) *testBackend {
	// Create a database pre-initialize with a genesis block
	db := rawdb.NewMemoryDatabase()
	(&core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  core.GenesisAlloc{testAddr: {Balance: big.NewInt(1000000)}},
	}).MustCommit(db)

	chain, _ := core.NewBlockChain(db, nil, params.TestChainConfig, ethash.NewFaker(), vm.Config{}, nil, nil, nil)

	bs, _ := core.GenerateChain(params.TestChainConfig, chain.Genesis(), ethash.NewFaker(), db, blocks, generator)
	if _, err := chain.InsertChain(bs); err != nil {
		panic(err)
	}
	txconfig := core.DefaultTxPoolConfig
	txconfig.Journal = "" // Don't litter the disk with test journals

	return &testBackend{
		db:     db,
		chain:  chain,
		//txpool: core.NewTxPool(txconfig, params.TestChainConfig, chain),
		txpool: &testTxPool{pool: make(map[common.Hash]*types.Transaction)},
	}
}

// close tears down the transaction pool and chain behind the mock backend.
func (b *testBackend) close() {
	//b.txpool.Stop()
	b.chain.Stop()
}

func (b *testBackend) Chain() *core.BlockChain     { return b.chain }
func (b *testBackend) StateBloom() *trie.SyncBloom { return nil }
func (b *testBackend) TxPool() eth.TxPool              { return b.txpool }

func (b *testBackend) RunPeer(peer *eth.Peer, handler eth.Handler) error {
	// Normally the backend would do peer mainentance and handshakes. All that
	// is omitted and we will just give control back to the handler.
	return handler(peer)
}
func (b *testBackend) PeerInfo(enode.ID) interface{} { panic("not implemented") }

func (b *testBackend) AcceptTxs() bool {
	panic("data processing tests should be done in the handler package")
}
func (b *testBackend) Handle(*eth.Peer, eth.Packet) error {
	panic("data processing tests should be done in the handler package")
}
*/
/*
// Tests that when broadcasting transactions, it sends the full transactions to all peers instead of Announcing (aka sending only hashes)
func TestBroadcastTransactionsOnQuorumRicardo(t *testing.T) {
	var (
		//evmux             = new(event.TypeMux)
		//pow               = ethash.NewFaker()
		//db                = rawdb.NewMemoryDatabase()
		//config            = &params.ChainConfig{}
		//gspec             = &core.Genesis{Config: config}
		destinationKey, _ = crypto.GenerateKey()
		totalPeers        = 3
	)
	//gspec.MustCommit(db)
	//blockchain, _ := core.NewBlockChain(db, nil, config, pow, vm.Config{}, nil, nil, nil)
	//txPool := &testTxPool{pool: make(map[common.Hash]*types.Transaction)}

	//pm, err := NewProtocolManager(config, nil, downloader.FullSync, DefaultConfig.NetworkId, evmux, txPool, pow, blockchain, db, 1, nil, false)
	//if err != nil {
	//	t.Fatalf("failed to start test protocol manager: %v", err)
	//}
	//pm.Start(totalPeers)
	//defer pm.Stop()
	//handler := newTestHandler()
	//defer handler.close()

	backend := newTestBackend(1024 + 15)
	defer backend.close()

	var peers []*testPeer
	wgPeers := sync.WaitGroup{}
	wgPeers.Add(totalPeers)
	for i := 0; i < totalPeers; i++ {
		peer, _ := newTestPeer(fmt.Sprintf("peer %d", i), 65, backend)
		go func() {
			<-peer.EthPeerRegistered
			wgPeers.Done()
		}()
		defer peer.close()

		peers = append(peers, peer)
	}
	wgPeers.Wait() // wait until all peers are synced before pushing tx to the pool

	transaction := types.NewTransaction(0, crypto.PubkeyToAddress(destinationKey.PublicKey), common.Big0, uint64(3000000), common.Big0, nil)
	transactions := types.Transactions{transaction}

	backend.txpool.AddRemotes(transactions) // this will trigger the transaction broadcast/announce

	doneCh := make(chan error, totalPeers)

	wgPeers.Add(totalPeers)
	defer func() {
		wgPeers.Wait()
		close(doneCh)
	}()

	for _, peer := range peers {
		go func(p *testPeer) {
			doneCh <- p2p.ExpectMsg(p.app, eth.TransactionsMsg, transactions)
			wgPeers.Done()
		}(peer)
	}
	var received int
	for {
		select {
		case err := <-doneCh:
			if err != nil {
				t.Fatalf("broadcast failed: %v", err)
				return
			}
			received++
			if received == totalPeers {
				// We found the right number
				return
			}
		case <-time.After(2 * time.Second):
			t.Errorf("timeout: broadcast count mismatch: have %d, want %d", received, totalPeers)
			return
		}
	}
}
*/
/*
func TestBroadcastTransactionsOnQuorum(t *testing.T) {
	var (
		//evmux             = new(event.TypeMux)
		//pow               = ethash.NewFaker()
		//db                = rawdb.NewMemoryDatabase()
		//config            = &params.ChainConfig{}
		//gspec             = &core.Genesis{Config: config}
		destinationKey, _ = crypto.GenerateKey()
		totalPeers        = 1
	)
	//gspec.MustCommit(db)
	//blockchain, _ := core.NewBlockChain(db, nil, config, pow, vm.Config{}, nil, nil, nil)
	//txPool := &testTxPool{pool: make(map[common.Hash]*types.Transaction)}
	//
	//pm, err := NewProtocolManager(config, nil, downloader.FullSync, DefaultConfig.NetworkId, evmux, txPool, pow, blockchain, db, 1, nil, false)
	//if err != nil {
	//	t.Fatalf("failed to start test protocol manager: %v", err)
	//}
	//pm.Start(totalPeers)
	//defer pm.Stop()

	backend := newTestBackend(1024 + 15)
	defer backend.close()

	var peers []*testPeer
	wgPeers := sync.WaitGroup{}
	wgPeers.Add(totalPeers)
	for i := 0; i < totalPeers; i++ {
		peer, _ := newTestPeer(fmt.Sprintf("peer %d", i), 65, backend)
		go func() {
			<-peer.EthPeerRegistered
			t.Log("peer registered ", i)
			wgPeers.Done()
		}()
		defer peer.close()

		peers = append(peers, peer)
	}
	t.Log("log something")
	wgPeers.Wait() // wait until all peers are synced before pushing tx to the pool
	t.Log("done waiting ")

	transaction := types.NewTransaction(0, crypto.PubkeyToAddress(destinationKey.PublicKey), common.Big0, uint64(3000000), common.Big0, nil)
	transactions := types.Transactions{transaction}

	backend.txpool.AddRemotes(transactions) // this will trigger the transaction broadcast/announce

	doneCh := make(chan error, totalPeers)

	wgPeers.Add(totalPeers)
	defer func() {
		wgPeers.Wait()
		close(doneCh)
	}()

	for _, peer := range peers {
		go func(p *testPeer) {
			doneCh <- p2p.ExpectMsg(p.app, eth.TransactionsMsg, transactions)
			wgPeers.Done()
		}(peer)
	}
	var received int
	for {
		select {
		case err := <-doneCh:
			if err != nil {
				t.Fatalf("broadcast failed: %v", err)
				return
			}
			received++
			if received == totalPeers {
				// We found the right number
				return
			}
		case <-time.After(2 * time.Second):
			t.Errorf("timeout: broadcast count mismatch: have %d, want %d", received, totalPeers)
			return
		}
	}
}
*/
