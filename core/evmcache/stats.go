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

import "sync/atomic"

// Stats is a snapshot of cache counters.
type Stats struct {
	Hits          uint64
	Misses        uint64
	Inserts       uint64
	Evictions     uint64
	Invalidations uint64
	Purges        uint64
}

// HitRate returns hits / (hits+misses). Zero if no lookups.
func (s Stats) HitRate() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}

// StoragePathStats counts where committed-state reads were resolved. Attach
// to a StateDB for experiment S-B; nil means no accounting.
type StoragePathStats struct {
	Dirty   atomic.Uint64 // served from dirtyStorage (current tx writes)
	Pending atomic.Uint64 // served from pendingStorage (earlier tx in block)
	Origin  atomic.Uint64 // served from originStorage (this block or retained)
	Cache   atomic.Uint64 // unused under Design D (cross-block hits land in Origin)
	Backend atomic.Uint64 // snapshot / trie / disk
}

// Snapshot returns a copy of the counters.
func (s *StoragePathStats) Snapshot() (dirty, pending, origin, cache, backend uint64) {
	if s == nil {
		return
	}
	return s.Dirty.Load(), s.Pending.Load(), s.Origin.Load(), s.Cache.Load(), s.Backend.Load()
}

// Reset zeroes all counters.
func (s *StoragePathStats) Reset() {
	if s == nil {
		return
	}
	s.Dirty.Store(0)
	s.Pending.Store(0)
	s.Origin.Store(0)
	s.Cache.Store(0)
	s.Backend.Store(0)
}

type counters struct {
	statsOn                                                 atomic.Bool
	hits, misses, inserts, evictions, invalidations, purges atomic.Uint64
}

// hit and miss sit on the lookup path, where an unconditional atomic
// read-modify-write is a measurable share of the total cost. They are only
// counted once stats are explicitly enabled; the guard is a plain load.
func (c *counters) hit() {
	if c.statsOn.Load() {
		c.hits.Add(1)
	}
}

func (c *counters) miss() {
	if c.statsOn.Load() {
		c.misses.Add(1)
	}
}

func (c *counters) snapshot() Stats {
	return Stats{
		Hits:          c.hits.Load(),
		Misses:        c.misses.Load(),
		Inserts:       c.inserts.Load(),
		Evictions:     c.evictions.Load(),
		Invalidations: c.invalidations.Load(),
		Purges:        c.purges.Load(),
	}
}

func (c *counters) reset() {
	c.hits.Store(0)
	c.misses.Store(0)
	c.inserts.Store(0)
	c.evictions.Store(0)
	c.invalidations.Store(0)
	c.purges.Store(0)
}
