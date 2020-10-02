package indexer

import (
	"errors"
	"fmt"

	"github.com/filecoin-project/lotus/chain/types"
)

var _ = fmt.Printf

var ErrCacheEmpty = errors.New("cache empty")
var ErrAddOutOfOrder = errors.New("added tipset height lower than current head")
var ErrRevertOutOfOrder = errors.New("reverted tipset does not match current head")

// TipSetCache is a cache of recent tipsets that can keep track of reversions.
// Inspired by tipSetCache in Lotus chain/events package.
type TipSetCache struct {
	buffer  []*types.TipSet
	idxHead int // idxHead is the current position of the head tipset in the buffer
	len     int // len is the number of items in the cache
}

func NewTipSetCache(size int) *TipSetCache {
	return &TipSetCache{
		buffer: make([]*types.TipSet, size),
	}
}

// Head returns the tipset at the head of the cache.
func (c *TipSetCache) Head() (*types.TipSet, error) {
	if c.len == 0 {
		return nil, ErrCacheEmpty
	}
	return c.buffer[c.idxHead], nil
}

// Tail returns the tipset at the tail of the cache.
func (c *TipSetCache) Tail() (*types.TipSet, error) {
	if c.len == 0 {
		return nil, ErrCacheEmpty
	}
	idxTail := normalModulo(c.idxHead-c.len+1, len(c.buffer))
	return c.buffer[idxTail], nil
}

// Add adds a new tipset which becomes the new head of the cache. If the buffer is full, the tail
// being evicted is also returned.
func (c *TipSetCache) Add(ts *types.TipSet) (*types.TipSet, error) {
	if c.len == 0 {
		// Special case for zero length caches, simply pass back the added tipset
		if len(c.buffer) == 0 {
			return ts, nil
		}

		c.buffer[c.idxHead] = ts
		c.len++
		return nil, nil
	}

	headHeight := c.buffer[c.idxHead].Height()
	if headHeight >= ts.Height() {
		return nil, ErrAddOutOfOrder
	}

	c.idxHead = normalModulo(c.idxHead+1, len(c.buffer))
	old := c.buffer[c.idxHead]
	c.buffer[c.idxHead] = ts
	if c.len < len(c.buffer) {
		c.len++
	}
	return old, nil
}

// Revert removes the head tipset
func (c *TipSetCache) Revert(ts *types.TipSet) error {
	if c.len == 0 {
		return nil
	}

	// Can only revert the most recent tipset
	if c.buffer[c.idxHead] != ts {
		return ErrRevertOutOfOrder
	}

	c.idxHead = normalModulo(c.idxHead-1, len(c.buffer))
	c.len--

	return nil
}

// Len returns the number of tipsets in the cache. This will never exceed the size of the cache.
func (c *TipSetCache) Len() int {
	return c.len
}

// Reset removes all tipsets from the cache
func (c *TipSetCache) Reset() {
	for i := range c.buffer {
		c.buffer[i] = nil
	}
	c.idxHead = 0
	c.len = 0
}

func normalModulo(n, m int) int {
	return ((n % m) + m) % m
}