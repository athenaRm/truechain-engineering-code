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

package miner

import (
	"bytes"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/truechain/truechain-engineering-code/common"
	"github.com/truechain/truechain-engineering-code/consensus"
	"github.com/truechain/truechain-engineering-code/consensus/misc"
	//	"github.com/truechain/truechain-engineering-code/consensus/minerva"
	"github.com/truechain/truechain-engineering-code/core"
	"github.com/truechain/truechain-engineering-code/core/state"
	"github.com/truechain/truechain-engineering-code/core/types"
	//"github.com/truechain/truechain-engineering-code/core/vm"
	"github.com/truechain/truechain-engineering-code/ethdb"
	"github.com/truechain/truechain-engineering-code/event"
	"github.com/truechain/truechain-engineering-code/log"
	"github.com/truechain/truechain-engineering-code/params"
	chain "github.com/truechain/truechain-engineering-code/core/snailchain"
	"gopkg.in/fatih/set.v0"
)

const (
	resultQueueSize  = 10
	miningLogAtDepth = 5

	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096
	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10
	// chainSideChanSize is the size of channel listening to ChainSideEvent.
	chainSideChanSize = 10
)

var (
	// fruit freshness
	fruitFreshness *big.Int = big.NewInt(17)
	//pinter hash Fresh
	pointerHashFresh *big.Int = big.NewInt(7)
)

// Agent can register themself with the worker
type Agent interface {
	Work() chan<- *Work
	SetReturnCh(chan<- *Result)
	Stop()
	Start()
	GetHashRate() int64
}

// Work is the workers current environment and holds
// all of the current state information
type Work struct {
	config *params.ChainConfig
	signer types.Signer

	state     *state.StateDB // apply state changes here
	ancestors *set.Set       // ancestor set (used for checking uncle parent validity)
	family    *set.Set       // family set (used for checking uncle invalidity)
	uncles    *set.Set       // uncle set
	tcount    int            // tx count in cycle
	gasPool   *core.GasPool  // available gas used to pack transactions

	Block *types.SnailBlock // the new block
	
	FruitSet []*types.SnailBlock //the for fruitset

	header   *types.SnailHeader
	txs      []*types.Transaction
	receipts []*types.Receipt
	fruits   []*types.SnailBlock // for the fresh
	signs    []*types.PbftSign
	body   *types.SnailBody

	createdAt time.Time
}

type Result struct {
	Work  *Work
	Block *types.SnailBlock
}

// worker is the main object which takes care of applying messages to the new state
type worker struct {
	config *params.ChainConfig
	engine consensus.Engine

	mu sync.Mutex

	// update loop
	mux *event.TypeMux

	fruitCh   chan chain.NewFruitsEvent
	fruitSub  event.Subscription // for fruit pool

	fruitChainCh   chan chain.NewMinedFruitEvent // for chain event not pool
	fruitChainSub  event.Subscription // for chain event not pool


	fastBlockCh  chan chain.NewFastBlocksEvent
	fastBlockSub event.Subscription //for fast block pool

	chainHeadCh  chan chain.ChainHeadEvent
	chainHeadSub event.Subscription
	chainSideCh  chan chain.ChainSideEvent
	chainSideSub event.Subscription
	wg           sync.WaitGroup

	agents map[Agent]struct{}
	recv   chan *Result

	etrue     Backend
	chain   *chain.SnailBlockChain
	snailchain  *chain.SnailBlockChain
	proc    chain.Validator
	chainDb ethdb.Database

	coinbase common.Address
	extra    []byte
	toElect   bool // for elect
	FruitOnly  bool // only miner fruit
	publickey   []byte// for publickey	

	currentMu sync.Mutex
	current   *Work

	snapshotMu    sync.RWMutex
	snapshotBlock *types.SnailBlock
	snapshotState *state.StateDB

	uncleMu        sync.Mutex
	possibleUncles map[common.Hash]*types.SnailBlock

	unconfirmed *unconfirmedBlocks // set of locally mined blocks pending canonicalness confirmations

	// atomic status counters
	mining int32
	atWork int32
	atCommintNewWoker bool
	FastBlockNumber   *big.Int
} 
 
func newWorker(config *params.ChainConfig, engine consensus.Engine, coinbase common.Address, etrue Backend, mux *event.TypeMux) *worker {
	worker := &worker{
		config:         config,
		engine:         engine,
		etrue:          etrue,
		mux:            mux,	
		//txsCh:          make(chan chain.NewTxsEvent, txChanSize),
		fruitCh:        make(chan chain.NewFruitsEvent, txChanSize),
		fruitChainCh:        make(chan chain.NewMinedFruitEvent, txChanSize),
		fastBlockCh:       make(chan chain.NewFastBlocksEvent, txChanSize),
		chainHeadCh:    make(chan chain.ChainHeadEvent, chainHeadChanSize),
		chainSideCh:    make(chan chain.ChainSideEvent, chainSideChanSize),
		chainDb:        etrue.ChainDb(),
		recv:           make(chan *Result, resultQueueSize),
		//TODO need konw how to 
		chain:          etrue.SnailBlockChain(),
		snailchain:     etrue.SnailBlockChain(),
		proc:           etrue.SnailBlockChain().Validator(),
		possibleUncles: make(map[common.Hash]*types.SnailBlock),
		coinbase:       coinbase,
		agents:         make(map[Agent]struct{}),
		unconfirmed:    newUnconfirmedBlocks(etrue.SnailBlockChain(), miningLogAtDepth),
		FastBlockNumber: big.NewInt(0),
	}
	//worker.txsSub = etrue.TxPool().SubscribeNewTxsEvent(worker.txsCh)
	// Subscribe events for blockchain
	worker.chainHeadSub = etrue.SnailBlockChain().SubscribeChainHeadEvent(worker.chainHeadCh)
	worker.chainSideSub = etrue.SnailBlockChain().SubscribeChainSideEvent(worker.chainSideCh)

	worker.fruitSub = etrue.SnailPool().SubscribeNewFruitEvent(worker.fruitCh)
	worker.fastBlockSub = etrue.SnailPool().SubscribeNewFastBlockEvent(worker.fastBlockCh)
	worker.fruitChainSub = etrue.SnailBlockChain().SubscribeNewMinedFruitEvent(worker.fruitChainCh)

	go worker.update()

	go worker.wait()
	worker.commitNewWork()

	return worker
}

func (self *worker) setEtherbase(addr common.Address) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.coinbase = addr
}

func (self *worker) setElection(toElect bool, pubkey []byte) {
	self.mu.Lock()
	defer  self.mu.Unlock()

	self.toElect = toElect
	self.publickey = make([]byte, len(pubkey))
	copy(self.publickey, pubkey)
}

func (self *worker) SetFruitOnly(FruitOnly bool) {
	self.mu.Lock()
	defer  self.mu.Unlock()

	self.FruitOnly = FruitOnly
}


func (self *worker) setExtra(extra []byte) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.extra = extra
}

func (self *worker) pending() (*types.Block, *state.StateDB) {
	if atomic.LoadInt32(&self.mining) == 0 {
		// return a snapshot to avoid contention on currentMu mutex
		self.snapshotMu.RLock()
		defer self.snapshotMu.RUnlock()
		//return self.snapshotBlock, self.snapshotState.Copy()
		return nil,nil
	}

	self.currentMu.Lock()
	defer self.currentMu.Unlock()
	return nil,nil
	//return self.current.Block, self.current.state.Copy()
}

func (self *worker) pendingSnail() (*types.SnailBlock, *state.StateDB) {
	if atomic.LoadInt32(&self.mining) == 0 {
		// return a snapshot to avoid contention on currentMu mutex
		self.snapshotMu.RLock()
		defer self.snapshotMu.RUnlock()
		return self.snapshotBlock, self.snapshotState.Copy()
	}

	self.currentMu.Lock()
	defer self.currentMu.Unlock()
	return self.current.Block, self.current.state.Copy()
}

func (self *worker) pendingBlock() *types.Block {
	if atomic.LoadInt32(&self.mining) == 0 {
		// return a snapshot to avoid contention on currentMu mutex
		self.snapshotMu.RLock()
		defer self.snapshotMu.RUnlock()
		//TODO 20180805
		//return snapshot
		return nil
	}

	self.currentMu.Lock()
	defer self.currentMu.Unlock()
	//return self.current.Block
	return nil
}


func (self *worker) pendingSnailBlock() *types.SnailBlock {
	if atomic.LoadInt32(&self.mining) == 0 {
		// return a snapshot to avoid contention on currentMu mutex
		self.snapshotMu.RLock()
		defer self.snapshotMu.RUnlock()
		return self.snapshotBlock
	}

	self.currentMu.Lock()
	defer self.currentMu.Unlock()
	return self.current.Block
}


func (self *worker) start() {
	self.mu.Lock()
	defer self.mu.Unlock()

	atomic.StoreInt32(&self.mining, 1)

	// spin up agents
	for agent := range self.agents {
		agent.Start()
	}
}

func (self *worker) stop() {
	self.wg.Wait()

	self.mu.Lock()
	defer self.mu.Unlock()
	if atomic.LoadInt32(&self.mining) == 1 {
		for agent := range self.agents {
			agent.Stop()
		}
	}
	self.atCommintNewWoker = false
	atomic.StoreInt32(&self.mining, 0)
	atomic.StoreInt32(&self.atWork, 0)
	
}

func (self *worker) register(agent Agent) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.agents[agent] = struct{}{}
	agent.SetReturnCh(self.recv)
}

func (self *worker) unregister(agent Agent) {
	self.mu.Lock()
	defer self.mu.Unlock()
	delete(self.agents, agent)
	agent.Stop()
}

func (self *worker) update() {
	//defer self.txsSub.Unsubscribe()
	defer self.chainHeadSub.Unsubscribe()
	defer self.chainSideSub.Unsubscribe()
	defer self.fastBlockSub.Unsubscribe()
	defer self.fruitSub.Unsubscribe()
	defer self.fruitChainSub.Unsubscribe()

	for {
		// A real event arrived, process interesting content
		select {
		// Handle ChainHeadEvent
		case <-self.chainHeadCh:
			if !self.atCommintNewWoker {
				log.Info("star commit new work  chainHeadCh")
				if atomic.LoadInt32(&self.mining) == 1{
					self.commitNewWork()
				}
				
			}

		// Handle ChainSideEvent
		case ev := <-self.chainSideCh:
			self.uncleMu.Lock()
			self.possibleUncles[ev.Block.Hash()] = ev.Block
			self.uncleMu.Unlock()

		//TODO　fruit event
		case <-self.fruitCh:
			log.Info("----------------start commit new work  fruitCh")
			// if only fruit only not need care about fruit event
			if !self.atCommintNewWoker && !self.FruitOnly {
				// after get the fruit event should star mining if have not mining
				log.Info("star commit new work  fruitCh")
				
				if atomic.LoadInt32(&self.mining) == 1{
					self.commitNewWork()
				}	
			}
		case  <-self.fastBlockCh:
			log.Info("------------start commit new work  fastBlockCh")
			if !self.atCommintNewWoker {
				log.Info("star commit new work  fastBlockCh")
				if atomic.LoadInt32(&self.mining) == 1{
					self.commitNewWork()
				}
					
			}else{
				log.Info("------------start commit new work  true?????")
			}
		case <-self.fruitChainCh:
			//self.commitNewWork()

		// TODO fast block event
		case <-self.fastBlockSub.Err():
			
			return
		case <-self.fruitSub.Err():
			return
		case <-self.fruitChainSub.Err():
			return
		case <-self.chainHeadSub.Err():
			return
		case <-self.chainSideSub.Err():
			return
		}
	}
}

func (self *worker) wait() {
	for {
		for result := range self.recv {
			atomic.AddInt32(&self.atWork, -1)

			if result == nil {
				continue
			}

			block := result.Block
			//work := result.Work

			if block.IsFruit() {
				if block.FastNumber() == nil {
					// if it does't include a fast block signs, it's not a fruit
					continue
				}
				if block.FastNumber().Cmp(common.Big0) == 0 {
					continue
				}

				//log.Info("🍒 —-------mined fruit"," FB NUMBER",block.FastNumber())
				
				// add fruit once 
				if self.FastBlockNumber != nil{

					if self.FastBlockNumber.Cmp(block.FastNumber()) !=0 {
						log.Info("🍒 —-------mined fruit"," FB NUMBER",block.FastNumber())
						//log.Info("not same fruits")
						var newFruits []*types.SnailBlock
						newFruits = append(newFruits, block)
						self.etrue.SnailPool().AddRemoteFruits(newFruits)
					}else{
						//log.Info("the same fruits")
					}

				}else{
					log.Info("🍒 —-------mined fruit"," FB NUMBER",block.FastNumber())
					var newFruits []*types.SnailBlock
					newFruits = append(newFruits, block)
					self.etrue.SnailPool().AddRemoteFruits(newFruits)
				}
				

				// make sure the fast number has been fruit
				self.FastBlockNumber.SetUint64(block.FastNumber().Uint64())

				// only have fast block not fruits we need commit new work
				if self.current.fruits == nil{
					self.atCommintNewWoker = false
				}
			} else {
				log.Info("+++++ mined block  ---  "," FB NUMBER",block.FastNumber(),"block number",block.Number())
				
				if block.Fruits() != nil{
					for _,fruit :=range block.Fruits(){
						log.Info(" ^^^  block fruits","fb number",fruit.FastNumber())
					}
				}

				stat, err := self.chain.WriteCanonicalBlock(block)
				if err != nil {
					log.Error("Failed writing block to chain", "err", err)
					continue
				}
 
				// Broadcast the block and announce chain insertion event
				self.mux.Post(chain.NewMinedBlockEvent{Block: block})
				var (
					events []interface{}
				)
				events = append(events, chain.ChainEvent{Block: block, Hash: block.Hash()})
				if stat == chain.CanonStatTy {
					events = append(events, chain.ChainHeadEvent{Block: block})
				}
				self.chain.PostChainEvents(events)

				// Insert the block into the set of pending ones to wait for confirmations
				self.unconfirmed.Insert(block.NumberU64(), block.Hash())

				self.atCommintNewWoker = false

			}

		}
	}
}

// push sends a new work task to currently live miner agents.
func (self *worker) push(work *Work) {
	if atomic.LoadInt32(&self.mining) != 1 {
		self.atCommintNewWoker = false
		return
	}
	for agent := range self.agents {
		atomic.AddInt32(&self.atWork, 1)
		if ch := agent.Work(); ch != nil {
			ch <- work
		}
	}
}

// makeCurrent creates a new environment for the current cycle.
func (self *worker) makeCurrent(parent *types.SnailBlock, header *types.SnailHeader) error {
	work := &Work{
		config:    self.config,
		signer:    types.NewEIP155Signer(self.config.ChainID),
		ancestors: set.New(),
		family:    set.New(),
		uncles:    set.New(),
		header:    header,
		createdAt: time.Now(),
	}

	// when 08 is processed ancestors contain 07 (quick block)
	for _, ancestor := range self.chain.GetBlocksFromHash(parent.Hash(), 7) {
		//TODO need add snail uncles 20180804
		/*
		for _, uncle := range ancestor.Uncles() {
			work.family.Add(uncle.Hash())
		}
		*/
		work.family.Add(ancestor.Hash())
		work.ancestors.Add(ancestor.Hash())
	}

	// Keep track of transactions which return errors so they can be removed
	work.tcount = 0
	self.current = work

	return nil
}


// TODO: if there are no fast blocks and fruits, can't mine a new snail block or fruit
func (self *worker) commitNewWork() {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.uncleMu.Lock()
	defer self.uncleMu.Unlock()
	self.currentMu.Lock()
	defer self.currentMu.Unlock()
	
	tstart := time.Now()
	parent := self.chain.CurrentBlock()
	self.atCommintNewWoker  = true
	
	//can not start miner when  fruits and fast block 
	tstamp := tstart.Unix()
	if parent.Time().Cmp(new(big.Int).SetInt64(tstamp)) >= 0 {
		tstamp = parent.Time().Int64() + 1
	}
	// this will ensure we're not going off too far in the future
	if now := time.Now().Unix(); tstamp > now+1 {
		wait := time.Duration(tstamp-now) * time.Second
		log.Info("Mining too far in the future", "wait", common.PrettyDuration(wait))
		time.Sleep(wait)
	}


	num := parent.Number()
	//TODO need add more struct member
	header := &types.SnailHeader{
		ParentHash:  parent.Hash(),
		ToElect:	 self.toElect,
		Publickey:   self.publickey,
		Number:      num.Add(num, common.Big1),
		Extra:       self.extra,
		Time:        big.NewInt(tstamp),
	}
	
	// Only set the coinbase if we are mining (avoid spurious block rewards)
	if atomic.LoadInt32(&self.mining) == 1 {
		header.Coinbase = self.coinbase
	}
	if err := self.engine.PrepareSnail(self.chain, header); err != nil {
		log.Error("Failed to prepare header for mining", "err", err)
		self.atCommintNewWoker  = false
		return
	}
	// Set the pointerHash 
	pointerNum := new(big.Int).Sub(parent.Number(), pointerHashFresh)
	if pointerNum.Cmp(common.Big0) < 0 {
		pointerNum = new(big.Int).Set(common.Big0)
	}
	header.PointerHash = self.chain.GetBlockByNumber(pointerNum.Uint64()).Hash()
	
	// If we are care about TheDAO hard-fork check whether to override the extra-data or not
	if daoBlock := self.config.DAOForkBlock; daoBlock != nil {
		// Check whether the block is among the fork extra-override range
		limit := new(big.Int).Add(daoBlock, params.DAOForkExtraRange)
		if header.Number.Cmp(daoBlock) >= 0 && header.Number.Cmp(limit) < 0 {
			// Depending whether we support or oppose the fork, override differently
			if self.config.DAOForkSupport {
				header.Extra = common.CopyBytes(params.DAOForkBlockExtra)
			} else if bytes.Equal(header.Extra, params.DAOForkBlockExtra) {
				header.Extra = []byte{} // If miner opposes, don't let it use the reserved extra-data
			}
		}
	}
	// Could potentially happen if starting to mine in an odd state.
	err := self.makeCurrent(parent, header)
	if err != nil {
		log.Error("Failed to create mining context", "err", err)
		self.atCommintNewWoker  = false
		return
	}
	// Create the current work task and check any fork transitions needed
	work := self.current
	if self.config.DAOForkSupport && self.config.DAOForkBlock != nil && self.config.DAOForkBlock.Cmp(header.Number) == 0 {
		misc.ApplyDAOHardFork(work.state)
	}	
 
	
	fastblock, errFb := self.etrue.SnailPool().PendingFastBlocks()
	if errFb != nil {
		self.atCommintNewWoker  = false
		return
	}

	
	fruits, errFruit := self.etrue.SnailPool().PendingFruits()
	if errFruit != nil {
		self.atCommintNewWoker  = false
		return
	}
	
	// only miner fruit if not fruit set only miner the fruit
	if self.FruitOnly {
		fruits = nil 
	}

	if fastblock == nil && fruits == nil{
		log.Info("__commit new work no fruits and fast block not start miner")
		self.atCommintNewWoker  = false
		return
	}
	
	
	if fastblock != nil{
		self.commitFastBlocks(fastblock)
		
		/*
		if self.FastBlockNumber == nil {		
			self.FastBlockNumber = new(big.Int).Set(common.Big0)
			self.FastBlockNumber.SetUint64(fastblock.NumberU64()) 		
		}else{
			if self.FastBlockNumber.Cmp(fastblock.Number()) == 0{
			}else{
				self.FastBlockNumber.SetUint64(fastblock.NumberU64()) 
			}
		}

		self.current.header.FastNumber = fastblock.Number()
		self.current.header.FastHash = fastblock.Hash()
		signs := fastblock.Body().Signs
		work.signs = make([]*types.PbftSign, len(signs))
		for i := range signs {
			work.signs[i] = types.CopyPbftSign(signs[i])
		}
		*/
	}
	
	// commit fruits make sure it is correct
	if fruits != nil{
		work.commitFruits(fruits, self.snailchain, self.coinbase)
	}
	
	// set work block
	work.Block = types.NewSnailBlock(
		self.current.header,
		self.current.fruits,
		self.current.signs,
		nil, 
	)

	

	if self.current.Block.FastNumber().Cmp(big.NewInt(0)) == 0 && self.current.Block.Fruits() == nil{
		log.Info("__commit new work have not fruits and fast block do not start miner  again")
		self.atCommintNewWoker  = false
		return
	} 

	// compute uncles for the new block.
	var (
		uncles    []*types.SnailHeader
		badUncles []common.Hash
	)
	for hash, uncle := range self.possibleUncles {
		if len(uncles) == 2 {
			
			break
		}
		if err := self.commitUncle(work, uncle.Header()); err != nil {
			log.Trace("Bad uncle found and will be removed", "hash", hash)
			log.Trace(fmt.Sprint(uncle))

			badUncles = append(badUncles, hash)
		} else {
			log.Debug("Committing new uncle to block", "hash", hash)
			uncles = append(uncles, uncle.Header())
		}
	}
	for _, hash := range badUncles {
		delete(self.possibleUncles, hash)
	}
	
	// TODO: get fruits from tx pool
	// Create the new block to seal with the consensus engine
	if work.Block, err = self.engine.FinalizeSnail(self.chain, header, uncles, work.fruits, work.signs); err != nil {
		log.Error("Failed to finalize block for sealing", "err", err)
		self.atCommintNewWoker  = false
		return
	}


	// We only care about logging if we're actually mining.
	if atomic.LoadInt32(&self.mining) == 1 {
		log.Info("________Commit new mining work", "number", work.Block.Number(), "txs", len(work.txs), "uncles", len(uncles), "fruits", len(work.Block.Fruits()), " fastblock", work.Block.FastNumber(),"elapsed", common.PrettyDuration(time.Since(tstart)))
		self.unconfirmed.Shift(work.Block.NumberU64() - 1)
	}
	
	self.push(work)
	self.updateSnapshot()
}

func (self *worker) commitUncle(work *Work, uncle *types.SnailHeader) error {
	hash := uncle.Hash()
	if work.uncles.Has(hash) {
		return fmt.Errorf("uncle not unique")
	}
	if !work.ancestors.Has(uncle.ParentHash) {
		return fmt.Errorf("uncle's parent unknown (%x)", uncle.ParentHash[0:4])
	}
	if work.family.Has(hash) {
		return fmt.Errorf("uncle already in family (%x)", hash)
	}
	work.uncles.Add(uncle.Hash())
	return nil
}

func (self *worker) updateSnapshot() {
	self.snapshotMu.Lock()
	defer self.snapshotMu.Unlock()

	self.snapshotBlock = types.NewSnailBlock(
		self.current.header,
		self.current.fruits,
		self.current.signs,
		nil,
	)

	//self.snapshotState = self.current.state.Copy()
}


// TODO: check fruit freshness?
func (env *Work) commitFruit(fruit *types.SnailBlock, bc *chain.SnailBlockChain, lastNumber *big.Int) error {

	if fruit.FastNumber().Cmp(lastNumber) <= 0 {
		return consensus.ErrInvalidNumber
	}
	//TODO should add pointer 
	pointer := bc.GetBlockByHash(fruit.PointerHash())
	if pointer == nil {
		//log.Info("   22  commit fruit","fast num",fruit.FastNumber())
		return core.ErrInvalidPointer
	}

	freshNumber := new(big.Int).Sub(env.header.Number, pointer.Number())
 
	if freshNumber.Cmp(fruitFreshness) > 0 {
		return core.ErrFreshness
	}

	return nil
}


// TODO: check fruits continue with last snail block
// find all fruits and start to the last parent fruits number and end continue fruit list 
func (env *Work) commitFruits(fruits []*types.SnailBlock, bc *chain.SnailBlockChain, coinbase common.Address) {

	var lastFastNumber *big.Int
	parent := bc.CurrentBlock()
	fs := parent.Fruits()

	/*
	for _, fruit := range fs {
		log.Info("----parent number","fb number",fruit.FastNumber())
	}
	*/

	if len(fs) > 0 {
		lastFastNumber = fs[len(fs) - 1].FastNumber()
	} else {
		lastFastNumber = new(big.Int).Set(common.Big0)
	}
	var fruitsContine []*types.SnailBlock 
	startCoypfruits := false
	
	// find the continue fruits
	
	for _, fruit := range fruits {
	//	log.Info("----pending number","fb number",fruit.FastNumber())
		if lastFastNumber.Cmp(common.Big0) == 0{
			startCoypfruits = true
		}
		if lastFastNumber.Cmp(fruit.FastNumber()) == 0 || lastFastNumber.Uint64() == fruit.FastNumber().Uint64()-1{
			startCoypfruits = true
			fruitsContine = append(fruitsContine, fruit)
			continue
		}
		if startCoypfruits {
			if lastFastNumber.Uint64()<fruit.FastNumber().Uint64(){
				if fruitsContine == nil{
					fruitsContine = append(fruitsContine, fruit)
				}else{
					if fruitsContine[len(fruitsContine)-1].FastNumber().Uint64() == fruit.FastNumber().Uint64()-1{
						fruitsContine = append(fruitsContine, fruit)
					}else{
						break
					}
				}
			}
		}
	}
	//log.Info(" end 11 ----------------- not continue number")
		//log.Info(" start 22 -----------------continue number")
	for _, fruit := range fruitsContine {
		//log.Info("---continue number","fb number",fruit.FastNumber())
		err := env.commitFruit(fruit, bc, lastFastNumber)
		if err == nil {
			env.fruits = append(env.fruits, fruit)
		}
	}

}

// find a corect fast block to miner
func (self *worker) commitFastBlocks(fastBlocks types.Blocks) error{
	
	if atomic.LoadInt32(&self.mining) == 0{
		return nil
	}

	if fastBlocks == nil{
		return core.ErrNoFastBlockToMiner
	} 
	
	/*
	log.Info("   1111111     fast block number","fb self.FastBlockNumber", self.FastBlockNumber )
	for _ , fb := range fastBlocks {
		log.Info(" ====---- fb list","fb number",fb.Number())
	}
	*/
	
	var fastBlock *types.Block
	for _ , fb := range fastBlocks {
		if self.FastBlockNumber.Uint64() == 0{
			log.Info("1")
			self.FastBlockNumber = new(big.Int).Set(common.Big0)
			//self.FastBlockNumber.SetUint64(fb.NumberU64()) 
			fastBlock = fb
			break	
		}

		// this fast block has been minered but pending not update
		if self.FastBlockNumber.Uint64() >= fb.NumberU64(){
			
			continue
		}
		//self.FastBlockNumber.SetUint64(fb.NumberU64())
		fastBlock = fb
		break
	} 

	log.Info("fast block number","fb self.FastBlockNumber", self.FastBlockNumber )
	if fastBlock != nil{
		self.current.header.FastNumber = fastBlock.Number()
		self.current.header.FastHash = fastBlock.Hash()
		signs := fastBlock.Body().Signs
		self.current.signs = make([]*types.PbftSign, len(signs))
		for i := range signs {
			self.current.signs[i] = types.CopyPbftSign(signs[i])
		}
	}
	return nil
}
