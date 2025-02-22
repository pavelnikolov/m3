// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package series

import (
	"errors"
	"sync"
	"time"

	"github.com/m3db/m3/src/dbnode/persist"
	"github.com/m3db/m3/src/dbnode/storage/block"
	"github.com/m3db/m3/src/dbnode/ts"
	"github.com/m3db/m3/src/dbnode/x/xio"
	"github.com/m3db/m3/src/x/context"
	"github.com/m3db/m3/src/x/ident"
	"github.com/m3db/m3/src/x/instrument"
	xtime "github.com/m3db/m3/src/x/time"

	"github.com/m3db/m3/src/dbnode/namespace"
	"go.uber.org/zap"
)

type bootstrapState int

const (
	bootstrapNotStarted bootstrapState = iota
	bootstrapped
)

var (
	// ErrSeriesAllDatapointsExpired is returned on tick when all datapoints are expired
	ErrSeriesAllDatapointsExpired = errors.New("series datapoints are all expired")

	errSeriesAlreadyBootstrapped         = errors.New("series is already bootstrapped")
	errSeriesNotBootstrapped             = errors.New("series is not yet bootstrapped")
	errBlockStateSnapshotNotBootstrapped = errors.New("block state snapshot is not bootstrapped")
)

type dbSeries struct {
	sync.RWMutex
	opts Options

	// NB(r): One should audit all places that access the
	// series ID before changing ownership semantics (e.g.
	// pooling the ID rather than releasing it to the GC on
	// calling series.Reset()).
	id   ident.ID
	tags ident.Tags

	buffer                      databaseBuffer
	cachedBlocks                block.DatabaseSeriesBlocks
	bs                          bootstrapState
	blockRetriever              QueryableBlockRetriever
	onRetrieveBlock             block.OnRetrieveBlock
	blockOnEvictedFromWiredList block.OnEvictedFromWiredList
	pool                        DatabaseSeriesPool
}

// NewDatabaseSeries creates a new database series
func NewDatabaseSeries(id ident.ID, tags ident.Tags, opts Options) DatabaseSeries {
	s := newDatabaseSeries()
	s.Reset(id, tags, nil, nil, nil, opts)
	return s
}

// newPooledDatabaseSeries creates a new pooled database series
func newPooledDatabaseSeries(pool DatabaseSeriesPool) DatabaseSeries {
	series := newDatabaseSeries()
	series.pool = pool
	return series
}

// NB(prateek): dbSeries.Reset(...) must be called upon the returned
// object prior to use.
func newDatabaseSeries() *dbSeries {
	series := &dbSeries{
		cachedBlocks: block.NewDatabaseSeriesBlocks(0),
		bs:           bootstrapNotStarted,
	}
	series.buffer = newDatabaseBuffer()
	return series
}

func (s *dbSeries) now() time.Time {
	nowFn := s.opts.ClockOptions().NowFn()
	return nowFn()
}

func (s *dbSeries) ID() ident.ID {
	s.RLock()
	id := s.id
	s.RUnlock()
	return id
}

func (s *dbSeries) Tags() ident.Tags {
	s.RLock()
	tags := s.tags
	s.RUnlock()
	return tags
}

func (s *dbSeries) Tick(blockStates ShardBlockStateSnapshot, nsCtx namespace.Context) (TickResult, error) {
	var r TickResult

	s.Lock()

	bufferResult := s.buffer.Tick(blockStates, nsCtx)
	r.MergedOutOfOrderBlocks = bufferResult.mergedOutOfOrderBlocks
	r.EvictedBuckets = bufferResult.evictedBucketTimes.Len()
	update, err := s.updateBlocksWithLock(blockStates, bufferResult.evictedBucketTimes)
	if err != nil {
		s.Unlock()
		return r, err
	}
	r.TickStatus = update.TickStatus
	r.MadeExpiredBlocks, r.MadeUnwiredBlocks =
		update.madeExpiredBlocks, update.madeUnwiredBlocks

	s.Unlock()

	if update.ActiveBlocks == 0 {
		return r, ErrSeriesAllDatapointsExpired
	}
	return r, nil
}

type updateBlocksResult struct {
	TickStatus
	madeExpiredBlocks int
	madeUnwiredBlocks int
}

func (s *dbSeries) updateBlocksWithLock(
	blockStates ShardBlockStateSnapshot,
	evictedBucketTimes OptimizedTimes,
) (updateBlocksResult, error) {
	var (
		result       updateBlocksResult
		now          = s.now()
		ropts        = s.opts.RetentionOptions()
		cachePolicy  = s.opts.CachePolicy()
		expireCutoff = now.Add(-ropts.RetentionPeriod()).Truncate(ropts.BlockSize())
		wiredTimeout = ropts.BlockDataExpiryAfterNotAccessedPeriod()
	)
	for startNano, currBlock := range s.cachedBlocks.AllBlocks() {
		start := startNano.ToTime()
		if start.Before(expireCutoff) || evictedBucketTimes.Contains(xtime.ToUnixNano(start)) {
			s.cachedBlocks.RemoveBlockAt(start)
			// If we're using the LRU policy and the block was retrieved from disk,
			// then don't close the block because that is the WiredList's
			// responsibility. The block will hang around the WiredList until
			// it is evicted to make room for something else at which point it
			// will be closed.
			//
			// Note that while we don't close the block, we do remove it from the list
			// of blocks. This is so that the series itself can still be expired if this
			// was the last block. The WiredList will still notify the shard/series via
			// the OnEvictedFromWiredList method when it closes the block, but those
			// methods are noops for series/blocks that have already been removed.
			//
			// Also note that while technically the DatabaseBlock protects against double
			// closes, they can be problematic due to pooling. I.E if the following sequence
			// of actions happens:
			// 		1) Tick closes expired block
			// 		2) Block is re-inserted into pool
			// 		3) Block is pulled out of pool and used for critical data
			// 		4) WiredList tries to close the block, not knowing that it has
			// 		   already been closed, and re-opened / re-used leading to
			// 		   unexpected behavior or data loss.
			if cachePolicy == CacheLRU && currBlock.WasRetrievedFromDisk() {
				// Do nothing
			} else {
				currBlock.Close()
			}
			result.madeExpiredBlocks++
			continue
		}

		result.ActiveBlocks++

		if cachePolicy == CacheAll {
			// Never unwire
			result.WiredBlocks++
			continue
		}

		// Potentially unwire
		var unwired, shouldUnwire bool
		blockStatesSnapshot, bootstrapped := blockStates.UnwrapValue()
		// Only use block state snapshot information to make eviction decisions if the block state
		// has been properly bootstrapped already.
		if bootstrapped {
			// Makes sure that the block has been flushed, which
			// prevents us from unwiring blocks that haven't been flushed yet which
			// would cause data loss.
			if blockState := blockStatesSnapshot.Snapshot[startNano]; blockState.WarmRetrievable {
				switch cachePolicy {
				case CacheNone:
					shouldUnwire = true
				case CacheRecentlyRead:
					sinceLastRead := now.Sub(currBlock.LastReadTime())
					shouldUnwire = sinceLastRead >= wiredTimeout
				case CacheLRU:
					// The tick is responsible for managing the lifecycle of blocks that were not
					// read from disk (not retrieved), and the WiredList will manage those that were
					// retrieved from disk.
					shouldUnwire = !currBlock.WasRetrievedFromDisk()
				default:
					s.opts.InstrumentOptions().Logger().Fatal(
						"unhandled cache policy in series tick", zap.Any("policy", cachePolicy))
				}
			}
		}

		if shouldUnwire {
			// Remove the block and it will be looked up later
			s.cachedBlocks.RemoveBlockAt(start)
			currBlock.Close()
			unwired = true
			result.madeUnwiredBlocks++
		}

		if unwired {
			result.UnwiredBlocks++
		} else {
			result.WiredBlocks++
			if currBlock.HasMergeTarget() {
				result.PendingMergeBlocks++
			}
		}
	}

	bufferStats := s.buffer.Stats()
	result.ActiveBlocks += bufferStats.wiredBlocks
	result.WiredBlocks += bufferStats.wiredBlocks

	return result, nil
}

func (s *dbSeries) IsEmpty() bool {
	s.RLock()
	blocksLen := s.cachedBlocks.Len()
	bufferEmpty := s.buffer.IsEmpty()
	s.RUnlock()
	if blocksLen == 0 && bufferEmpty {
		return true
	}
	return false
}

func (s *dbSeries) NumActiveBlocks() int {
	s.RLock()
	value := s.cachedBlocks.Len() + s.buffer.Stats().wiredBlocks
	s.RUnlock()
	return value
}

func (s *dbSeries) IsBootstrapped() bool {
	s.RLock()
	state := s.bs
	s.RUnlock()
	return state == bootstrapped
}

func (s *dbSeries) Write(
	ctx context.Context,
	timestamp time.Time,
	value float64,
	unit xtime.Unit,
	annotation []byte,
	wOpts WriteOptions,
) (bool, error) {
	s.Lock()
	wasWritten, err := s.buffer.Write(ctx, timestamp, value, unit, annotation, wOpts)
	s.Unlock()
	return wasWritten, err
}

func (s *dbSeries) ReadEncoded(
	ctx context.Context,
	start, end time.Time,
	nsCtx namespace.Context,
) ([][]xio.BlockReader, error) {
	s.RLock()
	reader := NewReaderUsingRetriever(s.id, s.blockRetriever, s.onRetrieveBlock, s, s.opts)
	r, err := reader.readersWithBlocksMapAndBuffer(ctx, start, end, s.cachedBlocks, s.buffer, nsCtx)
	s.RUnlock()
	return r, err
}

func (s *dbSeries) FetchBlocksForColdFlush(
	ctx context.Context,
	start time.Time,
	version int,
	nsCtx namespace.Context,
) ([]xio.BlockReader, error) {
	// This needs a write lock because the version on underlying buckets need
	// to be modified.
	s.Lock()
	br, err := s.buffer.FetchBlocksForColdFlush(ctx, start, version, nsCtx)
	s.Unlock()

	return br, err
}

func (s *dbSeries) FetchBlocks(
	ctx context.Context,
	starts []time.Time,
	nsCtx namespace.Context,
) ([]block.FetchBlockResult, error) {
	s.RLock()
	r, err := Reader{
		opts:       s.opts,
		id:         s.id,
		retriever:  s.blockRetriever,
		onRetrieve: s.onRetrieveBlock,
	}.fetchBlocksWithBlocksMapAndBuffer(ctx, starts, s.cachedBlocks, s.buffer, nsCtx)
	s.RUnlock()
	return r, err
}

func (s *dbSeries) FetchBlocksMetadata(
	ctx context.Context,
	start, end time.Time,
	opts FetchBlocksMetadataOptions,
) (block.FetchBlocksMetadataResult, error) {
	blockSize := s.opts.RetentionOptions().BlockSize()
	res := s.opts.FetchBlockMetadataResultsPool().Get()

	s.RLock()
	defer s.RUnlock()

	blocks := s.cachedBlocks.AllBlocks()

	for tNano, b := range blocks {
		t := tNano.ToTime()
		if !start.Before(t.Add(blockSize)) || !t.Before(end) {
			continue
		}
		if !opts.IncludeCachedBlocks && b.WasRetrievedFromDisk() {
			// Do not include cached blocks if not specified to, this is
			// to avoid high amounts of duplication if a significant number of
			// blocks are cached in memory when returning blocks metadata
			// from both in-memory and disk structures.
			continue
		}
		var (
			size     int64
			checksum *uint32
			lastRead time.Time
		)
		if opts.IncludeSizes {
			size = int64(b.Len())
		}
		if opts.IncludeChecksums {
			v, err := b.Checksum()
			if err != nil {
				return block.FetchBlocksMetadataResult{}, err
			}
			checksum = &v
		}
		if opts.IncludeLastRead {
			lastRead = b.LastReadTime()
		}
		res.Add(block.FetchBlockMetadataResult{
			Start:    t,
			Size:     size,
			Checksum: checksum,
			LastRead: lastRead,
		})
	}

	// Iterate over the encoders in the database buffer
	if !s.buffer.IsEmpty() {
		bufferResults, err := s.buffer.FetchBlocksMetadata(ctx, start, end, opts)
		if err != nil {
			return block.FetchBlocksMetadataResult{}, err
		}
		for _, result := range bufferResults.Results() {
			res.Add(result)
		}
		bufferResults.Close()
	}

	res.Sort()

	// NB(r): Since ID and Tags are garbage collected we can safely
	// return refs.
	tagsIter := s.opts.IdentifierPool().TagsIterator()
	tagsIter.Reset(s.tags)
	return block.NewFetchBlocksMetadataResult(s.id, tagsIter, res), nil
}

func (s *dbSeries) addBlockWithLock(b block.DatabaseBlock) {
	b.SetOnEvictedFromWiredList(s.blockOnEvictedFromWiredList)
	s.cachedBlocks.AddBlock(b)
}

func (s *dbSeries) Load(
	opts LoadOptions,
	bootstrappedBlocks block.DatabaseSeriesBlocks,
	blockStates BootstrappedBlockStateSnapshot,
) (LoadResult, error) {
	if opts.Bootstrap {
		bsResult, err := s.bootstrap(bootstrappedBlocks, blockStates)
		return LoadResult{Bootstrap: bsResult}, err
	}

	s.load(bootstrappedBlocks, blockStates)
	return LoadResult{}, nil
}

func (s *dbSeries) bootstrap(
	bootstrappedBlocks block.DatabaseSeriesBlocks,
	blockStates BootstrappedBlockStateSnapshot,
) (BootstrapResult, error) {
	s.Lock()
	defer func() {
		s.bs = bootstrapped
		s.Unlock()
	}()

	var result BootstrapResult
	if s.bs == bootstrapped {
		return result, errSeriesAlreadyBootstrapped
	}

	if bootstrappedBlocks == nil {
		return result, nil
	}

	s.loadWithLock(bootstrappedBlocks, blockStates)
	result.NumBlocksMovedToBuffer += int64(bootstrappedBlocks.Len())

	return result, nil
}

func (s *dbSeries) load(
	bootstrappedBlocks block.DatabaseSeriesBlocks,
	blockStates BootstrappedBlockStateSnapshot,
) {
	s.Lock()
	s.loadWithLock(bootstrappedBlocks, blockStates)
	s.Unlock()
}

func (s *dbSeries) loadWithLock(
	bootstrappedBlocks block.DatabaseSeriesBlocks,
	blockStates BootstrappedBlockStateSnapshot,
) {
	for _, block := range bootstrappedBlocks.AllBlocks() {
		blStartNano := xtime.ToUnixNano(block.StartTime())
		blState := blockStates.Snapshot[blStartNano]
		if !blState.WarmRetrievable {
			// If the block being bootstrapped has never been warm flushed before then the block
			// can be loaded into the buffer as a WarmWrite because a subsequent warm flush will
			// ensure that it gets persisted to disk.
			//
			// If the ColdWrites feature is disabled then this branch should always be followed.
			s.buffer.Load(block, WarmWrite)
		} else {
			// If the block being bootstrapped has been warm flushed before then the block should
			// be loaded into the buffer as a ColdWrite so that a subsequent cold flush will ensure
			// that it gets persisted to disk.
			//
			// This branch can be executed in the situation that a cold write was received for a block
			// that had already been flushed to disk. Before the cold write could be persisted to disk
			// via a cold flush, the node crashed and began bootsrapping itself. The cold write would be
			// read out of the commitlog and would eventually be loaded into the buffer via this branch.
			s.buffer.Load(block, ColdWrite)
		}
	}
}

func (s *dbSeries) OnRetrieveBlock(
	id ident.ID,
	tags ident.TagIterator,
	startTime time.Time,
	segment ts.Segment,
	nsCtx namespace.Context,
) {
	var (
		b    block.DatabaseBlock
		list *block.WiredList
	)
	s.Lock()
	defer func() {
		s.Unlock()
		if b != nil && list != nil {
			// 1) We need to update the WiredList so that blocks that were read from disk
			// can enter the list (OnReadBlock is only called for blocks that
			// were read from memory, regardless of whether the data originated
			// from disk or a buffer rotation.)
			// 2) We must perform this action outside of the lock to prevent deadlock
			// with the WiredList itself when it tries to call OnEvictedFromWiredList
			// on the same series that is trying to perform a blocking update.
			// 3) Doing this outside of the lock is safe because updating the
			// wired list is asynchronous already (Update just puts the block in
			// a channel to be processed later.)
			// 4) We have to perform a blocking update because in this flow, the block
			// is not already in the wired list so we need to make sure that the WiredList
			// takes control of its lifecycle.
			list.BlockingUpdate(b)
		}
	}()

	if !id.Equal(s.id) {
		return
	}

	b = s.opts.DatabaseBlockOptions().DatabaseBlockPool().Get()
	blockSize := s.opts.RetentionOptions().BlockSize()
	b.ResetFromDisk(startTime, blockSize, segment, s.id, nsCtx)

	// NB(r): Blocks retrieved have been triggered by a read, so set the last
	// read time as now so caching policies are followed.
	b.SetLastReadTime(s.now())

	// If we retrieved this from disk then we directly emplace it
	s.addBlockWithLock(b)

	list = s.opts.DatabaseBlockOptions().WiredList()
}

// OnReadBlock is only called for blocks that were read from memory, regardless of
// whether the data originated from disk or buffer rotation.
func (s *dbSeries) OnReadBlock(b block.DatabaseBlock) {
	if list := s.opts.DatabaseBlockOptions().WiredList(); list != nil {
		// The WiredList is only responsible for managing the lifecycle of blocks
		// retrieved from disk.
		if b.WasRetrievedFromDisk() {
			// 1) Need to update the WiredList so it knows which blocks have been
			// most recently read.
			// 2) We do a non-blocking update here to prevent deadlock with the
			// WiredList calling OnEvictedFromWiredList on the same series since
			// OnReadBlock is usually called within the context of a read lock
			// on this series.
			// 3) Its safe to do a non-blocking update because the wired list has
			// already been exposed to this block, so even if the wired list drops
			// this update, it will still manage this blocks lifecycle.
			list.NonBlockingUpdate(b)
		}
	}
}

func (s *dbSeries) OnEvictedFromWiredList(id ident.ID, blockStart time.Time) {
	s.Lock()
	defer s.Unlock()

	// Should never happen
	if !id.Equal(s.id) {
		return
	}

	block, ok := s.cachedBlocks.BlockAt(blockStart)
	if ok {
		if !block.WasRetrievedFromDisk() {
			// Should never happen - invalid application state could cause data loss
			instrument.EmitAndLogInvariantViolation(
				s.opts.InstrumentOptions(), func(l *zap.Logger) {
					l.With(
						zap.String("id", id.String()),
						zap.Time("blockStart", blockStart),
					).Error("tried to evict block that was not retrieved from disk")
				})
			return
		}

		s.cachedBlocks.RemoveBlockAt(blockStart)
	}
}

func (s *dbSeries) WarmFlush(
	ctx context.Context,
	blockStart time.Time,
	persistFn persist.DataFn,
	nsCtx namespace.Context,
) (FlushOutcome, error) {
	s.Lock()
	defer s.Unlock()

	if s.bs != bootstrapped {
		return FlushOutcomeErr, errSeriesNotBootstrapped
	}

	return s.buffer.WarmFlush(ctx, blockStart, s.id, s.tags, persistFn, nsCtx)
}

func (s *dbSeries) Snapshot(
	ctx context.Context,
	blockStart time.Time,
	persistFn persist.DataFn,
	nsCtx namespace.Context,
) error {
	// Need a write lock because the buffer Snapshot method mutates
	// state (by performing a pro-active merge).
	s.Lock()
	defer s.Unlock()

	if s.bs != bootstrapped {
		return errSeriesNotBootstrapped
	}

	return s.buffer.Snapshot(ctx, blockStart, s.id, s.tags, persistFn, nsCtx)
}

func (s *dbSeries) ColdFlushBlockStarts(blockStates BootstrappedBlockStateSnapshot) OptimizedTimes {
	s.RLock()
	defer s.RUnlock()

	return s.buffer.ColdFlushBlockStarts(blockStates.Snapshot)
}

func (s *dbSeries) Close() {
	s.Lock()
	defer s.Unlock()

	// See Reset() for why these aren't finalized
	s.id = nil
	s.tags = ident.Tags{}

	switch s.opts.CachePolicy() {
	case CacheLRU:
		// In the CacheLRU case, blocks that were retrieved from disk are owned
		// by the WiredList and should not be closed here. They will eventually
		// be evicted and closed by the WiredList when it needs to make room
		// for new blocks.
	default:
		// This call closes the blocks as well.
		s.cachedBlocks.RemoveAll()
	}

	// Reset (not close) underlying resources because the series will go
	// back into the pool and be re-used.
	s.buffer.Reset(nil, s.opts)
	s.cachedBlocks.Reset()

	if s.pool != nil {
		s.pool.Put(s)
	}
}

func (s *dbSeries) Reset(
	id ident.ID,
	tags ident.Tags,
	blockRetriever QueryableBlockRetriever,
	onRetrieveBlock block.OnRetrieveBlock,
	onEvictedFromWiredList block.OnEvictedFromWiredList,
	opts Options,
) {
	s.Lock()
	defer s.Unlock()

	// NB(r): We explicitly do not place this ID back into an
	// existing pool as high frequency users of series IDs such
	// as the commit log need to use the reference without the
	// overhead of ownership tracking. In addition, the blocks
	// themselves have a reference to the ID which is required
	// for the LRU/WiredList caching strategy eviction process.
	// Since the wired list can still have a reference to a
	// DatabaseBlock for which the corresponding DatabaseSeries
	// has been closed, its important that the ID itself is still
	// available because the process of kicking a DatabaseBlock
	// out of the WiredList requires the ID.
	//
	// Since series are purged so infrequently the overhead
	// of not releasing back an ID to a pool is amortized over
	// a long period of time.
	s.id = id
	s.tags = tags

	s.cachedBlocks.Reset()
	s.buffer.Reset(id, opts)
	s.opts = opts
	s.bs = bootstrapNotStarted
	s.blockRetriever = blockRetriever
	s.onRetrieveBlock = onRetrieveBlock
	s.blockOnEvictedFromWiredList = onEvictedFromWiredList
}
