package initialsync

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/kevinms/leakybucket-go"
	streamhelpers "github.com/libp2p/go-libp2p-core/helpers"
	"github.com/libp2p/go-libp2p-core/mux"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/pkg/errors"
	eth "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/beacon-chain/blockchain"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/beacon-chain/flags"
	"github.com/prysmaticlabs/prysm/beacon-chain/p2p"
	prysmsync "github.com/prysmaticlabs/prysm/beacon-chain/sync"
	p2ppb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	"github.com/prysmaticlabs/prysm/shared/featureconfig"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/rand"
	"github.com/sirupsen/logrus"
	"go.opencensus.io/trace"
)

const (
	// maxPendingRequests limits how many concurrent fetch request one can initiate.
	maxPendingRequests = 64
	// peersPercentagePerRequest caps percentage of peers to be used in a request.
	peersPercentagePerRequest = 0.75
	// handshakePollingInterval is a polling interval for checking the number of received handshakes.
	handshakePollingInterval = 5 * time.Second
	// peerLocksPollingInterval is a polling interval for checking if there are stale peer locks.
	peerLocksPollingInterval = 5 * time.Minute
	// peerLockMaxAge is maximum time before stale lock is purged.
	peerLockMaxAge = 60 * time.Minute
	// nonSkippedSlotsFullSearchEpochs how many epochs to check in full, before resorting to random
	// sampling of slots once per epoch
	nonSkippedSlotsFullSearchEpochs = 10
	// peerFilterCapacityWeight defines how peer's capacity affects peer's score. Provided as
	// percentage, i.e. 0.3 means capacity will determine 30% of peer's score.
	peerFilterCapacityWeight = 0.2
)

var (
	errNoPeersAvailable      = errors.New("no peers available, waiting for reconnect")
	errFetcherCtxIsDone      = errors.New("fetcher's context is done, reinitialize")
	errSlotIsTooHigh         = errors.New("slot is higher than the finalized slot")
	errBlockAlreadyProcessed = errors.New("block is already processed")
)

// blocksFetcherConfig is a config to setup the block fetcher.
type blocksFetcherConfig struct {
	headFetcher              blockchain.HeadFetcher
	finalizationFetcher      blockchain.FinalizationFetcher
	p2p                      p2p.P2P
	peerFilterCapacityWeight float64
	mode                     syncMode
}

// blocksFetcher is a service to fetch chain data from peers.
// On an incoming requests, requested block range is evenly divided
// among available peers (for fair network load distribution).
type blocksFetcher struct {
	sync.Mutex
	ctx                 context.Context
	cancel              context.CancelFunc
	rand                *rand.Rand
	headFetcher         blockchain.HeadFetcher
	finalizationFetcher blockchain.FinalizationFetcher
	p2p                 p2p.P2P
	blocksPerSecond     uint64
	rateLimiter         *leakybucket.Collector
	peerLocks           map[peer.ID]*peerLock
	fetchRequests       chan *fetchRequestParams
	fetchResponses      chan *fetchRequestResponse
	capacityWeight      float64       // how remaining capacity affects peer selection
	mode                syncMode      // allows to use fetcher in different sync scenarios
	quit                chan struct{} // termination notifier
}

// peerLock restricts fetcher actions on per peer basis. Currently, used for rate limiting.
type peerLock struct {
	sync.Mutex
	accessed time.Time
}

// fetchRequestParams holds parameters necessary to schedule a fetch request.
type fetchRequestParams struct {
	ctx   context.Context // if provided, it is used instead of global fetcher's context
	start uint64          // starting slot
	count uint64          // how many slots to receive (fetcher may return fewer slots)
}

// fetchRequestResponse is a combined type to hold results of both successful executions and errors.
// Valid usage pattern will be to check whether result's `err` is nil, before using `blocks`.
type fetchRequestResponse struct {
	pid          peer.ID
	start, count uint64
	blocks       []*eth.SignedBeaconBlock
	err          error
}

// newBlocksFetcher creates ready to use fetcher.
func newBlocksFetcher(ctx context.Context, cfg *blocksFetcherConfig) *blocksFetcher {
	blocksPerSecond := flags.Get().BlockBatchLimit
	allowedBlocksBurst := flags.Get().BlockBatchLimitBurstFactor * flags.Get().BlockBatchLimit
	// Allow fetcher to go almost to the full burst capacity (less a single batch).
	rateLimiter := leakybucket.NewCollector(
		float64(blocksPerSecond), int64(allowedBlocksBurst-blocksPerSecond),
		false /* deleteEmptyBuckets */)

	capacityWeight := cfg.peerFilterCapacityWeight
	if capacityWeight >= 1 {
		capacityWeight = peerFilterCapacityWeight
	}

	ctx, cancel := context.WithCancel(ctx)
	return &blocksFetcher{
		ctx:                 ctx,
		cancel:              cancel,
		rand:                rand.NewGenerator(),
		headFetcher:         cfg.headFetcher,
		finalizationFetcher: cfg.finalizationFetcher,
		p2p:                 cfg.p2p,
		blocksPerSecond:     uint64(blocksPerSecond),
		rateLimiter:         rateLimiter,
		peerLocks:           make(map[peer.ID]*peerLock),
		fetchRequests:       make(chan *fetchRequestParams, maxPendingRequests),
		fetchResponses:      make(chan *fetchRequestResponse, maxPendingRequests),
		capacityWeight:      capacityWeight,
		quit:                make(chan struct{}),
	}
}

// start boots up the fetcher, which starts listening for incoming fetch requests.
func (f *blocksFetcher) start() error {
	select {
	case <-f.ctx.Done():
		return errFetcherCtxIsDone
	default:
		go f.loop()
		return nil
	}
}

// stop terminates all fetcher operations.
func (f *blocksFetcher) stop() {
	defer func() {
		if f.rateLimiter != nil {
			f.rateLimiter.Free()
			f.rateLimiter = nil
		}
	}()
	f.cancel()
	<-f.quit // make sure that loop() is done
}

// requestResponses exposes a channel into which fetcher pushes generated request responses.
func (f *blocksFetcher) requestResponses() <-chan *fetchRequestResponse {
	return f.fetchResponses
}

// loop is a main fetcher loop, listens for incoming requests/cancellations, forwards outgoing responses.
func (f *blocksFetcher) loop() {
	defer close(f.quit)

	// Wait for all loop's goroutines to finish, and safely release resources.
	wg := &sync.WaitGroup{}
	defer func() {
		wg.Wait()
		close(f.fetchResponses)
	}()

	// Periodically remove stale peer locks.
	go func() {
		ticker := time.NewTicker(peerLocksPollingInterval)
		for {
			select {
			case <-ticker.C:
				f.removeStalePeerLocks(peerLockMaxAge)
			case <-f.ctx.Done():
				ticker.Stop()
				return
			}
		}
	}()

	// Main loop.
	for {
		// Make sure there is are available peers before processing requests.
		if _, err := f.waitForMinimumPeers(f.ctx); err != nil {
			log.Error(err)
		}

		select {
		case <-f.ctx.Done():
			log.Debug("Context closed, exiting goroutine (blocks fetcher)")
			return
		case req := <-f.fetchRequests:
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case <-f.ctx.Done():
				case f.fetchResponses <- f.handleRequest(req.ctx, req.start, req.count):
				}
			}()
		}
	}
}

// scheduleRequest adds request to incoming queue.
func (f *blocksFetcher) scheduleRequest(ctx context.Context, start, count uint64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	request := &fetchRequestParams{
		ctx:   ctx,
		start: start,
		count: count,
	}
	select {
	case <-f.ctx.Done():
		return errFetcherCtxIsDone
	case f.fetchRequests <- request:
	}
	return nil
}

// handleRequest parses fetch request and forwards it to response builder.
func (f *blocksFetcher) handleRequest(ctx context.Context, start, count uint64) *fetchRequestResponse {
	ctx, span := trace.StartSpan(ctx, "initialsync.handleRequest")
	defer span.End()

	response := &fetchRequestResponse{
		start:  start,
		count:  count,
		blocks: []*eth.SignedBeaconBlock{},
		err:    nil,
	}

	if ctx.Err() != nil {
		response.err = ctx.Err()
		return response
	}

	var targetEpoch uint64
	var peers []peer.ID
	if f.mode == modeStopOnFinalizedEpoch {
		headEpoch := f.finalizationFetcher.FinalizedCheckpt().Epoch
		targetEpoch, peers = f.p2p.Peers().BestFinalized(params.BeaconConfig().MaxPeersToSync, headEpoch)
	} else {
		headEpoch := helpers.SlotToEpoch(f.headFetcher.HeadSlot())
		targetEpoch, peers = f.p2p.Peers().BestNonFinalized(flags.Get().MinimumSyncPeers, headEpoch)
	}
	if len(peers) == 0 {
		response.err = errNoPeersAvailable
		return response
	}

	// Short circuit start far exceeding the highest finalized epoch in some infinite loop.
	if f.mode == modeStopOnFinalizedEpoch {
		highestFinalizedSlot := helpers.StartSlot(targetEpoch + 1)
		if start > highestFinalizedSlot {
			response.err = fmt.Errorf("%v, slot: %d, highest finalized slot: %d",
				errSlotIsTooHigh, start, highestFinalizedSlot)
			return response
		}
	}

	response.blocks, response.pid, response.err = f.fetchBlocksFromPeer(ctx, start, count, peers)
	return response
}

// fetchBlocksFromPeer fetches blocks from a single randomly selected peer.
func (f *blocksFetcher) fetchBlocksFromPeer(
	ctx context.Context,
	start, count uint64,
	peers []peer.ID,
) ([]*eth.SignedBeaconBlock, peer.ID, error) {
	ctx, span := trace.StartSpan(ctx, "initialsync.fetchBlocksFromPeer")
	defer span.End()

	var blocks []*eth.SignedBeaconBlock
	var err error
	if featureconfig.Get().EnablePeerScorer {
		peers, err = f.filterScoredPeers(ctx, peers, peersPercentagePerRequest)
	} else {
		peers, err = f.filterPeers(peers, peersPercentagePerRequest)
	}
	if err != nil {
		return blocks, "", err
	}
	req := &p2ppb.BeaconBlocksByRangeRequest{
		StartSlot: start,
		Count:     count,
		Step:      1,
	}
	for i := 0; i < len(peers); i++ {
		if blocks, err = f.requestBlocks(ctx, req, peers[i]); err == nil {
			if featureconfig.Get().EnablePeerScorer {
				f.p2p.Peers().Scorers().BlockProviderScorer().Touch(peers[i])
			}
			return blocks, peers[i], err
		}
	}
	return blocks, "", errNoPeersAvailable
}

// requestBlocks is a wrapper for handling BeaconBlocksByRangeRequest requests/streams.
func (f *blocksFetcher) requestBlocks(
	ctx context.Context,
	req *p2ppb.BeaconBlocksByRangeRequest,
	pid peer.ID,
) ([]*eth.SignedBeaconBlock, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	l := f.getPeerLock(pid)
	if l == nil {
		return nil, errors.New("cannot obtain lock")
	}
	l.Lock()
	log.WithFields(logrus.Fields{
		"peer":     pid,
		"start":    req.StartSlot,
		"count":    req.Count,
		"step":     req.Step,
		"capacity": f.rateLimiter.Remaining(pid.String()),
		"score":    f.p2p.Peers().Scorers().BlockProviderScorer().FormatScorePretty(pid),
	}).Debug("Requesting blocks")
	if f.rateLimiter.Remaining(pid.String()) < int64(req.Count) {
		log.WithField("peer", pid).Debug("Slowing down for rate limit")
		timer := time.NewTimer(f.rateLimiter.TillEmpty(pid.String()))
		select {
		case <-f.ctx.Done():
			timer.Stop()
			return nil, errFetcherCtxIsDone
		case <-timer.C:
			// Peer has gathered enough capacity to be polled again.
		}
	}
	f.rateLimiter.Add(pid.String(), int64(req.Count))
	l.Unlock()
	stream, err := f.p2p.Send(ctx, req, p2p.RPCBlocksByRangeTopic, pid)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := streamhelpers.FullClose(stream); err != nil && err.Error() != mux.ErrReset.Error() {
			log.WithError(err).Debugf("Failed to close stream with protocol %s", stream.Protocol())
		}
	}()

	resp := make([]*eth.SignedBeaconBlock, 0, req.Count)
	for i := uint64(0); ; i++ {
		isFirstChunk := i == 0
		blk, err := prysmsync.ReadChunkedBlock(stream, f.p2p, isFirstChunk)
		if err == io.EOF {
			break
		}
		// exit if more than max request blocks are returned
		if i >= params.BeaconNetworkConfig().MaxRequestBlocks {
			break
		}
		if err != nil {
			return nil, err
		}
		resp = append(resp, blk)
	}

	return resp, nil
}
