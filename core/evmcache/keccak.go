// Copyright 2026 The go-ethereum Authors
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
// along with the go-ethereum library. If not, see <https://www.gnu.org/licenses/>.

package evmcache

import (
	"bytes"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
)

// keccakEntry holds one cached SHA3 result. The full input is stored so a
// hit is never trusted on the table index alone.
type keccakEntry struct {
	input    [MaxKeccakInput]byte
	inLen    uint8
	hash     common.Hash
	occupied bool
}

// KeccakCache is a fixed-size direct-mapped cache of SHA3/KECCAK256
// results for inputs of at most MaxKeccakInput bytes. It is intended to be
// hooked into the SHA3 opcode only — never the general keccak used by trie
// hashing, where inputs are near-unique.
//
// Entries never go stale (keccak is a pure function). On collision the
// previous occupant is overwritten; there is no LRU bookkeeping.
type KeccakCache struct {
	enabled atomic.Bool

	mu    sync.RWMutex
	table []keccakEntry
	mask  uint64
	stats counters
}

// NewKeccakCache allocates a direct-mapped table with at least slots
// entries (rounded up to a power of two). The cache starts enabled.
func NewKeccakCache(slots int) *KeccakCache {
	if slots <= 0 {
		slots = DefaultKeccakSlots
	}
	slots = nextPow2(slots)
	c := &KeccakCache{
		table: make([]keccakEntry, slots),
		mask:  uint64(slots - 1),
	}
	c.enabled.Store(true)
	return c
}

// Enabled reports whether lookups are live.
func (c *KeccakCache) Enabled() bool {
	return c != nil && c.enabled.Load()
}

// SetEnabled toggles the cache at runtime without dropping the table.
func (c *KeccakCache) SetEnabled(on bool) {
	if c == nil {
		return
	}
	c.enabled.Store(on)
}

// Slots returns the table capacity.
func (c *KeccakCache) Slots() int {
	if c == nil {
		return 0
	}
	return len(c.table)
}

// Get returns a cached hash if the full input bytes match the occupant.
// Inputs longer than MaxKeccakInput (or a disabled cache) always miss.
func (c *KeccakCache) Get(data []byte) (common.Hash, bool) {
	if !c.Enabled() || len(data) > MaxKeccakInput {
		return common.Hash{}, false
	}
	idx := keccakIndex(data) & c.mask
	c.mu.RLock()
	e := &c.table[idx]
	ok := e.occupied && int(e.inLen) == len(data) && bytes.Equal(e.input[:e.inLen], data)
	var h common.Hash
	if ok {
		h = e.hash
	}
	c.mu.RUnlock()
	if ok {
		c.stats.hit()
		return h, true
	}
	c.stats.miss()
	return common.Hash{}, false
}

// Put stores the hash of data. Inputs longer than MaxKeccakInput are
// ignored. The input bytes are copied; the caller may reuse data.
func (c *KeccakCache) Put(data []byte, hash common.Hash) {
	if !c.Enabled() || len(data) > MaxKeccakInput {
		return
	}
	idx := keccakIndex(data) & c.mask
	c.mu.Lock()
	e := &c.table[idx]
	if e.occupied && (int(e.inLen) != len(data) || !bytes.Equal(e.input[:e.inLen], data)) {
		c.stats.evictions.Add(1)
	}
	copy(e.input[:], data)
	e.inLen = uint8(len(data))
	e.hash = hash
	e.occupied = true
	c.mu.Unlock()
	c.stats.inserts.Add(1)
}

// Purge drops every entry.
func (c *KeccakCache) Purge() {
	if c == nil {
		return
	}
	c.mu.Lock()
	clear(c.table)
	c.mu.Unlock()
	c.stats.purges.Add(1)
}

// Stats returns a snapshot of counters.
func (c *KeccakCache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	return c.stats.snapshot()
}

// ResetStats zeroes the counters without dropping cached entries.
func (c *KeccakCache) ResetStats() {
	if c == nil {
		return
	}
	c.stats.reset()
}

// SetStatsEnabled toggles hit/miss accounting. It is off by default so the
// lookup path pays no atomic increment; insert, eviction and purge counts are
// always kept.
func (c *KeccakCache) SetStatsEnabled(on bool) {
	if c == nil {
		return
	}
	c.stats.statsOn.Store(on)
}

// StatsEnabled reports whether hit/miss accounting is on.
func (c *KeccakCache) StatsEnabled() bool {
	return c != nil && c.stats.statsOn.Load()
}

func keccakIndex(data []byte) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for _, b := range data {
		h ^= uint64(b)
		h *= prime
	}
	return h
}
