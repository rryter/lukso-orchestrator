package consensus

import (
	"context"
	"github.com/ethereum/go-ethereum/event"
	"github.com/lukso-network/lukso-orchestrator/orchestrator/cache"
	testDB "github.com/lukso-network/lukso-orchestrator/orchestrator/db/testing"
	"github.com/lukso-network/lukso-orchestrator/shared/testutil"
	"github.com/lukso-network/lukso-orchestrator/shared/types"
	"testing"
)

type mockFeedService struct {
	headerInfoFeed event.Feed
	shardInfoFeed  event.Feed
	scope          event.SubscriptionScope
}

func (mc *mockFeedService) SubscribeHeaderInfoEvent(ch chan<- *types.PandoraHeaderInfo) event.Subscription {
	return mc.scope.Track(mc.headerInfoFeed.Subscribe(ch))
}

func (mc *mockFeedService) SubscribeShardInfoEvent(ch chan<- *types.VanguardShardInfo) event.Subscription {
	return mc.scope.Track(mc.shardInfoFeed.Subscribe(ch))
}

func setup(ctx context.Context, t *testing.T) (*Service, *mockFeedService) {
	testDB := testDB.SetupDB(t)
	mfs := new(mockFeedService)

	cfg := &Config{
		VerifiedSlotInfoDB:           testDB,
		InvalidSlotInfoDB:            testDB,
		VanguardPendingShardingCache: cache.NewVanShardInfoCache(1024),
		PandoraPendingHeaderCache:    cache.NewPanHeaderCache(),
		VanguardShardFeed:            mfs,
		PandoraHeaderFeed:            mfs,
	}

	return New(ctx, cfg), mfs
}

func getHeaderInfosAndShardInfos(fromSlot uint64, num uint64) ([]*types.PandoraHeaderInfo, []*types.VanguardShardInfo) {
	headerInfos := make([]*types.PandoraHeaderInfo, 0)
	vanShardInfos := make([]*types.VanguardShardInfo, 0)

	for i := fromSlot; i < num; i++ {
		headerInfo := new(types.PandoraHeaderInfo)
		headerInfo.Header = testutil.NewEth1Header(i)
		headerInfo.Slot = i
		headerInfos = append(headerInfos, headerInfo)

		vanShardInfo := testutil.NewVanguardShardInfo(i, headerInfo.Header)
		vanShardInfos = append(vanShardInfos, vanShardInfo)

	}
	return headerInfos, vanShardInfos
}
