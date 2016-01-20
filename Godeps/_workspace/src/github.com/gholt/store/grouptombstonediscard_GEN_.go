package store

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/gholt/brimtime.v1"
)

type groupTombstoneDiscardState struct {
	interval       int
	age            uint64
	batchSize      int
	notifyChanLock sync.Mutex
	notifyChan     chan *bgNotification
	localRemovals  [][]groupLocalRemovalEntry
}

type groupLocalRemovalEntry struct {
	keyA uint64
	keyB uint64

	nameKeyA uint64
	nameKeyB uint64

	timestampbits uint64
}

func (store *DefaultGroupStore) tombstoneDiscardConfig(cfg *GroupStoreConfig) {
	store.tombstoneDiscardState.interval = cfg.TombstoneDiscardInterval
	store.tombstoneDiscardState.age = (uint64(cfg.TombstoneAge) * uint64(time.Second) / 1000) << _TSB_UTIL_BITS
	store.tombstoneDiscardState.batchSize = cfg.TombstoneDiscardBatchSize
}

// TombstoneDiscardPass will immediately execute a pass to discard expired
// tombstones (deletion markers) rather than waiting for the next interval. If
// a pass is currently executing, it will be stopped and restarted so that a
// call to this function ensures one complete pass occurs.
func (store *DefaultGroupStore) TombstoneDiscardPass() {
	store.tombstoneDiscardState.notifyChanLock.Lock()
	if store.tombstoneDiscardState.notifyChan == nil {
		store.tombstoneDiscardPass(make(chan *bgNotification))
	} else {
		c := make(chan struct{}, 1)
		store.tombstoneDiscardState.notifyChan <- &bgNotification{
			action:   _BG_PASS,
			doneChan: c,
		}
		<-c
	}
	store.tombstoneDiscardState.notifyChanLock.Unlock()
}

// EnableTombstoneDiscard will resume discard passes. A discard pass removes
// expired tombstones (deletion markers).
func (store *DefaultGroupStore) EnableTombstoneDiscard() {
	store.tombstoneDiscardState.notifyChanLock.Lock()
	if store.tombstoneDiscardState.notifyChan == nil {
		store.tombstoneDiscardState.notifyChan = make(chan *bgNotification, 1)
		go store.tombstoneDiscardLauncher(store.tombstoneDiscardState.notifyChan)
	}
	store.tombstoneDiscardState.notifyChanLock.Unlock()
}

// DisableTombstoneDiscard will stop any discard passes until
// EnableTombstoneDiscard is called. A discard pass removes expired tombstones
// (deletion markers).
func (store *DefaultGroupStore) DisableTombstoneDiscard() {
	store.tombstoneDiscardState.notifyChanLock.Lock()
	if store.tombstoneDiscardState.notifyChan != nil {
		c := make(chan struct{}, 1)
		store.tombstoneDiscardState.notifyChan <- &bgNotification{
			action:   _BG_DISABLE,
			doneChan: c,
		}
		<-c
		store.tombstoneDiscardState.notifyChan = nil
	}
	store.tombstoneDiscardState.notifyChanLock.Unlock()
}

func (store *DefaultGroupStore) tombstoneDiscardLauncher(notifyChan chan *bgNotification) {
	interval := float64(store.tombstoneDiscardState.interval) * float64(time.Second)
	store.randMutex.Lock()
	nextRun := time.Now().Add(time.Duration(interval + interval*store.rand.NormFloat64()*0.1))
	store.randMutex.Unlock()
	var notification *bgNotification
	running := true
	for running {
		if notification == nil {
			sleep := nextRun.Sub(time.Now())
			if sleep > 0 {
				select {
				case notification = <-notifyChan:
				case <-time.After(sleep):
				}
			} else {
				select {
				case notification = <-notifyChan:
				default:
				}
			}
		}
		store.randMutex.Lock()
		nextRun = time.Now().Add(time.Duration(interval + interval*store.rand.NormFloat64()*0.1))
		store.randMutex.Unlock()
		if notification != nil {
			var nextNotification *bgNotification
			switch notification.action {
			case _BG_PASS:
				nextNotification = store.tombstoneDiscardPass(notifyChan)
			case _BG_DISABLE:
				running = false
			default:
				store.logCritical("tombstoneDiscard: invalid action requested: %d", notification.action)
			}
			notification.doneChan <- struct{}{}
			notification = nextNotification
		} else {
			notification = store.tombstoneDiscardPass(notifyChan)
		}
	}
}

func (store *DefaultGroupStore) tombstoneDiscardPass(notifyChan chan *bgNotification) *bgNotification {
	if store.logDebug != nil {
		begin := time.Now()
		defer func() {
			store.logDebug("tombstoneDiscard: pass took %s", time.Now().Sub(begin))
		}()
	}
	if n := store.tombstoneDiscardPassLocalRemovals(notifyChan); n != nil {
		return n
	}
	return store.tombstoneDiscardPassExpiredDeletions(notifyChan)
}

// tombstoneDiscardPassLocalRemovals removes all entries marked with the
// _TSB_LOCAL_REMOVAL bit. These are entries that other routines have indicated
// are no longer needed in memory.
func (store *DefaultGroupStore) tombstoneDiscardPassLocalRemovals(notifyChan chan *bgNotification) *bgNotification {
	// Each worker will perform a pass on a subsection of each partition's key
	// space. Additionally, each worker will start their work on different
	// partition. This reduces contention for a given section of the locmap.
	partitionShift := uint16(0)
	partitionMax := uint64(0)
	if store.msgRing != nil {
		pbc := store.msgRing.Ring().PartitionBitCount()
		partitionShift = 64 - pbc
		partitionMax = (uint64(1) << pbc) - 1
	}
	workerMax := uint64(store.workers - 1)
	workerPartitionPiece := (uint64(1) << partitionShift) / (workerMax + 1)
	work := func(partition uint64, worker uint64) {
		partitionOnLeftBits := partition << partitionShift
		rangeBegin := partitionOnLeftBits + (workerPartitionPiece * worker)
		var rangeEnd uint64
		// A little bit of complexity here to handle where the more general
		// expressions would have overflow issues.
		if worker != workerMax {
			rangeEnd = partitionOnLeftBits + (workerPartitionPiece * (worker + 1)) - 1
		} else {
			if partition != partitionMax {
				rangeEnd = ((partition + 1) << partitionShift) - 1
			} else {
				rangeEnd = math.MaxUint64
			}
		}
		store.locmap.Discard(rangeBegin, rangeEnd, _TSB_LOCAL_REMOVAL)
	}
	var abort uint32
	wg := &sync.WaitGroup{}
	wg.Add(int(workerMax + 1))
	workerPartitionOffset := (partitionMax + 1) / (workerMax + 1)
	for worker := uint64(0); worker <= workerMax; worker++ {
		go func(worker uint64) {
			partitionBegin := workerPartitionOffset * worker
			for partition := partitionBegin; partition <= partitionMax; partition++ {
				if atomic.LoadUint32(&abort) != 0 {
					break
				}
				work(partition, worker)
			}
			for partition := uint64(0); partition < partitionBegin; partition++ {
				if atomic.LoadUint32(&abort) != 0 {
					break
				}
				work(partition, worker)
			}
			wg.Done()
		}(worker)
	}
	waitChan := make(chan struct{}, 1)
	go func() {
		wg.Wait()
		close(waitChan)
	}()
	select {
	case notification := <-notifyChan:
		atomic.AddUint32(&abort, 1)
		<-waitChan
		return notification
	case <-waitChan:
		return nil
	}
}

// tombstoneDiscardPassExpiredDeletions scans for entries marked with
// _TSB_DELETION (but not _TSB_LOCAL_REMOVAL) that are older than the maximum
// tombstone age and marks them for _TSB_LOCAL_REMOVAL.
func (store *DefaultGroupStore) tombstoneDiscardPassExpiredDeletions(notifyChan chan *bgNotification) *bgNotification {
	// Each worker will perform a pass on a subsection of each partition's key
	// space. Additionally, each worker will start their work on different
	// partition. This reduces contention for a given section of the locmap.
	partitionShift := uint16(0)
	partitionMax := uint64(0)
	if store.msgRing != nil {
		pbc := store.msgRing.Ring().PartitionBitCount()
		partitionShift = 64 - pbc
		partitionMax = (uint64(1) << pbc) - 1
	}
	workerMax := uint64(store.workers - 1)
	workerPartitionPiece := (uint64(1) << partitionShift) / (workerMax + 1)
	work := func(partition uint64, worker uint64, localRemovals []groupLocalRemovalEntry) {
		partitionOnLeftBits := partition << partitionShift
		rangeBegin := partitionOnLeftBits + (workerPartitionPiece * worker)
		var rangeEnd uint64
		// A little bit of complexity here to handle where the more general
		// expressions would have overflow issues.
		if worker != workerMax {
			rangeEnd = partitionOnLeftBits + (workerPartitionPiece * (worker + 1)) - 1
		} else {
			if partition != partitionMax {
				rangeEnd = ((partition + 1) << partitionShift) - 1
			} else {
				rangeEnd = math.MaxUint64
			}
		}
		cutoff := (uint64(brimtime.TimeToUnixMicro(time.Now())) << _TSB_UTIL_BITS) - store.tombstoneDiscardState.age
		more := true
		for more {
			localRemovalsIndex := 0
			// Since we shouldn't try to modify what we're scanning while we're
			// scanning (lock contention) we instead record in localRemovals
			// what to modify after the scan.
			rangeBegin, more = store.locmap.ScanCallback(rangeBegin, rangeEnd, _TSB_DELETION, _TSB_LOCAL_REMOVAL, cutoff, uint64(store.tombstoneDiscardState.batchSize), func(keyA uint64, keyB uint64, nameKeyA uint64, nameKeyB uint64, timestampbits uint64, length uint32) bool {
				e := &localRemovals[localRemovalsIndex]
				e.keyA = keyA
				e.keyB = keyB

				e.nameKeyA = nameKeyA
				e.nameKeyB = nameKeyB

				e.timestampbits = timestampbits
				localRemovalsIndex++
				return true
			})
			atomic.AddInt32(&store.expiredDeletions, int32(localRemovalsIndex))
			for i := 0; i < localRemovalsIndex; i++ {
				e := &localRemovals[i]
				// These writes go through the entire system, so they're
				// persisted and therefore restored on restarts.
				store.write(e.keyA, e.keyB, e.nameKeyA, e.nameKeyB, e.timestampbits|_TSB_LOCAL_REMOVAL, nil, true)
			}
		}
	}
	// To avoid memory churn, the localRemovals scratchpads are allocated just
	// once and passed in to the workers.
	for len(store.tombstoneDiscardState.localRemovals) <= int(workerMax) {
		store.tombstoneDiscardState.localRemovals = append(store.tombstoneDiscardState.localRemovals, make([]groupLocalRemovalEntry, store.tombstoneDiscardState.batchSize))
	}
	var abort uint32
	wg := &sync.WaitGroup{}
	wg.Add(int(workerMax + 1))
	for worker := uint64(0); worker <= workerMax; worker++ {
		go func(worker uint64) {
			localRemovals := store.tombstoneDiscardState.localRemovals[worker]
			partitionBegin := (partitionMax + 1) / (workerMax + 1) * worker
			for partition := partitionBegin; ; {
				if atomic.LoadUint32(&abort) != 0 {
					break
				}
				work(partition, worker, localRemovals)
				partition++
				if partition > partitionMax {
					partition = 0
				}
				if partition == partitionBegin {
					break
				}
			}
			wg.Done()
		}(worker)
	}
	waitChan := make(chan struct{}, 1)
	go func() {
		wg.Wait()
		close(waitChan)
	}()
	select {
	case notification := <-notifyChan:
		atomic.AddUint32(&abort, 1)
		<-waitChan
		return notification
	case <-waitChan:
		return nil
	}
}
