package vanguardchain

import (
	"context"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/lukso-network/lukso-orchestrator/shared/types"
	eth "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
)

// OnNewConsensusInfo :
//	- sends the new consensus info to all subscribed pandora clients
//  - store consensus info into cache as well as into kv consensusInfoDB
func (s *Service) OnNewConsensusInfo(ctx context.Context, consensusInfo *types.MinimalEpochConsensusInfo) {
	nsent := s.consensusInfoFeed.Send(consensusInfo)
	log.WithField("nsent", nsent).Trace("Send consensus info to subscribers")

	if err := s.consensusInfoDB.SaveConsensusInfo(ctx, consensusInfo); err != nil {
		log.WithError(err).Warn("failed to save consensus info into consensusInfoDB!")
		return
	}
}

func (s *Service) OnNewPendingVanguardBlock(ctx context.Context, block *eth.BeaconBlock) {
	if nil == block {
		err := fmt.Errorf("block cannot be nil")
		log.WithError(err).Warn("failed to save vanguard block hash")
		return
	}

	blockHash, err := block.HashTreeRoot()

	if nil != err {
		log.WithError(err).Warn("failed to save vanguard block hash")
		return
	}

	hash := common.BytesToHash(blockHash[:])
	headerHash := &types.HeaderHash{
		HeaderHash: hash,
		Status:     types.Pending,
	}

	err = s.vanguardHeaderHashDB.SaveVanguardHeaderHash(uint64(block.Slot), headerHash)

	if nil != err {
		log.WithError(err).Warn("failed to save vanguard block hash")
		return
	}
}