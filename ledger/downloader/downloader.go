//
// (at your option) any later version.
//
//

package downloader

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	ethereum "github.com/5uwifi/canchain"
	"github.com/5uwifi/canchain/common"
	"github.com/5uwifi/canchain/kernel/rawdb"
	"github.com/5uwifi/canchain/kernel/types"
	"github.com/5uwifi/canchain/candb"
	"github.com/5uwifi/canchain/basis/event"
	"github.com/5uwifi/canchain/basis/log4j"
	"github.com/5uwifi/canchain/basis/metrics"
	"github.com/5uwifi/canchain/params"
)

var (
	MaxHashFetch    = 512 // Amount of hashes to be fetched per retrieval request
	MaxBlockFetch   = 128 // Amount of blocks to be fetched per retrieval request
	MaxHeaderFetch  = 192 // Amount of block headers to be fetched per retrieval request
	MaxSkeletonSize = 128 // Number of header fetches to need for a skeleton assembly
	MaxBodyFetch    = 128 // Amount of block bodies to be fetched per retrieval request
	MaxReceiptFetch = 256 // Amount of transaction receipts to allow fetching per request
	MaxStateFetch   = 384 // Amount of node state values to allow fetching per request

	MaxForkAncestry  = 3 * params.EpochDuration // Maximum chain reorganisation
	rttMinEstimate   = 2 * time.Second          // Minimum round-trip time to target for download requests
	rttMaxEstimate   = 20 * time.Second         // Maximum round-trip time to target for download requests
	rttMinConfidence = 0.1                      // Worse confidence factor in our estimated RTT value
	ttlScaling       = 3                        // Constant scaling factor for RTT -> TTL conversion
	ttlLimit         = time.Minute              // Maximum TTL allowance to prevent reaching crazy timeouts

	qosTuningPeers   = 5    // Number of peers to tune based on (best peers)
	qosConfidenceCap = 10   // Number of peers above which not to modify RTT confidence
	qosTuningImpact  = 0.25 // Impact that a new tuning target has on the previous value

	maxQueuedHeaders  = 32 * 1024 // [eth/62] Maximum number of headers to queue for import (DOS protection)
	maxHeadersProcess = 2048      // Number of header download results to import at once into the chain
	maxResultsProcess = 2048      // Number of content download results to import at once into the chain

	fsHeaderCheckFrequency = 100             // Verification frequency of the downloaded headers during fast sync
	fsHeaderSafetyNet      = 2048            // Number of headers to discard in case a chain violation is detected
	fsHeaderForceVerify    = 24              // Number of headers to verify before and after the pivot to accept it
	fsHeaderContCheck      = 3 * time.Second // Time interval to check for header continuations during state download
	fsMinFullBlocks        = 64              // Number of blocks to retrieve fully even in fast sync
)

var (
	errBusy                    = errors.New("busy")
	errUnknownPeer             = errors.New("peer is unknown or unhealthy")
	errBadPeer                 = errors.New("action from bad peer ignored")
	errStallingPeer            = errors.New("peer is stalling")
	errNoPeers                 = errors.New("no peers to keep download active")
	errTimeout                 = errors.New("timeout")
	errEmptyHeaderSet          = errors.New("empty header set by peer")
	errPeersUnavailable        = errors.New("no peers available or all tried for download")
	errInvalidAncestor         = errors.New("retrieved ancestor is invalid")
	errInvalidChain            = errors.New("retrieved hash chain is invalid")
	errInvalidBlock            = errors.New("retrieved block is invalid")
	errInvalidBody             = errors.New("retrieved block body is invalid")
	errInvalidReceipt          = errors.New("retrieved receipt is invalid")
	errCancelBlockFetch        = errors.New("block download canceled (requested)")
	errCancelHeaderFetch       = errors.New("block header download canceled (requested)")
	errCancelBodyFetch         = errors.New("block body download canceled (requested)")
	errCancelReceiptFetch      = errors.New("receipt download canceled (requested)")
	errCancelStateFetch        = errors.New("state data download canceled (requested)")
	errCancelHeaderProcessing  = errors.New("header processing canceled (requested)")
	errCancelContentProcessing = errors.New("content processing canceled (requested)")
	errNoSyncActive            = errors.New("no sync active")
	errTooOld                  = errors.New("peer doesn't speak recent enough protocol version (need version >= 62)")
)

type Downloader struct {
	mode SyncMode       // Synchronisation mode defining the strategy used (per sync cycle)
	mux  *event.TypeMux // Event multiplexer to announce sync operation events

	queue   *queue   // Scheduler for selecting the hashes to download
	peers   *peerSet // Set of active peers from which download can proceed
	stateDB candb.Database

	rttEstimate   uint64 // Round trip time to target for download requests
	rttConfidence uint64 // Confidence in the estimated RTT (unit: millionths to allow atomic ops)

	// Statistics
	syncStatsChainOrigin uint64 // Origin block number where syncing started at
	syncStatsChainHeight uint64 // Highest block number known when syncing started
	syncStatsState       stateSyncStats
	syncStatsLock        sync.RWMutex // Lock protecting the sync stats fields

	lightchain LightChain
	blockchain BlockChain

	// Callbacks
	dropPeer peerDropFn // Drops a peer for misbehaving

	// Status
	synchroniseMock func(id string, hash common.Hash) error // Replacement for synchronise during testing
	synchronising   int32
	notified        int32
	committed       int32

	// Channels
	headerCh      chan dataPack        // [eth/62] Channel receiving inbound block headers
	bodyCh        chan dataPack        // [eth/62] Channel receiving inbound block bodies
	receiptCh     chan dataPack        // [eth/63] Channel receiving inbound receipts
	bodyWakeCh    chan bool            // [eth/62] Channel to signal the block body fetcher of new tasks
	receiptWakeCh chan bool            // [eth/63] Channel to signal the receipt fetcher of new tasks
	headerProcCh  chan []*types.Header // [eth/62] Channel to feed the header processor new tasks

	// for stateFetcher
	stateSyncStart chan *stateSync
	trackStateReq  chan *stateReq
	stateCh        chan dataPack // [eth/63] Channel receiving inbound node state data

	// Cancellation and termination
	cancelPeer string         // Identifier of the peer currently being used as the master (cancel on drop)
	cancelCh   chan struct{}  // Channel to cancel mid-flight syncs
	cancelLock sync.RWMutex   // Lock to protect the cancel channel and peer in delivers
	cancelWg   sync.WaitGroup // Make sure all fetcher goroutines have exited.

	quitCh   chan struct{} // Quit channel to signal termination
	quitLock sync.RWMutex  // Lock to prevent double closes

	// Testing hooks
	syncInitHook     func(uint64, uint64)  // Method to call upon initiating a new sync run
	bodyFetchHook    func([]*types.Header) // Method to call upon starting a block body fetch
	receiptFetchHook func([]*types.Header) // Method to call upon starting a receipt fetch
	chainInsertHook  func([]*fetchResult)  // Method to call upon inserting a chain of blocks (possibly in multiple invocations)
}

type LightChain interface {
	// HasHeader verifies a header's presence in the local chain.
	HasHeader(common.Hash, uint64) bool

	// GetHeaderByHash retrieves a header from the local chain.
	GetHeaderByHash(common.Hash) *types.Header

	// CurrentHeader retrieves the head header from the local chain.
	CurrentHeader() *types.Header

	// GetTd returns the total difficulty of a local block.
	GetTd(common.Hash, uint64) *big.Int

	// InsertHeaderChain inserts a batch of headers into the local chain.
	InsertHeaderChain([]*types.Header, int) (int, error)

	// Rollback removes a few recently added elements from the local chain.
	Rollback([]common.Hash)
}

type BlockChain interface {
	LightChain

	// HasBlock verifies a block's presence in the local chain.
	HasBlock(common.Hash, uint64) bool

	// GetBlockByHash retrieves a block from the local chain.
	GetBlockByHash(common.Hash) *types.Block

	// CurrentBlock retrieves the head block from the local chain.
	CurrentBlock() *types.Block

	// CurrentFastBlock retrieves the head fast block from the local chain.
	CurrentFastBlock() *types.Block

	// FastSyncCommitHead directly commits the head block to a certain entity.
	FastSyncCommitHead(common.Hash) error

	// InsertChain inserts a batch of blocks into the local chain.
	InsertChain(types.Blocks) (int, error)

	// InsertReceiptChain inserts a batch of receipts into the local chain.
	InsertReceiptChain(types.Blocks, []types.Receipts) (int, error)
}

func New(mode SyncMode, stateDb candb.Database, mux *event.TypeMux, chain BlockChain, lightchain LightChain, dropPeer peerDropFn) *Downloader {
	if lightchain == nil {
		lightchain = chain
	}

	dl := &Downloader{
		mode:           mode,
		stateDB:        stateDb,
		mux:            mux,
		queue:          newQueue(),
		peers:          newPeerSet(),
		rttEstimate:    uint64(rttMaxEstimate),
		rttConfidence:  uint64(1000000),
		blockchain:     chain,
		lightchain:     lightchain,
		dropPeer:       dropPeer,
		headerCh:       make(chan dataPack, 1),
		bodyCh:         make(chan dataPack, 1),
		receiptCh:      make(chan dataPack, 1),
		bodyWakeCh:     make(chan bool, 1),
		receiptWakeCh:  make(chan bool, 1),
		headerProcCh:   make(chan []*types.Header, 1),
		quitCh:         make(chan struct{}),
		stateCh:        make(chan dataPack),
		stateSyncStart: make(chan *stateSync),
		syncStatsState: stateSyncStats{
			processed: rawdb.ReadFastTrieProgress(stateDb),
		},
		trackStateReq: make(chan *stateReq),
	}
	go dl.qosTuner()
	go dl.stateFetcher()
	return dl
}

//
func (d *Downloader) Progress() ethereum.SyncProgress {
	// Lock the current stats and return the progress
	d.syncStatsLock.RLock()
	defer d.syncStatsLock.RUnlock()

	current := uint64(0)
	switch d.mode {
	case FullSync:
		current = d.blockchain.CurrentBlock().NumberU64()
	case FastSync:
		current = d.blockchain.CurrentFastBlock().NumberU64()
	case LightSync:
		current = d.lightchain.CurrentHeader().Number.Uint64()
	}
	return ethereum.SyncProgress{
		StartingBlock: d.syncStatsChainOrigin,
		CurrentBlock:  current,
		HighestBlock:  d.syncStatsChainHeight,
		PulledStates:  d.syncStatsState.processed,
		KnownStates:   d.syncStatsState.processed + d.syncStatsState.pending,
	}
}

func (d *Downloader) Synchronising() bool {
	return atomic.LoadInt32(&d.synchronising) > 0
}

func (d *Downloader) RegisterPeer(id string, version int, peer Peer) error {
	logger := log4j.New("peer", id)
	logger.Trace("Registering sync peer")
	if err := d.peers.Register(newPeerConnection(id, version, peer, logger)); err != nil {
		logger.Error("Failed to register sync peer", "err", err)
		return err
	}
	d.qosReduceConfidence()

	return nil
}

func (d *Downloader) RegisterLightPeer(id string, version int, peer LightPeer) error {
	return d.RegisterPeer(id, version, &lightPeerWrapper{peer})
}

func (d *Downloader) UnregisterPeer(id string) error {
	// Unregister the peer from the active peer set and revoke any fetch tasks
	logger := log4j.New("peer", id)
	logger.Trace("Unregistering sync peer")
	if err := d.peers.Unregister(id); err != nil {
		logger.Error("Failed to unregister sync peer", "err", err)
		return err
	}
	d.queue.Revoke(id)

	// If this peer was the master peer, abort sync immediately
	d.cancelLock.RLock()
	master := id == d.cancelPeer
	d.cancelLock.RUnlock()

	if master {
		d.cancel()
	}
	return nil
}

func (d *Downloader) Synchronise(id string, head common.Hash, td *big.Int, mode SyncMode) error {
	err := d.synchronise(id, head, td, mode)
	switch err {
	case nil:
	case errBusy:

	case errTimeout, errBadPeer, errStallingPeer,
		errEmptyHeaderSet, errPeersUnavailable, errTooOld,
		errInvalidAncestor, errInvalidChain:
		log4j.Warn("Synchronisation failed, dropping peer", "peer", id, "err", err)
		if d.dropPeer == nil {
			// The dropPeer method is nil when `--copydb` is used for a local copy.
			// Timeouts can occur if e.g. compaction hits at the wrong time, and can be ignored
			log4j.Warn("Downloader wants to drop peer, but peerdrop-function is not set", "peer", id)
		} else {
			d.dropPeer(id)
		}
	default:
		log4j.Warn("Synchronisation failed, retrying", "err", err)
	}
	return err
}

func (d *Downloader) synchronise(id string, hash common.Hash, td *big.Int, mode SyncMode) error {
	// Mock out the synchronisation if testing
	if d.synchroniseMock != nil {
		return d.synchroniseMock(id, hash)
	}
	// Make sure only one goroutine is ever allowed past this point at once
	if !atomic.CompareAndSwapInt32(&d.synchronising, 0, 1) {
		return errBusy
	}
	defer atomic.StoreInt32(&d.synchronising, 0)

	// Post a user notification of the sync (only once per session)
	if atomic.CompareAndSwapInt32(&d.notified, 0, 1) {
		log4j.Info("Block synchronisation started")
	}
	// Reset the queue, peer set and wake channels to clean any internal leftover state
	d.queue.Reset()
	d.peers.Reset()

	for _, ch := range []chan bool{d.bodyWakeCh, d.receiptWakeCh} {
		select {
		case <-ch:
		default:
		}
	}
	for _, ch := range []chan dataPack{d.headerCh, d.bodyCh, d.receiptCh} {
		for empty := false; !empty; {
			select {
			case <-ch:
			default:
				empty = true
			}
		}
	}
	for empty := false; !empty; {
		select {
		case <-d.headerProcCh:
		default:
			empty = true
		}
	}
	// Create cancel channel for aborting mid-flight and mark the master peer
	d.cancelLock.Lock()
	d.cancelCh = make(chan struct{})
	d.cancelPeer = id
	d.cancelLock.Unlock()

	defer d.Cancel() // No matter what, we can't leave the cancel channel open

	// Set the requested sync mode, unless it's forbidden
	d.mode = mode

	// Retrieve the origin peer and initiate the downloading process
	p := d.peers.Peer(id)
	if p == nil {
		return errUnknownPeer
	}
	return d.syncWithPeer(p, hash, td)
}

func (d *Downloader) syncWithPeer(p *peerConnection, hash common.Hash, td *big.Int) (err error) {
	d.mux.Post(StartEvent{})
	defer func() {
		// reset on error
		if err != nil {
			d.mux.Post(FailedEvent{err})
		} else {
			d.mux.Post(DoneEvent{})
		}
	}()
	if p.version < 62 {
		return errTooOld
	}

	log4j.Debug("Synchronising with the network", "peer", p.id, "eth", p.version, "head", hash, "td", td, "mode", d.mode)
	defer func(start time.Time) {
		log4j.Debug("Synchronisation terminated", "elapsed", time.Since(start))
	}(time.Now())

	// Look up the sync boundaries: the common ancestor and the target block
	latest, err := d.fetchHeight(p)
	if err != nil {
		return err
	}
	height := latest.Number.Uint64()

	origin, err := d.findAncestor(p, height)
	if err != nil {
		return err
	}
	d.syncStatsLock.Lock()
	if d.syncStatsChainHeight <= origin || d.syncStatsChainOrigin > origin {
		d.syncStatsChainOrigin = origin
	}
	d.syncStatsChainHeight = height
	d.syncStatsLock.Unlock()

	// Ensure our origin point is below any fast sync pivot point
	pivot := uint64(0)
	if d.mode == FastSync {
		if height <= uint64(fsMinFullBlocks) {
			origin = 0
		} else {
			pivot = height - uint64(fsMinFullBlocks)
			if pivot <= origin {
				origin = pivot - 1
			}
		}
	}
	d.committed = 1
	if d.mode == FastSync && pivot != 0 {
		d.committed = 0
	}
	// Initiate the sync using a concurrent header and content retrieval algorithm
	d.queue.Prepare(origin+1, d.mode)
	if d.syncInitHook != nil {
		d.syncInitHook(origin, height)
	}

	fetchers := []func() error{
		func() error { return d.fetchHeaders(p, origin+1, pivot) }, // Headers are always retrieved
		func() error { return d.fetchBodies(origin + 1) },          // Bodies are retrieved during normal and fast sync
		func() error { return d.fetchReceipts(origin + 1) },        // Receipts are retrieved during fast sync
		func() error { return d.processHeaders(origin+1, pivot, td) },
	}
	if d.mode == FastSync {
		fetchers = append(fetchers, func() error { return d.processFastSyncContent(latest) })
	} else if d.mode == FullSync {
		fetchers = append(fetchers, d.processFullSyncContent)
	}
	return d.spawnSync(fetchers)
}

func (d *Downloader) spawnSync(fetchers []func() error) error {
	errc := make(chan error, len(fetchers))
	d.cancelWg.Add(len(fetchers))
	for _, fn := range fetchers {
		fn := fn
		go func() { defer d.cancelWg.Done(); errc <- fn() }()
	}
	// Wait for the first error, then terminate the others.
	var err error
	for i := 0; i < len(fetchers); i++ {
		if i == len(fetchers)-1 {
			// Close the queue when all fetchers have exited.
			// This will cause the block processor to end when
			// it has processed the queue.
			d.queue.Close()
		}
		if err = <-errc; err != nil {
			break
		}
	}
	d.queue.Close()
	d.Cancel()
	return err
}

func (d *Downloader) cancel() {
	// Close the current cancel channel
	d.cancelLock.Lock()
	if d.cancelCh != nil {
		select {
		case <-d.cancelCh:
			// Channel was already closed
		default:
			close(d.cancelCh)
		}
	}
	d.cancelLock.Unlock()
}

func (d *Downloader) Cancel() {
	d.cancel()
	d.cancelWg.Wait()
}

func (d *Downloader) Terminate() {
	// Close the termination channel (make sure double close is allowed)
	d.quitLock.Lock()
	select {
	case <-d.quitCh:
	default:
		close(d.quitCh)
	}
	d.quitLock.Unlock()

	// Cancel any pending download requests
	d.Cancel()
}

func (d *Downloader) fetchHeight(p *peerConnection) (*types.Header, error) {
	p.log.Debug("Retrieving remote chain height")

	// Request the advertised remote head block and wait for the response
	head, _ := p.peer.Head()
	go p.peer.RequestHeadersByHash(head, 1, 0, false)

	ttl := d.requestTTL()
	timeout := time.After(ttl)
	for {
		select {
		case <-d.cancelCh:
			return nil, errCancelBlockFetch

		case packet := <-d.headerCh:
			// Discard anything not from the origin peer
			if packet.PeerId() != p.id {
				log4j.Debug("Received headers from incorrect peer", "peer", packet.PeerId())
				break
			}
			// Make sure the peer actually gave something valid
			headers := packet.(*headerPack).headers
			if len(headers) != 1 {
				p.log.Debug("Multiple headers for single request", "headers", len(headers))
				return nil, errBadPeer
			}
			head := headers[0]
			p.log.Debug("Remote head header identified", "number", head.Number, "hash", head.Hash())
			return head, nil

		case <-timeout:
			p.log.Debug("Waiting for head header timed out", "elapsed", ttl)
			return nil, errTimeout

		case <-d.bodyCh:
		case <-d.receiptCh:
			// Out of bounds delivery, ignore
		}
	}
}

func (d *Downloader) findAncestor(p *peerConnection, height uint64) (uint64, error) {
	// Figure out the valid ancestor range to prevent rewrite attacks
	floor, ceil := int64(-1), d.lightchain.CurrentHeader().Number.Uint64()

	if d.mode == FullSync {
		ceil = d.blockchain.CurrentBlock().NumberU64()
	} else if d.mode == FastSync {
		ceil = d.blockchain.CurrentFastBlock().NumberU64()
	}
	if ceil >= MaxForkAncestry {
		floor = int64(ceil - MaxForkAncestry)
	}
	p.log.Debug("Looking for common ancestor", "local", ceil, "remote", height)

	// Request the topmost blocks to short circuit binary ancestor lookup
	head := ceil
	if head > height {
		head = height
	}
	from := int64(head) - int64(MaxHeaderFetch)
	if from < 0 {
		from = 0
	}
	// Span out with 15 block gaps into the future to catch bad head reports
	limit := 2 * MaxHeaderFetch / 16
	count := 1 + int((int64(ceil)-from)/16)
	if count > limit {
		count = limit
	}
	go p.peer.RequestHeadersByNumber(uint64(from), count, 15, false)

	// Wait for the remote response to the head fetch
	number, hash := uint64(0), common.Hash{}

	ttl := d.requestTTL()
	timeout := time.After(ttl)

	for finished := false; !finished; {
		select {
		case <-d.cancelCh:
			return 0, errCancelHeaderFetch

		case packet := <-d.headerCh:
			// Discard anything not from the origin peer
			if packet.PeerId() != p.id {
				log4j.Debug("Received headers from incorrect peer", "peer", packet.PeerId())
				break
			}
			// Make sure the peer actually gave something valid
			headers := packet.(*headerPack).headers
			if len(headers) == 0 {
				p.log.Warn("Empty head header set")
				return 0, errEmptyHeaderSet
			}
			// Make sure the peer's reply conforms to the request
			for i := 0; i < len(headers); i++ {
				if number := headers[i].Number.Int64(); number != from+int64(i)*16 {
					p.log.Warn("Head headers broke chain ordering", "index", i, "requested", from+int64(i)*16, "received", number)
					return 0, errInvalidChain
				}
			}
			// Check if a common ancestor was found
			finished = true
			for i := len(headers) - 1; i >= 0; i-- {
				// Skip any headers that underflow/overflow our requested set
				if headers[i].Number.Int64() < from || headers[i].Number.Uint64() > ceil {
					continue
				}
				// Otherwise check if we already know the header or not
				if (d.mode == FullSync && d.blockchain.HasBlock(headers[i].Hash(), headers[i].Number.Uint64())) || (d.mode != FullSync && d.lightchain.HasHeader(headers[i].Hash(), headers[i].Number.Uint64())) {
					number, hash = headers[i].Number.Uint64(), headers[i].Hash()

					// If every header is known, even future ones, the peer straight out lied about its head
					if number > height && i == limit-1 {
						p.log.Warn("Lied about chain head", "reported", height, "found", number)
						return 0, errStallingPeer
					}
					break
				}
			}

		case <-timeout:
			p.log.Debug("Waiting for head header timed out", "elapsed", ttl)
			return 0, errTimeout

		case <-d.bodyCh:
		case <-d.receiptCh:
			// Out of bounds delivery, ignore
		}
	}
	// If the head fetch already found an ancestor, return
	if hash != (common.Hash{}) {
		if int64(number) <= floor {
			p.log.Warn("Ancestor below allowance", "number", number, "hash", hash, "allowance", floor)
			return 0, errInvalidAncestor
		}
		p.log.Debug("Found common ancestor", "number", number, "hash", hash)
		return number, nil
	}
	// Ancestor not found, we need to binary search over our chain
	start, end := uint64(0), head
	if floor > 0 {
		start = uint64(floor)
	}
	for start+1 < end {
		// Split our chain interval in two, and request the hash to cross check
		check := (start + end) / 2

		ttl := d.requestTTL()
		timeout := time.After(ttl)

		go p.peer.RequestHeadersByNumber(check, 1, 0, false)

		// Wait until a reply arrives to this request
		for arrived := false; !arrived; {
			select {
			case <-d.cancelCh:
				return 0, errCancelHeaderFetch

			case packer := <-d.headerCh:
				// Discard anything not from the origin peer
				if packer.PeerId() != p.id {
					log4j.Debug("Received headers from incorrect peer", "peer", packer.PeerId())
					break
				}
				// Make sure the peer actually gave something valid
				headers := packer.(*headerPack).headers
				if len(headers) != 1 {
					p.log.Debug("Multiple headers for single request", "headers", len(headers))
					return 0, errBadPeer
				}
				arrived = true

				// Modify the search interval based on the response
				if (d.mode == FullSync && !d.blockchain.HasBlock(headers[0].Hash(), headers[0].Number.Uint64())) || (d.mode != FullSync && !d.lightchain.HasHeader(headers[0].Hash(), headers[0].Number.Uint64())) {
					end = check
					break
				}
				header := d.lightchain.GetHeaderByHash(headers[0].Hash()) // Independent of sync mode, header surely exists
				if header.Number.Uint64() != check {
					p.log.Debug("Received non requested header", "number", header.Number, "hash", header.Hash(), "request", check)
					return 0, errBadPeer
				}
				start = check

			case <-timeout:
				p.log.Debug("Waiting for search header timed out", "elapsed", ttl)
				return 0, errTimeout

			case <-d.bodyCh:
			case <-d.receiptCh:
				// Out of bounds delivery, ignore
			}
		}
	}
	// Ensure valid ancestry and return
	if int64(start) <= floor {
		p.log.Warn("Ancestor below allowance", "number", start, "hash", hash, "allowance", floor)
		return 0, errInvalidAncestor
	}
	p.log.Debug("Found common ancestor", "number", start, "hash", hash)
	return start, nil
}

func (d *Downloader) fetchHeaders(p *peerConnection, from uint64, pivot uint64) error {
	p.log.Debug("Directing header downloads", "origin", from)
	defer p.log.Debug("Header download terminated")

	// Create a timeout timer, and the associated header fetcher
	skeleton := true            // Skeleton assembly phase or finishing up
	request := time.Now()       // time of the last skeleton fetch request
	timeout := time.NewTimer(0) // timer to dump a non-responsive active peer
	<-timeout.C                 // timeout channel should be initially empty
	defer timeout.Stop()

	var ttl time.Duration
	getHeaders := func(from uint64) {
		request = time.Now()

		ttl = d.requestTTL()
		timeout.Reset(ttl)

		if skeleton {
			p.log.Trace("Fetching skeleton headers", "count", MaxHeaderFetch, "from", from)
			go p.peer.RequestHeadersByNumber(from+uint64(MaxHeaderFetch)-1, MaxSkeletonSize, MaxHeaderFetch-1, false)
		} else {
			p.log.Trace("Fetching full headers", "count", MaxHeaderFetch, "from", from)
			go p.peer.RequestHeadersByNumber(from, MaxHeaderFetch, 0, false)
		}
	}
	// Start pulling the header chain skeleton until all is done
	getHeaders(from)

	for {
		select {
		case <-d.cancelCh:
			return errCancelHeaderFetch

		case packet := <-d.headerCh:
			// Make sure the active peer is giving us the skeleton headers
			if packet.PeerId() != p.id {
				log4j.Debug("Received skeleton from incorrect peer", "peer", packet.PeerId())
				break
			}
			headerReqTimer.UpdateSince(request)
			timeout.Stop()

			// If the skeleton's finished, pull any remaining head headers directly from the origin
			if packet.Items() == 0 && skeleton {
				skeleton = false
				getHeaders(from)
				continue
			}
			// If no more headers are inbound, notify the content fetchers and return
			if packet.Items() == 0 {
				// Don't abort header fetches while the pivot is downloading
				if atomic.LoadInt32(&d.committed) == 0 && pivot <= from {
					p.log.Debug("No headers, waiting for pivot commit")
					select {
					case <-time.After(fsHeaderContCheck):
						getHeaders(from)
						continue
					case <-d.cancelCh:
						return errCancelHeaderFetch
					}
				}
				// Pivot done (or not in fast sync) and no more headers, terminate the process
				p.log.Debug("No more headers available")
				select {
				case d.headerProcCh <- nil:
					return nil
				case <-d.cancelCh:
					return errCancelHeaderFetch
				}
			}
			headers := packet.(*headerPack).headers

			// If we received a skeleton batch, resolve internals concurrently
			if skeleton {
				filled, proced, err := d.fillHeaderSkeleton(from, headers)
				if err != nil {
					p.log.Debug("Skeleton chain invalid", "err", err)
					return errInvalidChain
				}
				headers = filled[proced:]
				from += uint64(proced)
			}
			// Insert all the new headers and fetch the next batch
			if len(headers) > 0 {
				p.log.Trace("Scheduling new headers", "count", len(headers), "from", from)
				select {
				case d.headerProcCh <- headers:
				case <-d.cancelCh:
					return errCancelHeaderFetch
				}
				from += uint64(len(headers))
			}
			getHeaders(from)

		case <-timeout.C:
			if d.dropPeer == nil {
				// The dropPeer method is nil when `--copydb` is used for a local copy.
				// Timeouts can occur if e.g. compaction hits at the wrong time, and can be ignored
				p.log.Warn("Downloader wants to drop peer, but peerdrop-function is not set", "peer", p.id)
				break
			}
			// Header retrieval timed out, consider the peer bad and drop
			p.log.Debug("Header request timed out", "elapsed", ttl)
			headerTimeoutMeter.Mark(1)
			d.dropPeer(p.id)

			// Finish the sync gracefully instead of dumping the gathered data though
			for _, ch := range []chan bool{d.bodyWakeCh, d.receiptWakeCh} {
				select {
				case ch <- false:
				case <-d.cancelCh:
				}
			}
			select {
			case d.headerProcCh <- nil:
			case <-d.cancelCh:
			}
			return errBadPeer
		}
	}
}

//
//
func (d *Downloader) fillHeaderSkeleton(from uint64, skeleton []*types.Header) ([]*types.Header, int, error) {
	log4j.Debug("Filling up skeleton", "from", from)
	d.queue.ScheduleSkeleton(from, skeleton)

	var (
		deliver = func(packet dataPack) (int, error) {
			pack := packet.(*headerPack)
			return d.queue.DeliverHeaders(pack.peerID, pack.headers, d.headerProcCh)
		}
		expire   = func() map[string]int { return d.queue.ExpireHeaders(d.requestTTL()) }
		throttle = func() bool { return false }
		reserve  = func(p *peerConnection, count int) (*fetchRequest, bool, error) {
			return d.queue.ReserveHeaders(p, count), false, nil
		}
		fetch    = func(p *peerConnection, req *fetchRequest) error { return p.FetchHeaders(req.From, MaxHeaderFetch) }
		capacity = func(p *peerConnection) int { return p.HeaderCapacity(d.requestRTT()) }
		setIdle  = func(p *peerConnection, accepted int) { p.SetHeadersIdle(accepted) }
	)
	err := d.fetchParts(errCancelHeaderFetch, d.headerCh, deliver, d.queue.headerContCh, expire,
		d.queue.PendingHeaders, d.queue.InFlightHeaders, throttle, reserve,
		nil, fetch, d.queue.CancelHeaders, capacity, d.peers.HeaderIdlePeers, setIdle, "headers")

	log4j.Debug("Skeleton fill terminated", "err", err)

	filled, proced := d.queue.RetrieveHeaders()
	return filled, proced, err
}

func (d *Downloader) fetchBodies(from uint64) error {
	log4j.Debug("Downloading block bodies", "origin", from)

	var (
		deliver = func(packet dataPack) (int, error) {
			pack := packet.(*bodyPack)
			return d.queue.DeliverBodies(pack.peerID, pack.transactions, pack.uncles)
		}
		expire   = func() map[string]int { return d.queue.ExpireBodies(d.requestTTL()) }
		fetch    = func(p *peerConnection, req *fetchRequest) error { return p.FetchBodies(req) }
		capacity = func(p *peerConnection) int { return p.BlockCapacity(d.requestRTT()) }
		setIdle  = func(p *peerConnection, accepted int) { p.SetBodiesIdle(accepted) }
	)
	err := d.fetchParts(errCancelBodyFetch, d.bodyCh, deliver, d.bodyWakeCh, expire,
		d.queue.PendingBlocks, d.queue.InFlightBlocks, d.queue.ShouldThrottleBlocks, d.queue.ReserveBodies,
		d.bodyFetchHook, fetch, d.queue.CancelBodies, capacity, d.peers.BodyIdlePeers, setIdle, "bodies")

	log4j.Debug("Block body download terminated", "err", err)
	return err
}

func (d *Downloader) fetchReceipts(from uint64) error {
	log4j.Debug("Downloading transaction receipts", "origin", from)

	var (
		deliver = func(packet dataPack) (int, error) {
			pack := packet.(*receiptPack)
			return d.queue.DeliverReceipts(pack.peerID, pack.receipts)
		}
		expire   = func() map[string]int { return d.queue.ExpireReceipts(d.requestTTL()) }
		fetch    = func(p *peerConnection, req *fetchRequest) error { return p.FetchReceipts(req) }
		capacity = func(p *peerConnection) int { return p.ReceiptCapacity(d.requestRTT()) }
		setIdle  = func(p *peerConnection, accepted int) { p.SetReceiptsIdle(accepted) }
	)
	err := d.fetchParts(errCancelReceiptFetch, d.receiptCh, deliver, d.receiptWakeCh, expire,
		d.queue.PendingReceipts, d.queue.InFlightReceipts, d.queue.ShouldThrottleReceipts, d.queue.ReserveReceipts,
		d.receiptFetchHook, fetch, d.queue.CancelReceipts, capacity, d.peers.ReceiptIdlePeers, setIdle, "receipts")

	log4j.Debug("Transaction receipt download terminated", "err", err)
	return err
}

//
//
//  - errCancel:   error type to return if the fetch operation is cancelled (mostly makes logging nicer)
//  - deliveryCh:  channel from which to retrieve downloaded data packets (merged from all concurrent peers)
//  - deliver:     processing callback to deliver data packets into type specific download queues (usually within `queue`)
//  - wakeCh:      notification channel for waking the fetcher when new tasks are available (or sync completed)
//  - expire:      task callback method to abort requests that took too long and return the faulty peers (traffic shaping)
//  - pending:     task callback for the number of requests still needing download (detect completion/non-completability)
//  - inFlight:    task callback for the number of in-progress requests (wait for all active downloads to finish)
//  - throttle:    task callback to check if the processing queue is full and activate throttling (bound memory use)
//  - reserve:     task callback to reserve new download tasks to a particular peer (also signals partial completions)
//  - fetchHook:   tester callback to notify of new tasks being initiated (allows testing the scheduling logic)
//  - fetch:       network callback to actually send a particular download request to a physical remote peer
//  - cancel:      task callback to abort an in-flight download request and allow rescheduling it (in case of lost peer)
//  - capacity:    network callback to retrieve the estimated type-specific bandwidth capacity of a peer (traffic shaping)
//  - idle:        network callback to retrieve the currently (type specific) idle peers that can be assigned tasks
//  - setIdle:     network callback to set a peer back to idle and update its estimated capacity (traffic shaping)
//  - kind:        textual label of the type being downloaded to display in log mesages
func (d *Downloader) fetchParts(errCancel error, deliveryCh chan dataPack, deliver func(dataPack) (int, error), wakeCh chan bool,
	expire func() map[string]int, pending func() int, inFlight func() bool, throttle func() bool, reserve func(*peerConnection, int) (*fetchRequest, bool, error),
	fetchHook func([]*types.Header), fetch func(*peerConnection, *fetchRequest) error, cancel func(*fetchRequest), capacity func(*peerConnection) int,
	idle func() ([]*peerConnection, int), setIdle func(*peerConnection, int), kind string) error {

	// Create a ticker to detect expired retrieval tasks
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	update := make(chan struct{}, 1)

	// Prepare the queue and fetch block parts until the block header fetcher's done
	finished := false
	for {
		select {
		case <-d.cancelCh:
			return errCancel

		case packet := <-deliveryCh:
			// If the peer was previously banned and failed to deliver its pack
			// in a reasonable time frame, ignore its message.
			if peer := d.peers.Peer(packet.PeerId()); peer != nil {
				// Deliver the received chunk of data and check chain validity
				accepted, err := deliver(packet)
				if err == errInvalidChain {
					return err
				}
				// Unless a peer delivered something completely else than requested (usually
				// caused by a timed out request which came through in the end), set it to
				// idle. If the delivery's stale, the peer should have already been idled.
				if err != errStaleDelivery {
					setIdle(peer, accepted)
				}
				// Issue a log to the user to see what's going on
				switch {
				case err == nil && packet.Items() == 0:
					peer.log.Trace("Requested data not delivered", "type", kind)
				case err == nil:
					peer.log.Trace("Delivered new batch of data", "type", kind, "count", packet.Stats())
				default:
					peer.log.Trace("Failed to deliver retrieved data", "type", kind, "err", err)
				}
			}
			// Blocks assembled, try to update the progress
			select {
			case update <- struct{}{}:
			default:
			}

		case cont := <-wakeCh:
			// The header fetcher sent a continuation flag, check if it's done
			if !cont {
				finished = true
			}
			// Headers arrive, try to update the progress
			select {
			case update <- struct{}{}:
			default:
			}

		case <-ticker.C:
			// Sanity check update the progress
			select {
			case update <- struct{}{}:
			default:
			}

		case <-update:
			// Short circuit if we lost all our peers
			if d.peers.Len() == 0 {
				return errNoPeers
			}
			// Check for fetch request timeouts and demote the responsible peers
			for pid, fails := range expire() {
				if peer := d.peers.Peer(pid); peer != nil {
					// If a lot of retrieval elements expired, we might have overestimated the remote peer or perhaps
					// ourselves. Only reset to minimal throughput but don't drop just yet. If even the minimal times
					// out that sync wise we need to get rid of the peer.
					//
					// The reason the minimum threshold is 2 is because the downloader tries to estimate the bandwidth
					// and latency of a peer separately, which requires pushing the measures capacity a bit and seeing
					// how response times reacts, to it always requests one more than the minimum (i.e. min 2).
					if fails > 2 {
						peer.log.Trace("Data delivery timed out", "type", kind)
						setIdle(peer, 0)
					} else {
						peer.log.Debug("Stalling delivery, dropping", "type", kind)
						if d.dropPeer == nil {
							// The dropPeer method is nil when `--copydb` is used for a local copy.
							// Timeouts can occur if e.g. compaction hits at the wrong time, and can be ignored
							peer.log.Warn("Downloader wants to drop peer, but peerdrop-function is not set", "peer", pid)
						} else {
							d.dropPeer(pid)
						}
					}
				}
			}
			// If there's nothing more to fetch, wait or terminate
			if pending() == 0 {
				if !inFlight() && finished {
					log4j.Debug("Data fetching completed", "type", kind)
					return nil
				}
				break
			}
			// Send a download request to all idle peers, until throttled
			progressed, throttled, running := false, false, inFlight()
			idles, total := idle()

			for _, peer := range idles {
				// Short circuit if throttling activated
				if throttle() {
					throttled = true
					break
				}
				// Short circuit if there is no more available task.
				if pending() == 0 {
					break
				}
				// Reserve a chunk of fetches for a peer. A nil can mean either that
				// no more headers are available, or that the peer is known not to
				// have them.
				request, progress, err := reserve(peer, capacity(peer))
				if err != nil {
					return err
				}
				if progress {
					progressed = true
				}
				if request == nil {
					continue
				}
				if request.From > 0 {
					peer.log.Trace("Requesting new batch of data", "type", kind, "from", request.From)
				} else {
					peer.log.Trace("Requesting new batch of data", "type", kind, "count", len(request.Headers), "from", request.Headers[0].Number)
				}
				// Fetch the chunk and make sure any errors return the hashes to the queue
				if fetchHook != nil {
					fetchHook(request.Headers)
				}
				if err := fetch(peer, request); err != nil {
					// Although we could try and make an attempt to fix this, this error really
					// means that we've double allocated a fetch task to a peer. If that is the
					// case, the internal state of the downloader and the queue is very wrong so
					// better hard crash and note the error instead of silently accumulating into
					// a much bigger issue.
					panic(fmt.Sprintf("%v: %s fetch assignment failed", peer, kind))
				}
				running = true
			}
			// Make sure that we have peers available for fetching. If all peers have been tried
			// and all failed throw an error
			if !progressed && !throttled && !running && len(idles) == total && pending() > 0 {
				return errPeersUnavailable
			}
		}
	}
}

func (d *Downloader) processHeaders(origin uint64, pivot uint64, td *big.Int) error {
	// Keep a count of uncertain headers to roll back
	rollback := []*types.Header{}
	defer func() {
		if len(rollback) > 0 {
			// Flatten the headers and roll them back
			hashes := make([]common.Hash, len(rollback))
			for i, header := range rollback {
				hashes[i] = header.Hash()
			}
			lastHeader, lastFastBlock, lastBlock := d.lightchain.CurrentHeader().Number, common.Big0, common.Big0
			if d.mode != LightSync {
				lastFastBlock = d.blockchain.CurrentFastBlock().Number()
				lastBlock = d.blockchain.CurrentBlock().Number()
			}
			d.lightchain.Rollback(hashes)
			curFastBlock, curBlock := common.Big0, common.Big0
			if d.mode != LightSync {
				curFastBlock = d.blockchain.CurrentFastBlock().Number()
				curBlock = d.blockchain.CurrentBlock().Number()
			}
			log4j.Warn("Rolled back headers", "count", len(hashes),
				"header", fmt.Sprintf("%d->%d", lastHeader, d.lightchain.CurrentHeader().Number),
				"fast", fmt.Sprintf("%d->%d", lastFastBlock, curFastBlock),
				"block", fmt.Sprintf("%d->%d", lastBlock, curBlock))
		}
	}()

	// Wait for batches of headers to process
	gotHeaders := false

	for {
		select {
		case <-d.cancelCh:
			return errCancelHeaderProcessing

		case headers := <-d.headerProcCh:
			// Terminate header processing if we synced up
			if len(headers) == 0 {
				// Notify everyone that headers are fully processed
				for _, ch := range []chan bool{d.bodyWakeCh, d.receiptWakeCh} {
					select {
					case ch <- false:
					case <-d.cancelCh:
					}
				}
				// If no headers were retrieved at all, the peer violated its TD promise that it had a
				// better chain compared to ours. The only exception is if its promised blocks were
				// already imported by other means (e.g. fecher):
				//
				// R <remote peer>, L <local node>: Both at block 10
				// R: Mine block 11, and propagate it to L
				// L: Queue block 11 for import
				// L: Notice that R's head and TD increased compared to ours, start sync
				// L: Import of block 11 finishes
				// L: Sync begins, and finds common ancestor at 11
				// L: Request new headers up from 11 (R's TD was higher, it must have something)
				// R: Nothing to give
				if d.mode != LightSync {
					head := d.blockchain.CurrentBlock()
					if !gotHeaders && td.Cmp(d.blockchain.GetTd(head.Hash(), head.NumberU64())) > 0 {
						return errStallingPeer
					}
				}
				// If fast or light syncing, ensure promised headers are indeed delivered. This is
				// needed to detect scenarios where an attacker feeds a bad pivot and then bails out
				// of delivering the post-pivot blocks that would flag the invalid content.
				//
				// This check cannot be executed "as is" for full imports, since blocks may still be
				// queued for processing when the header download completes. However, as long as the
				// peer gave us something useful, we're already happy/progressed (above check).
				if d.mode == FastSync || d.mode == LightSync {
					head := d.lightchain.CurrentHeader()
					if td.Cmp(d.lightchain.GetTd(head.Hash(), head.Number.Uint64())) > 0 {
						return errStallingPeer
					}
				}
				// Disable any rollback and return
				rollback = nil
				return nil
			}
			// Otherwise split the chunk of headers into batches and process them
			gotHeaders = true

			for len(headers) > 0 {
				// Terminate if something failed in between processing chunks
				select {
				case <-d.cancelCh:
					return errCancelHeaderProcessing
				default:
				}
				// Select the next chunk of headers to import
				limit := maxHeadersProcess
				if limit > len(headers) {
					limit = len(headers)
				}
				chunk := headers[:limit]

				// In case of header only syncing, validate the chunk immediately
				if d.mode == FastSync || d.mode == LightSync {
					// Collect the yet unknown headers to mark them as uncertain
					unknown := make([]*types.Header, 0, len(headers))
					for _, header := range chunk {
						if !d.lightchain.HasHeader(header.Hash(), header.Number.Uint64()) {
							unknown = append(unknown, header)
						}
					}
					// If we're importing pure headers, verify based on their recentness
					frequency := fsHeaderCheckFrequency
					if chunk[len(chunk)-1].Number.Uint64()+uint64(fsHeaderForceVerify) > pivot {
						frequency = 1
					}
					if n, err := d.lightchain.InsertHeaderChain(chunk, frequency); err != nil {
						// If some headers were inserted, add them too to the rollback list
						if n > 0 {
							rollback = append(rollback, chunk[:n]...)
						}
						log4j.Debug("Invalid header encountered", "number", chunk[n].Number, "hash", chunk[n].Hash(), "err", err)
						return errInvalidChain
					}
					// All verifications passed, store newly found uncertain headers
					rollback = append(rollback, unknown...)
					if len(rollback) > fsHeaderSafetyNet {
						rollback = append(rollback[:0], rollback[len(rollback)-fsHeaderSafetyNet:]...)
					}
				}
				// Unless we're doing light chains, schedule the headers for associated content retrieval
				if d.mode == FullSync || d.mode == FastSync {
					// If we've reached the allowed number of pending headers, stall a bit
					for d.queue.PendingBlocks() >= maxQueuedHeaders || d.queue.PendingReceipts() >= maxQueuedHeaders {
						select {
						case <-d.cancelCh:
							return errCancelHeaderProcessing
						case <-time.After(time.Second):
						}
					}
					// Otherwise insert the headers for content retrieval
					inserts := d.queue.Schedule(chunk, origin)
					if len(inserts) != len(chunk) {
						log4j.Debug("Stale headers")
						return errBadPeer
					}
				}
				headers = headers[limit:]
				origin += uint64(limit)
			}

			// Update the highest block number we know if a higher one is found.
			d.syncStatsLock.Lock()
			if d.syncStatsChainHeight < origin {
				d.syncStatsChainHeight = origin - 1
			}
			d.syncStatsLock.Unlock()

			// Signal the content downloaders of the availablility of new tasks
			for _, ch := range []chan bool{d.bodyWakeCh, d.receiptWakeCh} {
				select {
				case ch <- true:
				default:
				}
			}
		}
	}
}

func (d *Downloader) processFullSyncContent() error {
	for {
		results := d.queue.Results(true)
		if len(results) == 0 {
			return nil
		}
		if d.chainInsertHook != nil {
			d.chainInsertHook(results)
		}
		if err := d.importBlockResults(results); err != nil {
			return err
		}
	}
}

func (d *Downloader) importBlockResults(results []*fetchResult) error {
	// Check for any early termination requests
	if len(results) == 0 {
		return nil
	}
	select {
	case <-d.quitCh:
		return errCancelContentProcessing
	default:
	}
	// Retrieve the a batch of results to import
	first, last := results[0].Header, results[len(results)-1].Header
	log4j.Debug("Inserting downloaded chain", "items", len(results),
		"firstnum", first.Number, "firsthash", first.Hash(),
		"lastnum", last.Number, "lasthash", last.Hash(),
	)
	blocks := make([]*types.Block, len(results))
	for i, result := range results {
		blocks[i] = types.NewBlockWithHeader(result.Header).WithBody(result.Transactions, result.Uncles)
	}
	if index, err := d.blockchain.InsertChain(blocks); err != nil {
		log4j.Debug("Downloaded item processing failed", "number", results[index].Header.Number, "hash", results[index].Header.Hash(), "err", err)
		return errInvalidChain
	}
	return nil
}

func (d *Downloader) processFastSyncContent(latest *types.Header) error {
	// Start syncing state of the reported head block. This should get us most of
	// the state of the pivot block.
	stateSync := d.syncState(latest.Root)
	defer stateSync.Cancel()
	go func() {
		if err := stateSync.Wait(); err != nil && err != errCancelStateFetch {
			d.queue.Close() // wake up WaitResults
		}
	}()
	// Figure out the ideal pivot block. Note, that this goalpost may move if the
	// sync takes long enough for the chain head to move significantly.
	pivot := uint64(0)
	if height := latest.Number.Uint64(); height > uint64(fsMinFullBlocks) {
		pivot = height - uint64(fsMinFullBlocks)
	}
	// To cater for moving pivot points, track the pivot block and subsequently
	// accumulated download results separately.
	var (
		oldPivot *fetchResult   // Locked in pivot block, might change eventually
		oldTail  []*fetchResult // Downloaded content after the pivot
	)
	for {
		// Wait for the next batch of downloaded data to be available, and if the pivot
		// block became stale, move the goalpost
		results := d.queue.Results(oldPivot == nil) // Block if we're not monitoring pivot staleness
		if len(results) == 0 {
			// If pivot sync is done, stop
			if oldPivot == nil {
				return stateSync.Cancel()
			}
			// If sync failed, stop
			select {
			case <-d.cancelCh:
				return stateSync.Cancel()
			default:
			}
		}
		if d.chainInsertHook != nil {
			d.chainInsertHook(results)
		}
		if oldPivot != nil {
			results = append(append([]*fetchResult{oldPivot}, oldTail...), results...)
		}
		// Split around the pivot block and process the two sides via fast/full sync
		if atomic.LoadInt32(&d.committed) == 0 {
			latest = results[len(results)-1].Header
			if height := latest.Number.Uint64(); height > pivot+2*uint64(fsMinFullBlocks) {
				log4j.Warn("Pivot became stale, moving", "old", pivot, "new", height-uint64(fsMinFullBlocks))
				pivot = height - uint64(fsMinFullBlocks)
			}
		}
		P, beforeP, afterP := splitAroundPivot(pivot, results)
		if err := d.commitFastSyncData(beforeP, stateSync); err != nil {
			return err
		}
		if P != nil {
			// If new pivot block found, cancel old state retrieval and restart
			if oldPivot != P {
				stateSync.Cancel()

				stateSync = d.syncState(P.Header.Root)
				defer stateSync.Cancel()
				go func() {
					if err := stateSync.Wait(); err != nil && err != errCancelStateFetch {
						d.queue.Close() // wake up WaitResults
					}
				}()
				oldPivot = P
			}
			// Wait for completion, occasionally checking for pivot staleness
			select {
			case <-stateSync.done:
				if stateSync.err != nil {
					return stateSync.err
				}
				if err := d.commitPivotBlock(P); err != nil {
					return err
				}
				oldPivot = nil

			case <-time.After(time.Second):
				oldTail = afterP
				continue
			}
		}
		// Fast sync done, pivot commit done, full import
		if err := d.importBlockResults(afterP); err != nil {
			return err
		}
	}
}

func splitAroundPivot(pivot uint64, results []*fetchResult) (p *fetchResult, before, after []*fetchResult) {
	for _, result := range results {
		num := result.Header.Number.Uint64()
		switch {
		case num < pivot:
			before = append(before, result)
		case num == pivot:
			p = result
		default:
			after = append(after, result)
		}
	}
	return p, before, after
}

func (d *Downloader) commitFastSyncData(results []*fetchResult, stateSync *stateSync) error {
	// Check for any early termination requests
	if len(results) == 0 {
		return nil
	}
	select {
	case <-d.quitCh:
		return errCancelContentProcessing
	case <-stateSync.done:
		if err := stateSync.Wait(); err != nil {
			return err
		}
	default:
	}
	// Retrieve the a batch of results to import
	first, last := results[0].Header, results[len(results)-1].Header
	log4j.Debug("Inserting fast-sync blocks", "items", len(results),
		"firstnum", first.Number, "firsthash", first.Hash(),
		"lastnumn", last.Number, "lasthash", last.Hash(),
	)
	blocks := make([]*types.Block, len(results))
	receipts := make([]types.Receipts, len(results))
	for i, result := range results {
		blocks[i] = types.NewBlockWithHeader(result.Header).WithBody(result.Transactions, result.Uncles)
		receipts[i] = result.Receipts
	}
	if index, err := d.blockchain.InsertReceiptChain(blocks, receipts); err != nil {
		log4j.Debug("Downloaded item processing failed", "number", results[index].Header.Number, "hash", results[index].Header.Hash(), "err", err)
		return errInvalidChain
	}
	return nil
}

func (d *Downloader) commitPivotBlock(result *fetchResult) error {
	block := types.NewBlockWithHeader(result.Header).WithBody(result.Transactions, result.Uncles)
	log4j.Debug("Committing fast sync pivot as new head", "number", block.Number(), "hash", block.Hash())
	if _, err := d.blockchain.InsertReceiptChain([]*types.Block{block}, []types.Receipts{result.Receipts}); err != nil {
		return err
	}
	if err := d.blockchain.FastSyncCommitHead(block.Hash()); err != nil {
		return err
	}
	atomic.StoreInt32(&d.committed, 1)
	return nil
}

func (d *Downloader) DeliverHeaders(id string, headers []*types.Header) (err error) {
	return d.deliver(id, d.headerCh, &headerPack{id, headers}, headerInMeter, headerDropMeter)
}

func (d *Downloader) DeliverBodies(id string, transactions [][]*types.Transaction, uncles [][]*types.Header) (err error) {
	return d.deliver(id, d.bodyCh, &bodyPack{id, transactions, uncles}, bodyInMeter, bodyDropMeter)
}

func (d *Downloader) DeliverReceipts(id string, receipts [][]*types.Receipt) (err error) {
	return d.deliver(id, d.receiptCh, &receiptPack{id, receipts}, receiptInMeter, receiptDropMeter)
}

func (d *Downloader) DeliverNodeData(id string, data [][]byte) (err error) {
	return d.deliver(id, d.stateCh, &statePack{id, data}, stateInMeter, stateDropMeter)
}

func (d *Downloader) deliver(id string, destCh chan dataPack, packet dataPack, inMeter, dropMeter metrics.Meter) (err error) {
	// Update the delivery metrics for both good and failed deliveries
	inMeter.Mark(int64(packet.Items()))
	defer func() {
		if err != nil {
			dropMeter.Mark(int64(packet.Items()))
		}
	}()
	// Deliver or abort if the sync is canceled while queuing
	d.cancelLock.RLock()
	cancel := d.cancelCh
	d.cancelLock.RUnlock()
	if cancel == nil {
		return errNoSyncActive
	}
	select {
	case destCh <- packet:
		return nil
	case <-cancel:
		return errNoSyncActive
	}
}

func (d *Downloader) qosTuner() {
	for {
		// Retrieve the current median RTT and integrate into the previoust target RTT
		rtt := time.Duration((1-qosTuningImpact)*float64(atomic.LoadUint64(&d.rttEstimate)) + qosTuningImpact*float64(d.peers.medianRTT()))
		atomic.StoreUint64(&d.rttEstimate, uint64(rtt))

		// A new RTT cycle passed, increase our confidence in the estimated RTT
		conf := atomic.LoadUint64(&d.rttConfidence)
		conf = conf + (1000000-conf)/2
		atomic.StoreUint64(&d.rttConfidence, conf)

		// Log the new QoS values and sleep until the next RTT
		log4j.Debug("Recalculated downloader QoS values", "rtt", rtt, "confidence", float64(conf)/1000000.0, "ttl", d.requestTTL())
		select {
		case <-d.quitCh:
			return
		case <-time.After(rtt):
		}
	}
}

func (d *Downloader) qosReduceConfidence() {
	// If we have a single peer, confidence is always 1
	peers := uint64(d.peers.Len())
	if peers == 0 {
		// Ensure peer connectivity races don't catch us off guard
		return
	}
	if peers == 1 {
		atomic.StoreUint64(&d.rttConfidence, 1000000)
		return
	}
	// If we have a ton of peers, don't drop confidence)
	if peers >= uint64(qosConfidenceCap) {
		return
	}
	// Otherwise drop the confidence factor
	conf := atomic.LoadUint64(&d.rttConfidence) * (peers - 1) / peers
	if float64(conf)/1000000 < rttMinConfidence {
		conf = uint64(rttMinConfidence * 1000000)
	}
	atomic.StoreUint64(&d.rttConfidence, conf)

	rtt := time.Duration(atomic.LoadUint64(&d.rttEstimate))
	log4j.Debug("Relaxed downloader QoS values", "rtt", rtt, "confidence", float64(conf)/1000000.0, "ttl", d.requestTTL())
}

//
func (d *Downloader) requestRTT() time.Duration {
	return time.Duration(atomic.LoadUint64(&d.rttEstimate)) * 9 / 10
}

func (d *Downloader) requestTTL() time.Duration {
	var (
		rtt  = time.Duration(atomic.LoadUint64(&d.rttEstimate))
		conf = float64(atomic.LoadUint64(&d.rttConfidence)) / 1000000.0
	)
	ttl := time.Duration(ttlScaling) * time.Duration(float64(rtt)/conf)
	if ttl > ttlLimit {
		ttl = ttlLimit
	}
	return ttl
}
