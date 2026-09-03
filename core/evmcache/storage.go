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
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
)

// storageKey identifies a committed storage slot. It is a plain comparable
// struct so that Go's runtime hasher (AES-accelerated on amd64) does the
// hashing, rather than a hand-rolled byte loop.
type storageKey struct {
	addr common.Address
	slot common.Hash
}

// storageSnapshot is the published contents of the cache at one state root.
//
// A snapshot is immutable once handed to StorageCache.cur: readers take no
// lock, so mutating a published map would be a data race. Every commit
// therefore publishes fresh maps.
//
// The contents are split in two so that a commit does not have to copy
// everything it already holds. delta carries the slots written by the last
// few blocks and is rebuilt every commit; base carries the rest and is only
// rebuilt when delta grows past its share of the budget. Copying the whole
// thing every block costs milliseconds once the cache is large, which is far
// more than the lookups save.
type storageSnapshot struct {
	root  common.Hash
	delta map[storageKey]common.Hash
	base  map[storageKey]common.Hash
}

// StorageCache is a bounded, process-lifetime map from (address, slot) to
// the committed storage value at a tagged state root. It is consulted on
// GetCommittedState after the per-object maps miss and before the snapshot
// / trie readers. Entries are written only at block commit, so reverts
// cannot poison it.
//
// Block commit is the only writer and it runs once per block, which makes a
// copy-on-write layout affordable: writes stage into a side map and publish
// a rebuilt snapshot with a single atomic store, leaving the lookup path
// free of locks and of per-account generation bookkeeping.
//
// The cache is safe for concurrent use. It is a no-op when disabled.
type StorageCache struct {
	enabled atomic.Bool
	cur     atomic.Pointer[storageSnapshot]

	// mu guards the staging area, which only block commit touches. Readers
	// never acquire it.
	mu         sync.Mutex
	staged     map[storageKey]common.Hash
	destructed map[common.Address]struct{}
	sinceFold  int

	limit      int
	deltaLimit int
	stats      counters
}

// foldEveryNth bounds how long the base may go unrebuilt even when the delta
// stays small, so a mostly-idle chain still reclaims evicted entries.
const foldEveryNth = 32

// NewStorageCache returns a cache holding at most slots entries. Between base
// rebuilds it may hold up to an eighth more. The cache starts enabled and
// empty, with no root tag.
func NewStorageCache(slots int) *StorageCache {
	if slots <= 0 {
		slots = DefaultStorageSlots
	}
	c := &StorageCache{limit: slots, deltaLimit: slots / 8}
	c.enabled.Store(true)
	return c
}

// Enabled reports whether lookups and commits are live.
func (c *StorageCache) Enabled() bool {
	return c != nil && c.enabled.Load()
}

// SetEnabled toggles the cache at runtime without dropping its contents. A
// disabled cache misses every lookup and ignores commits.
func (c *StorageCache) SetEnabled(on bool) {
	if c == nil {
		return
	}
	c.enabled.Store(on)
}

// Root returns the state root the cache currently belongs to.
func (c *StorageCache) Root() common.Hash {
	if c == nil {
		return common.Hash{}
	}
	if s := c.cur.Load(); s != nil {
		return s.root
	}
	return common.Hash{}
}

// Slots returns the maximum number of entries the cache will hold.
func (c *StorageCache) Slots() int {
	if c == nil {
		return 0
	}
	return c.limit
}

// Len returns the number of distinct entries in the published snapshot.
func (c *StorageCache) Len() int {
	if c == nil {
		return 0
	}
	s := c.cur.Load()
	if s == nil {
		return 0
	}
	n := len(s.base)
	for k := range s.delta {
		if _, ok := s.base[k]; !ok {
			n++
		}
	}
	return n
}

// Get returns the committed value for (addr, slot) if the cache is enabled,
// tagged with root, and holds the slot. A root mismatch is a miss; it does
// not purge (the caller purges on reorg).
//
// The path is one atomic pointer load, one root comparison and at most two
// map lookups. No locks and no atomic read-modify-writes.
func (c *StorageCache) Get(root common.Hash, addr common.Address, slot common.Hash) (common.Hash, bool) {
	if c == nil || !c.enabled.Load() {
		return common.Hash{}, false
	}
	s := c.cur.Load()
	if s == nil || s.root != root {
		c.stats.miss()
		return common.Hash{}, false
	}
	key := storageKey{addr: addr, slot: slot}
	// The delta holds the most recent writes, so it wins over the base.
	if val, ok := s.delta[key]; ok {
		c.stats.hit()
		return val, true
	}
	if val, ok := s.base[key]; ok {
		c.stats.hit()
		return val, true
	}
	c.stats.miss()
	return common.Hash{}, false
}

// Put stages (addr, slot) → value for the commit in progress. The value only
// becomes visible to Get once EndCommit publishes it, which is what keeps a
// reverted call from poisoning the cache.
func (c *StorageCache) Put(addr common.Address, slot, value common.Hash) {
	if !c.Enabled() {
		return
	}
	c.mu.Lock()
	if c.staged == nil {
		c.staged = make(map[storageKey]common.Hash)
	}
	c.staged[storageKey{addr: addr, slot: slot}] = value
	c.mu.Unlock()
	c.stats.inserts.Add(1)
}

// InvalidateAccount marks addr's slots to be dropped by the commit in
// progress. Used for self-destructed accounts. A staged Put for the same
// account still wins, so an account destroyed and recreated in one block
// keeps its new slots.
func (c *StorageCache) InvalidateAccount(addr common.Address) {
	if !c.Enabled() {
		return
	}
	c.mu.Lock()
	if c.destructed == nil {
		c.destructed = make(map[common.Address]struct{})
	}
	c.destructed[addr] = struct{}{}
	c.mu.Unlock()
	c.stats.invalidations.Add(1)
}

// BeginCommit reports whether this cache may be updated from a StateDB whose
// pre-state root is origin. An empty cache (never committed) accepts the
// first commit. A mismatch means the StateDB is not the cache's chain head
// (side block, historical query) and the caller must not write.
func (c *StorageCache) BeginCommit(origin common.Hash) bool {
	if !c.Enabled() {
		return false
	}
	s := c.cur.Load()
	return s == nil || s.root == (common.Hash{}) || s.root == origin
}

// EndCommit folds the staged slots into a new snapshot and publishes it under
// newRoot. Must be called only after a successful BeginCommit and the
// corresponding Puts.
//
// The usual commit only rebuilds the delta, which is bounded at an eighth of
// the budget. The base is rebuilt when the delta outgrows that, after
// foldEveryNth commits, or when an account self-destructs.
func (c *StorageCache) EndCommit(newRoot common.Hash) {
	if !c.Enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	var base, delta map[storageKey]common.Hash
	if prev := c.cur.Load(); prev != nil {
		base, delta = prev.base, prev.delta
	}

	// Dead slots can sit anywhere, so a self-destruct forces both maps to be
	// rebuilt. It runs before this block's own writes are merged in, so an
	// account destroyed and recreated in one block keeps its new slots.
	forceFold := false
	if len(c.destructed) > 0 {
		base, delta = c.dropDead(base), c.dropDead(delta)
		forceFold = true
	}

	merged := make(map[storageKey]common.Hash, len(delta)+len(c.staged))
	for k, v := range delta {
		merged[k] = v
	}
	for k, v := range c.staged {
		merged[k] = v
	}
	delta = merged

	c.sinceFold++
	if forceFold || len(delta) > c.deltaLimit || c.sinceFold >= foldEveryNth {
		base, delta = c.fold(base, delta)
		c.sinceFold = 0
	}

	c.cur.Store(&storageSnapshot{root: newRoot, base: base, delta: delta})
	c.staged = nil
	c.destructed = nil
}

// dropDead copies m without the slots of self-destructed accounts. It must
// copy rather than delete in place, because m may still be published.
func (c *StorageCache) dropDead(m map[storageKey]common.Hash) map[storageKey]common.Hash {
	if len(m) == 0 {
		return nil
	}
	out := make(map[storageKey]common.Hash, len(m))
	var evicted uint64
	for k, v := range m {
		if _, dead := c.destructed[k.addr]; dead {
			evicted++
			continue
		}
		out[k] = v
	}
	if evicted > 0 {
		c.stats.evictions.Add(evicted)
	}
	return out
}

// fold merges delta into a fresh base and returns an empty delta. Delta
// entries are inserted first, so the slots kept when over budget are the most
// recently written ones.
func (c *StorageCache) fold(base, delta map[storageKey]common.Hash) (map[storageKey]common.Hash, map[storageKey]common.Hash) {
	size := len(base) + len(delta)
	if size > c.limit {
		size = c.limit
	}
	next := make(map[storageKey]common.Hash, size)

	var evicted uint64
	for k, v := range delta {
		if len(next) >= c.limit {
			evicted++
			continue
		}
		next[k] = v
	}
	for k, v := range base {
		if _, fresh := next[k]; fresh {
			continue
		}
		if len(next) >= c.limit {
			evicted++
			continue
		}
		next[k] = v
	}
	if evicted > 0 {
		c.stats.evictions.Add(evicted)
	}
	return next, nil
}

// Purge drops every entry and the root tag. Call on reorg or SetHead.
func (c *StorageCache) Purge() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.staged = nil
	c.destructed = nil
	c.sinceFold = 0
	c.cur.Store(nil)
	c.mu.Unlock()
	c.stats.purges.Add(1)
	c.stats.invalidations.Add(1)
}

// Stats returns a snapshot of counters.
func (c *StorageCache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	return c.stats.snapshot()
}

// ResetStats zeroes the counters without dropping cached entries.
func (c *StorageCache) ResetStats() {
	if c == nil {
		return
	}
	c.stats.reset()
}

// SetStatsEnabled toggles hit/miss accounting. It is off by default so the
// lookup path pays no atomic increment; insert, eviction, invalidation and
// purge counts are always kept.
func (c *StorageCache) SetStatsEnabled(on bool) {
	if c == nil {
		return
	}
	c.stats.statsOn.Store(on)
}

// StatsEnabled reports whether hit/miss accounting is on.
func (c *StorageCache) StatsEnabled() bool {
	return c != nil && c.stats.statsOn.Load()
}
