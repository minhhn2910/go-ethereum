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
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
)

// retained is one contract's committed slot map, handed to the next block's
// state object as originStorage so a cross-block SLOAD is the same map probe
// geth already does.
type retained struct {
	m map[common.Hash]common.Hash
	n int // len(m) at the last Retain, so Len can be updated without ranging
}

// StorageCache keeps per-address slot maps alive across blocks.
//
// After a block commits, each touched account's originStorage already holds
// the committed slots. Instead of throwing those maps away, Retain hands them
// to the cache and the next StateDB's newObject Adopt's them back. There is
// no separate lookup path: GetCommittedState's existing originStorage probe
// is the hit.
//
// Assumed: no Get during Commit, no reorgs. Under those, the maps can be
// shared and updated in place — no mutex, no copy-on-write, no delta/base.
type StorageCache struct {
	enabled atomic.Bool
	root    common.Hash
	slots   int
	byAddr  map[common.Address]*retained
	stats   counters
}

// NewStorageCache returns an enabled, empty cache. The slots argument is
// accepted for call-site compatibility and ignored: retained maps grow with
// the working set.
func NewStorageCache(slots int) *StorageCache {
	c := &StorageCache{byAddr: make(map[common.Address]*retained)}
	c.enabled.Store(true)
	return c
}

// Enabled reports whether adopt/retain are live.
func (c *StorageCache) Enabled() bool {
	return c != nil && c.enabled.Load()
}

// SetEnabled toggles the cache at runtime without dropping its contents. A
// disabled cache adopts nothing and ignores retain/commit.
func (c *StorageCache) SetEnabled(on bool) {
	if c == nil {
		return
	}
	c.enabled.Store(on)
}

// Root returns the state root the retained maps currently describe.
func (c *StorageCache) Root() common.Hash {
	if c == nil {
		return common.Hash{}
	}
	return c.root
}

// Len returns the number of retained slots as of the last Retain/Drop/Purge.
func (c *StorageCache) Len() int {
	if c == nil {
		return 0
	}
	return c.slots
}

// Accounts returns how many addresses currently have a retained map.
func (c *StorageCache) Accounts() int {
	if c == nil {
		return 0
	}
	return len(c.byAddr)
}

// Adopt returns the retained slot map for addr if the cache is enabled and
// tagged with root. The map is shared with the cache; the caller mutates it
// in place (originStorage only ever receives committed values). Nil means
// miss: the caller should allocate a fresh map.
func (c *StorageCache) Adopt(root common.Hash, addr common.Address) map[common.Hash]common.Hash {
	if c == nil || !c.enabled.Load() || c.root != root {
		return nil
	}
	r := c.byAddr[addr]
	if r == nil {
		return nil
	}
	return r.m
}

// Retain stores m as the committed slot map for addr. Called after
// commitStorage has merged this block's writes into originStorage.
func (c *StorageCache) Retain(addr common.Address, m map[common.Hash]common.Hash) {
	if c == nil || !c.enabled.Load() || m == nil {
		return
	}
	old := 0
	if r := c.byAddr[addr]; r != nil {
		old = r.n
	}
	n := len(m)
	c.byAddr[addr] = &retained{m: m, n: n}
	c.slots += n - old
	c.stats.inserts.Add(1)
}

// Commit tags the retained maps with newRoot. No maps are copied.
func (c *StorageCache) Commit(newRoot common.Hash) {
	if c == nil || !c.enabled.Load() {
		return
	}
	c.root = newRoot
}

// Drop forgets addr's retained map. Used for self-destructed accounts.
func (c *StorageCache) Drop(addr common.Address) {
	if c == nil || !c.enabled.Load() {
		return
	}
	r := c.byAddr[addr]
	if r == nil {
		return
	}
	c.slots -= r.n
	delete(c.byAddr, addr)
	c.stats.invalidations.Add(1)
}

// Purge drops every retained map and the root tag. Call on reorg or SetHead.
func (c *StorageCache) Purge() {
	if c == nil {
		return
	}
	c.byAddr = make(map[common.Address]*retained)
	c.slots = 0
	c.root = common.Hash{}
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

// ResetStats zeroes the counters without dropping retained maps.
func (c *StorageCache) ResetStats() {
	if c == nil {
		return
	}
	c.stats.reset()
}

// SetStatsEnabled is kept so existing call sites compile. Design D has no
// lookup path of its own, so hit/miss accounting is not collected here.
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
