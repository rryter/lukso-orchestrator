package cache

import (
	"context"
	eth1Types "github.com/ethereum/go-ethereum/core/types"
	lru "github.com/hashicorp/golang-lru"
	"github.com/lukso-network/lukso-orchestrator/shared/types"
	"github.com/pkg/errors"
	"sync"
)

var (
	// maxPanHeaderCacheSize with 1024 consensus infos will be 1024 * 1.5kb.
	maxPanHeaderCacheSize = 1 << 10

	// errInvalidSlot
	errInvalidSlot = errors.New("Invalid slot")
)

// PanHeaderCache
type PanHeaderCache struct {
	cache *lru.Cache
	lock  sync.RWMutex
}

// NewPanHeaderCache initializes the map and underlying cache.
func NewPanHeaderCache() *PanHeaderCache {
	cache, err := lru.New(maxPanHeaderCacheSize)
	if err != nil {
		panic(err)
	}
	return &PanHeaderCache{
		cache: cache,
	}
}

// Put
func (c *PanHeaderCache) Put(ctx context.Context, slot uint64, header *eth1Types.Header) error {
	copyHeader := types.CopyHeader(header)
	c.cache.Add(slot, copyHeader)
	return nil
}

// Get
func (c *PanHeaderCache) Get(ctx context.Context, slot uint64) (*eth1Types.Header, error) {
	item, exists := c.cache.Get(slot)
	if exists && item != nil {
		header := item.(*eth1Types.Header)
		copiedHeader := types.CopyHeader(header)
		return copiedHeader, nil
	}
	return nil, errInvalidSlot
}