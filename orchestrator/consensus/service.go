package consensus

import (
	"context"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/lukso-network/lukso-orchestrator/orchestrator/db"
	"github.com/lukso-network/lukso-orchestrator/orchestrator/db/iface"
	"github.com/lukso-network/lukso-orchestrator/orchestrator/db/kv"
	"github.com/lukso-network/lukso-orchestrator/orchestrator/rpc/api/events"
	"github.com/lukso-network/lukso-orchestrator/orchestrator/utils"
	"github.com/lukso-network/lukso-orchestrator/shared"
	"github.com/lukso-network/lukso-orchestrator/shared/types"
	log "github.com/sirupsen/logrus"
	"sync"
	"time"
)

type databaseErrors struct {
	vanguardErr error
	pandoraErr  error
	realmErr    error
}

type invalidationWorkPayload struct {
	invalidationStartRealmSlot uint64
	fromSlot                   uint64
	possibleSkippedPair        []*events.RealmPair
	pandoraHashes              []*types.HeaderHash
	vanguardHashes             []*types.HeaderHash
	pandoraOrphans             map[uint64]*types.HeaderHash
	vanguardOrphans            map[uint64]*types.HeaderHash
}

// Service This part could be moved to other place during refactor, might be registered as a service
type Service struct {
	VanguardHeaderHashDB        iface.VanHeaderAccessDatabase
	PandoraHeaderHashDB         iface.PanHeaderAccessDatabase
	RealmDB                     iface.RealmAccessDatabase
	VanguardHeadersChan         chan *types.HeaderHash
	VanguardConsensusInfoChan   chan *types.MinimalEpochConsensusInfo
	PandoraHeadersChan          chan *types.HeaderHash
	stopChan                    chan bool
	canonicalizeChan            chan uint64
	isWorking                   bool
	canonicalizeLock            *sync.Mutex
	invalidationWorkPayloadChan chan invalidationWorkPayload
	errChan                     chan databaseErrors
	started                     bool
}

// Start service should be registered only after Pandora and Vanguard notified about:
// - consensus info (Vanguard)
// - pendingHeaders (Vanguard)
// - pendingHeaders (Pandora)
// In current implementation we use debounce to determine state of syncing
func (service *Service) Start() {
	go func() {
		for {
			stop := <-service.stopChan

			if stop {
				log.WithField("canonicalize", "stop").Info("Received stop signal")
				return
			}
		}
	}()

	// There might be multiple scenarios that will trigger different slot required to trigger the canonicalize
	service.workLoop()

	return
}

func (service *Service) Stop() error {
	service.stopChan <- true

	return nil
}

func (service *Service) Status() error {
	return nil
}

var _ shared.Service = &Service{}

func New(
	ctx context.Context,
	database db.Database,
	vanguardHeadersChan chan *types.HeaderHash,
	vanguardConsensusInfoChan chan *types.MinimalEpochConsensusInfo,
	pandoraHeadersChan chan *types.HeaderHash,
) (service *Service) {
	stopChan := make(chan bool)
	canonicalizeChain := make(chan uint64)
	invalidationWorkPayloadChan := make(chan invalidationWorkPayload, 10000)
	errChan := make(chan databaseErrors, 10000)

	return &Service{
		VanguardHeaderHashDB:      database,
		PandoraHeaderHashDB:       database,
		RealmDB:                   database,
		VanguardHeadersChan:       vanguardHeadersChan,
		VanguardConsensusInfoChan: vanguardConsensusInfoChan,
		PandoraHeadersChan:        pandoraHeadersChan,
		stopChan:                  stopChan,
		canonicalizeChan:          canonicalizeChain,
		canonicalizeLock:          &sync.Mutex{},
		// invalidationWorkPayloadChan is internal so it is created on the fly
		invalidationWorkPayloadChan: invalidationWorkPayloadChan,
		// errChan is internal so it is created on the fly
		errChan: errChan,
	}
}

// invokeInvalidation will prepare payload for crawler and will push it through the channel
// if any error occurs it will be pushed back to databaseErrorsChan
// if any skip must happen it will be send information via skipChan
func invokeInvalidation(
	vanguardHashDB db.VanguardHeaderHashDB,
	pandoraHeaderHashDB db.PandoraHeaderHashDB,
	realmDB db.RealmDB,
	fromSlot uint64,
	batchLimit uint64,
	skipChan chan bool,
	databaseErrorsChan chan databaseErrors,
	invalidationWorkPayloadChan chan invalidationWorkPayload,
) {
	possibleSkippedPair := make([]*events.RealmPair, 0)
	latestSavedVerifiedRealmSlot := realmDB.LatestVerifiedRealmSlot()
	invalidationStartRealmSlot := latestSavedVerifiedRealmSlot

	if fromSlot > latestSavedVerifiedRealmSlot {
		databaseErrorsChan <- databaseErrors{realmErr: fmt.Errorf("I cannot start invalidation without root")}

		return
	}

	log.WithField("latestSavedVerifiedRealmSlot", latestSavedVerifiedRealmSlot).
		WithField("slot", fromSlot).
		Info("Invalidation starts")

	pandoraHeaderHashes, err := pandoraHeaderHashDB.PandoraHeaderHashes(fromSlot, batchLimit)

	if nil != err {
		log.WithField("cause", "Failed to invalidate pending queue").Error(err)
		databaseErrorsChan <- databaseErrors{pandoraErr: err}

		return
	}

	vanguardBlockHashes, err := vanguardHashDB.VanguardHeaderHashes(fromSlot, batchLimit)

	if nil != err {
		log.WithField("cause", "Failed to invalidate pending queue").Error(err)
		databaseErrorsChan <- databaseErrors{vanguardErr: err}

		return
	}

	pandoraRange := len(pandoraHeaderHashes)
	vanguardRange := len(vanguardBlockHashes)

	pandoraOrphans := map[uint64]*types.HeaderHash{}
	vanguardOrphans := map[uint64]*types.HeaderHash{}

	// You wont match anything, so short circuit
	if pandoraRange < 1 || vanguardRange < 1 {
		log.WithField("pandoraRange", pandoraRange).WithField("vanguardRange", vanguardRange).
			Trace("Not enough blocks to start invalidation")

		skipChan <- true

		return
	}

	log.WithField("pandoraRange", pandoraRange).WithField("vanguardRange", vanguardRange).
		Trace("Invalidation with range of blocks")

	invalidationWorkPayloadChan <- invalidationWorkPayload{
		invalidationStartRealmSlot: invalidationStartRealmSlot,
		fromSlot:                   fromSlot,
		possibleSkippedPair:        possibleSkippedPair,
		pandoraHashes:              pandoraHeaderHashes,
		vanguardHashes:             vanguardBlockHashes,
		pandoraOrphans:             pandoraOrphans,
		vanguardOrphans:            vanguardOrphans,
	}

	return
}

// Canonicalize must be called numerous of times with different from slot
// new slots may arrive after canonicalization, so Canonicalize must be invoked again
// function must be working only on started service
func (service *Service) Canonicalize(
	fromSlot uint64,
	batchLimit uint64,
) (
	vanguardErr error,
	pandoraErr error,
	realmErr error,
) {
	if nil == service {
		realmErr = fmt.Errorf("cannot start canonicalization without service")

		return
	}

	if !service.started {
		log.WithField("tip", "use service.Start() before using Canonicalize").
			Fatal("I cannot Canonicalize on not started service")

		return
	}

	vanguardHashDB := service.VanguardHeaderHashDB
	pandoraHeaderHashDB := service.PandoraHeaderHashDB
	realmDB := service.RealmDB

	skipChan := make(chan bool)
	errChan := service.errChan

	// Short circuit, do not invalidate when databases are not present.
	if nil == vanguardHashDB || nil == pandoraHeaderHashDB || nil == realmDB {
		log.WithField("vanguardHashDB", vanguardHashDB).
			WithField("pandoraHeaderHashDB", pandoraHeaderHashDB).
			WithField("realmDB", realmDB).Warn("Databases are not present")
		return
	}

	log.Info("I am starting to Canonicalize in batches")
	select {
	case stop := <-service.stopChan:
		if stop {
			service.isWorking = false
			log.Info("I stop Invalidation")
			return
		}
	case shouldSkip := <-skipChan:
		if shouldSkip {
			return
		}
	case databaseErrorList := <-errChan:
		vanguardErr = databaseErrorList.vanguardErr
		pandoraErr = databaseErrorList.pandoraErr
		realmErr = databaseErrorList.realmErr

		return
	default:
		// If higher slot was found and is valid all the gaps below must me treated as skipped
		// Any other should be treated as pending
		// When Sharding info comes we can determine slashing and Invalid state
		// SIDE NOTE: This is invalid, when a lot of blocks were just simply not present yet due to the network traffic
		invokeInvalidation(
			vanguardHashDB,
			pandoraHeaderHashDB,
			realmDB,
			fromSlot,
			batchLimit,
			skipChan,
			errChan,
			service.invalidationWorkPayloadChan,
		)
		work := <-service.invalidationWorkPayloadChan
		handlePreparedWork(vanguardHashDB, pandoraHeaderHashDB, realmDB, work, errChan)
	}

	return
}

// workLoop should be responsible of handling multiple events and resolving them
// Assumption is that if you want to validate pending queue you should receive information from Vanguard and Pandora
// TODO: handle reorgs
// TODO: consider working on MinimalConsensusInfo
func (service *Service) workLoop() {
	var onceAtTheTime = sync.Once{}
	service.started = true
	verifiedSlotWorkLoopStart := service.RealmDB.LatestVerifiedRealmSlot()
	log.WithField("verifiedSlotWorkLoopStart", verifiedSlotWorkLoopStart).
		Info("I am starting the work loop")
	realmDB := service.RealmDB
	possiblePendingWork := make([]*types.HeaderHash, 0)

	// This is arbitrary, it may be less or more. Depends on the approach
	debounceDuration := time.Second
	// Create merged channel
	mergedChannel := merge(service.VanguardHeadersChan, service.PandoraHeadersChan)

	// Create bridge for debounce
	mergedHeadersChanBridge := make(chan interface{})
	// Provide handlers for debounce
	mergedChannelHandler := func(workHeader interface{}) {
		header, isHeaderHash := workHeader.(*types.HeaderHash)

		if !isHeaderHash {
			log.WithField("cause", "mergedChannelHandler").Warn("invalid header hash")

			return
		}

		if nil == header {
			log.WithField("cause", "mergedChannelHandler").Warn("empty header hash")
			return
		}

		latestVerifiedRealmSlot := realmDB.LatestVerifiedRealmSlot()

		// This is naive, but might workHeader
		// We need to have at least one pair to start invalidation.
		// It might lead to 2 pairs on one side, or invalidation stall,
		// But ATM I do not have quicker and better idea
		if len(possiblePendingWork) < 2 {
			log.WithField("cause", "mergedChannelHandler").Debug("not enough pending pairs")
			return
		}

		service.canonicalizeChan <- latestVerifiedRealmSlot
	}

	go func() {
		for {
			select {
			case header := <-mergedChannel:
				possiblePendingWork = append(possiblePendingWork, header)
				log.WithField("cause", "worker").
					Debug("I am pushing header to merged channel")
				mergedHeadersChanBridge <- header
			case slot := <-service.canonicalizeChan:
				possiblePendingWork = make([]*types.HeaderHash, 0)

				if !service.isWorking {
					onceAtTheTime = sync.Once{}
				}

				onceAtTheTime.Do(func() {
					defer func() {
						service.isWorking = false
					}()
					service.isWorking = true
					log.WithField("latestVerifiedSlot", slot).
						Info("I am starting canonicalization")

					vanguardErr, pandoraErr, realmErr := service.Canonicalize(slot, 50000)

					log.WithField("latestVerifiedSlot", slot).
						Info("After canonicalization")

					if nil != vanguardErr {
						log.WithField("canonicalize", "vanguardErr").Debug(vanguardErr)
					}

					if nil != pandoraErr {
						log.WithField("canonicalize", "pandoraErr").Debug(pandoraErr)
					}

					if nil != realmErr {
						log.WithField("canonicalize", "realmErr").Debug(realmErr)
					}
				})
			case stop := <-service.stopChan:
				if stop {
					log.WithField("canonicalize", "stop").Info("Received stop signal")

					return
				}
			}
		}
	}()

	// Debounce (aggregate) calls and invoke invalidation of pending queue only when needed
	go utils.Debounce(
		context.Background(),
		debounceDuration,
		mergedHeadersChanBridge,
		mergedChannelHandler,
	)
}

// handlePreparedWork should be synchronous approach
func handlePreparedWork(
	vanguardHashDB db.VanguardHeaderHashDB,
	pandoraHeaderHashDB db.PandoraHeaderHashDB,
	realmDB db.RealmDB,
	invalidationWorkPayload invalidationWorkPayload,
	errChan chan databaseErrors,
) {
	vanguardBlockHashes := invalidationWorkPayload.vanguardHashes
	pandoraHeaderHashes := invalidationWorkPayload.pandoraHashes
	invalidationStartRealmSlot := invalidationWorkPayload.invalidationStartRealmSlot
	fromSlot := invalidationWorkPayload.fromSlot
	possibleSkippedPair := invalidationWorkPayload.possibleSkippedPair
	vanguardOrphans := invalidationWorkPayload.vanguardOrphans
	pandoraOrphans := invalidationWorkPayload.pandoraOrphans

	var (
		vanguardErr error
		pandoraErr  error
		realmErr    error
	)

	// TODO: move it to memory, and save in batch
	// This is quite naive, but should work
	for index, vanguardBlockHash := range vanguardBlockHashes {
		slotToCheck := fromSlot + uint64(index)

		if len(pandoraHeaderHashes) <= index {
			break
		}

		pandoraHeaderHash := pandoraHeaderHashes[index]

		// Potentially skipped slot
		if nil == pandoraHeaderHash && nil == vanguardBlockHash {
			possibleSkippedPair = append(possibleSkippedPair, &events.RealmPair{
				Slot:          slotToCheck,
				VanguardHash:  nil,
				PandoraHashes: nil,
			})

			continue
		}

		// I dont know yet, if it is true.
		// In my opinion INVALID state is 100% accurate only with blockShard verification approach
		// TODO: add additional Sharding info check VanguardBlock -> PandoraHeaderHash when implementation on vanguard side will be ready
		if nil == pandoraHeaderHash {
			vanguardHeaderHash := &types.HeaderHash{
				HeaderHash: vanguardBlockHash.HeaderHash,
				Status:     types.Pending,
			}
			vanguardOrphans[slotToCheck] = vanguardHeaderHash

			continue
		}

		if nil == vanguardBlockHash {
			currentPandoraHeaderHash := &types.HeaderHash{
				HeaderHash: pandoraHeaderHash.HeaderHash,
				Status:     types.Pending,
			}
			pandoraOrphans[slotToCheck] = currentPandoraHeaderHash

			continue
		}

		log.WithField("slot", slotToCheck).
			WithField("hash", vanguardBlockHash.HeaderHash.String()).
			Debug("I am inserting verified vanguardBlockHash")

		vanguardErr = vanguardHashDB.SaveVanguardHeaderHash(slotToCheck, &types.HeaderHash{
			HeaderHash: vanguardBlockHash.HeaderHash,
			Status:     types.Verified,
		})

		log.WithField("slot", slotToCheck).
			WithField("hash", pandoraHeaderHash.HeaderHash.String()).
			Debug("I am inserting verified pandoraHeaderHash")

		pandoraErr = pandoraHeaderHashDB.SavePandoraHeaderHash(slotToCheck, &types.HeaderHash{
			HeaderHash: pandoraHeaderHash.HeaderHash,
			Status:     types.Verified,
		})

		if nil != vanguardErr || nil != pandoraErr {
			break
		}

		realmErr = realmDB.SaveLatestVerifiedRealmSlot(slotToCheck)
	}

	if nil != vanguardErr || nil != pandoraErr || nil != realmErr {
		log.WithField("vanguardErr", vanguardErr).
			WithField("pandoraErr", pandoraErr).
			WithField("realmErr", realmErr).
			Error("Got error during invalidation of pending queue")
		errChan <- databaseErrors{
			vanguardErr: vanguardErr,
			pandoraErr:  pandoraErr,
			realmErr:    realmErr,
		}

		return
	}

	// Resolve state of possible invalid pairs
	latestSavedVerifiedRealmSlot := realmDB.LatestVerifiedRealmSlot()
	log.WithField("possibleInvalidPairs", len(possibleSkippedPair)).
		WithField("latestVerifiedRealmSlot", latestSavedVerifiedRealmSlot).
		WithField("invalidationStartRealmSlot", invalidationStartRealmSlot).
		Info("Requeue possible invalid pairs")

	invalidationRange := latestSavedVerifiedRealmSlot - invalidationStartRealmSlot

	// All of orphans and possibleSkipped are still pending
	if 0 == invalidationRange {
		log.WithField("invalidationStartRealmSlot", invalidationStartRealmSlot).
			WithField("latestVerifiedRealmSlot", latestSavedVerifiedRealmSlot).
			Warn("I did not progress any slot")

		return
	}

	if invalidationRange < 0 {
		log.Fatal("Got wrong invalidation range. This is a fatal bug that should never happen.")

		return
	}

	// Mark all pandora Orphans as skipped
	for slot := range pandoraOrphans {
		pandoraErr = pandoraHeaderHashDB.SavePandoraHeaderHash(slot, &types.HeaderHash{
			HeaderHash: common.Hash{},
			Status:     types.Skipped,
		})
	}

	// Mark all vanguard orphans as skipped
	for slot := range vanguardOrphans {
		vanguardErr = vanguardHashDB.SaveVanguardHeaderHash(slot, &types.HeaderHash{
			HeaderHash: common.Hash{},
			Status:     types.Skipped,
		})
	}

	if nil != vanguardErr || nil != pandoraErr {
		log.WithField("vanguardErr", vanguardErr).
			WithField("pandoraErr", pandoraErr).
			Error("Got error during invalidation of possible orphans")
		errChan <- databaseErrors{
			vanguardErr: vanguardErr,
			pandoraErr:  pandoraErr,
			realmErr:    realmErr,
		}

		return
	}

	pendingPairs := make([]*events.RealmPair, 0)

	for _, pair := range possibleSkippedPair {
		if nil == pair {
			continue
		}

		if pair.Slot > latestSavedVerifiedRealmSlot {
			pendingPairs = append(pendingPairs, pair)

			continue
		}

		vanguardErr = vanguardHashDB.SaveVanguardHeaderHash(pair.Slot, &types.HeaderHash{
			Status: types.Skipped,
		})

		// TODO: when more shard will come we will need to maintain this information
		pandoraErr = pandoraHeaderHashDB.SavePandoraHeaderHash(pair.Slot, &types.HeaderHash{
			Status: types.Skipped,
		})

		if nil != vanguardErr || nil != pandoraErr {
			log.WithField("vanguardErr", vanguardErr).
				WithField("pandoraErr", pandoraErr).
				WithField("realmErr", realmErr).
				Error("Got error during invalidation of pending queue")
			break
		}
	}

	if nil != vanguardErr || nil != pandoraErr {
		log.WithField("vanguardErr", vanguardErr).
			WithField("pandoraErr", pandoraErr).
			Error("Got error during invalidation of possible skipped pairs")
		errChan <- databaseErrors{
			vanguardErr: vanguardErr,
			pandoraErr:  pandoraErr,
			realmErr:    realmErr,
		}

		return
	}

	for _, pair := range pendingPairs {
		if nil != pair.VanguardHash {
			vanguardErr = vanguardHashDB.SaveVanguardHeaderHash(pair.Slot, &types.HeaderHash{
				Status:     types.Skipped,
				HeaderHash: pair.VanguardHash.HeaderHash,
			})
		}

		// TODO: when more shard will come we will need to maintain this information
		if len(pair.PandoraHashes) > 0 && nil != pair.PandoraHashes[0] {
			pandoraErr = pandoraHeaderHashDB.SavePandoraHeaderHash(pair.Slot, &types.HeaderHash{
				Status:     types.Skipped,
				HeaderHash: pair.PandoraHashes[0].HeaderHash,
			})
		}
	}

	if nil != vanguardErr || nil != pandoraErr {
		log.WithField("vanguardErr", vanguardErr).
			WithField("pandoraErr", pandoraErr).
			Error("Got error during invalidation of pendingPairs")
		errChan <- databaseErrors{
			vanguardErr: vanguardErr,
			pandoraErr:  pandoraErr,
			realmErr:    realmErr,
		}

		return
	}

	var (
		finalVanguardBatch []*types.HeaderHash
		finalPandoraBatch  []*types.HeaderHash
	)

	// At the very end fill all vanguard and pandora nil entries as skipped ones
	// Do not fetch any higher records
	finalVanguardBatch, vanguardErr = vanguardHashDB.VanguardHeaderHashes(
		invalidationStartRealmSlot,
		invalidationRange,
	)

	finalPandoraBatch, pandoraErr = pandoraHeaderHashDB.PandoraHeaderHashes(
		invalidationStartRealmSlot,
		invalidationRange,
	)

	if nil != vanguardErr || nil != pandoraErr {
		log.WithField("vanguardErr", vanguardErr).
			WithField("pandoraErr", pandoraErr).
			WithField("realmErr", realmErr).
			Error("Got error during invalidation of pending queue")

		errChan <- databaseErrors{
			vanguardErr: vanguardErr,
			pandoraErr:  pandoraErr,
			realmErr:    realmErr,
		}

		return
	}

	for index, headerHash := range finalVanguardBatch {
		if nil != headerHash {
			continue
		}

		slotToCheck := fromSlot + uint64(index)

		if slotToCheck > latestSavedVerifiedRealmSlot {
			continue
		}

		headerHash = &types.HeaderHash{
			HeaderHash: kv.EmptyHash,
			Status:     types.Skipped,
		}

		vanguardErr = vanguardHashDB.SaveVanguardHeaderHash(slotToCheck, headerHash)
	}

	for index, headerHash := range finalPandoraBatch {
		if nil != headerHash {
			continue
		}

		slotToCheck := fromSlot + uint64(index)

		if slotToCheck > latestSavedVerifiedRealmSlot {
			continue
		}

		headerHash = &types.HeaderHash{
			HeaderHash: kv.EmptyHash,
			Status:     types.Skipped,
		}
		pandoraErr = pandoraHeaderHashDB.SavePandoraHeaderHash(slotToCheck, headerHash)
	}

	if nil != vanguardErr || nil != pandoraErr {
		log.WithField("vanguardErr", vanguardErr).
			WithField("pandoraErr", pandoraErr).
			WithField("realmErr", realmErr).
			Error("Got error during invalidation of final Vanguard or Pandora batch")

		errChan <- databaseErrors{
			vanguardErr: vanguardErr,
			pandoraErr:  pandoraErr,
			realmErr:    realmErr,
		}

		return
	}

	log.WithField("highestCheckedSlot", latestSavedVerifiedRealmSlot).
		Info("I have resolved Canonicalize")
}

func merge(cs ...<-chan *types.HeaderHash) <-chan *types.HeaderHash {
	out := make(chan *types.HeaderHash)
	var wg sync.WaitGroup
	wg.Add(len(cs))
	for _, c := range cs {
		go func(c <-chan *types.HeaderHash) {
			for v := range c {
				out <- v
			}
			wg.Done()
		}(c)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}