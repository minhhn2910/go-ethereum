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
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestStorageCacheHitMiss(t *testing.T) {
	c := NewStorageCache(16)
	root := common.HexToHash("0x01")
	addr := common.Address{0xaa}
	slot := common.HexToHash("0x02")
	val := common.HexToHash("0x03")

	if _, ok := c.Get(root, addr, slot); ok {
		t.Fatal("empty cache hit")
	}
	if !c.BeginCommit(common.Hash{}) {
		t.Fatal("empty cache should accept first commit")
	}
	c.Put(addr, slot, val)
	c.EndCommit(root)

	got, ok := c.Get(root, addr, slot)
	if !ok || got != val {
		t.Fatalf("hit = %v %v, want %v true", got, ok, val)
	}
	// Wrong root is a miss, never a stale hit.
	if _, ok := c.Get(common.HexToHash("0xff"), addr, slot); ok {
		t.Fatal("hit with mismatched root")
	}
	// Address is part of the key.
	if _, ok := c.Get(root, common.Address{0xbb}, slot); ok {
		t.Fatal("hit with different address, same slot")
	}
}

func TestStorageCacheDisable(t *testing.T) {
	c := NewStorageCache(8)
	c.Put(common.Address{1}, common.Hash{2}, common.Hash{3})
	c.EndCommit(common.Hash{4})
	c.SetEnabled(false)
	if c.Enabled() {
		t.Fatal("expected disabled")
	}
	if _, ok := c.Get(common.Hash{4}, common.Address{1}, common.Hash{2}); ok {
		t.Fatal("disabled cache must miss")
	}
	c.SetEnabled(true)
	if _, ok := c.Get(common.Hash{4}, common.Address{1}, common.Hash{2}); !ok {
		t.Fatal("re-enabled cache should still hold the entry")
	}
}

func TestStorageCacheInvalidateAccount(t *testing.T) {
	c := NewStorageCache(32)
	addr := common.Address{0x42}
	c.Put(addr, common.Hash{1}, common.Hash{9})
	c.Put(addr, common.Hash{2}, common.Hash{8})
	c.EndCommit(common.Hash{1})

	// The account self-destructs in the next block.
	c.InvalidateAccount(addr)
	c.EndCommit(common.Hash{2})
	if _, ok := c.Get(common.Hash{2}, addr, common.Hash{1}); ok {
		t.Fatal("invalidated account still hits")
	}
	// A later block writing the same account is live again.
	c.Put(addr, common.Hash{1}, common.Hash{7})
	c.EndCommit(common.Hash{3})
	got, ok := c.Get(common.Hash{3}, addr, common.Hash{1})
	if !ok || got != (common.Hash{7}) {
		t.Fatalf("got %v %v", got, ok)
	}
}

// A single block may both destroy and recreate an account. The staged slots
// are the new incarnation's and must survive the invalidation.
func TestStorageCacheDestructAndRecreateInOneCommit(t *testing.T) {
	c := NewStorageCache(32)
	addr := common.Address{0x42}
	c.Put(addr, common.Hash{1}, common.Hash{9})
	c.Put(addr, common.Hash{2}, common.Hash{8})
	c.EndCommit(common.Hash{1})

	c.InvalidateAccount(addr)
	c.Put(addr, common.Hash{1}, common.Hash{7})
	c.EndCommit(common.Hash{2})

	got, ok := c.Get(common.Hash{2}, addr, common.Hash{1})
	if !ok || got != (common.Hash{7}) {
		t.Fatalf("recreated slot = %v %v, want 0x07 true", got, ok)
	}
	if _, ok := c.Get(common.Hash{2}, addr, common.Hash{2}); ok {
		t.Fatal("slot from the destroyed incarnation still hits")
	}
}

// Staged slots must stay invisible until EndCommit publishes them; that is
// what stops a reverted call from being observable.
func TestStorageCacheStagedNotVisibleUntilCommit(t *testing.T) {
	c := NewStorageCache(16)
	addr := common.Address{0xaa}
	slot, val := common.Hash{1}, common.Hash{9}

	c.EndCommit(common.Hash{1})
	c.Put(addr, slot, val)
	if _, ok := c.Get(common.Hash{1}, addr, slot); ok {
		t.Fatal("staged slot visible before EndCommit")
	}
	c.EndCommit(common.Hash{2})
	if got, ok := c.Get(common.Hash{2}, addr, slot); !ok || got != val {
		t.Fatalf("published slot = %v %v, want %v true", got, ok, val)
	}
}

func TestStorageCachePurge(t *testing.T) {
	c := NewStorageCache(8)
	c.Put(common.Address{1}, common.Hash{1}, common.Hash{1})
	c.EndCommit(common.Hash{2})
	c.Purge()
	if c.Root() != (common.Hash{}) {
		t.Fatal("purge should drop the root tag")
	}
	if _, ok := c.Get(common.Hash{2}, common.Address{1}, common.Hash{1}); ok {
		t.Fatal("purged cache still hits")
	}
}

func TestStorageCacheOverBudgetEvicts(t *testing.T) {
	c := NewStorageCache(1)
	a1, a2 := common.Address{1}, common.Address{2}
	c.Put(a1, common.Hash{1}, common.Hash{9})
	c.Put(a2, common.Hash{1}, common.Hash{8})
	c.EndCommit(common.Hash{1})
	if st := c.Stats(); st.Evictions == 0 {
		t.Fatal("expected an eviction on a 1-entry budget")
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("cache holds %d entries, want 1", got)
	}
}

// When the cache is over budget the slots this block touched must be the ones
// retained, since they are the likeliest to be read again.
func TestStorageCacheEvictionKeepsCurrentBlock(t *testing.T) {
	const budget = 4
	c := NewStorageCache(budget)
	addr := common.Address{1}
	key := func(i int) common.Hash { return common.Hash{0xff, byte(i)} }

	for i := 0; i < budget; i++ {
		c.Put(addr, key(i), common.Hash{byte(i)})
	}
	c.EndCommit(common.Hash{1})

	for i := budget; i < 2*budget; i++ {
		c.Put(addr, key(i), common.Hash{byte(i)})
	}
	c.EndCommit(common.Hash{2})

	if got := c.Len(); got != budget {
		t.Fatalf("cache holds %d entries, want %d", got, budget)
	}
	for i := budget; i < 2*budget; i++ {
		if got, ok := c.Get(common.Hash{2}, addr, key(i)); !ok || got != (common.Hash{byte(i)}) {
			t.Fatalf("slot %d from the current block was evicted (%v %v)", i, got, ok)
		}
	}
}

// The base is only rebuilt occasionally, so entries have to stay readable
// whether they currently sit in the delta or have been folded into the base.
func TestStorageCacheSurvivesFolds(t *testing.T) {
	const (
		budget   = 1024
		blocks   = 40
		perBlock = 8
	)
	c := NewStorageCache(budget)
	addr := common.Address{1}
	key := func(i int) common.Hash { return common.Hash{0xff, byte(i >> 8), byte(i)} }
	val := func(i int) common.Hash { return common.Hash{0x01, byte(i >> 8), byte(i)} }

	var root common.Hash
	for blk := 0; blk < blocks; blk++ {
		for j := 0; j < perBlock; j++ {
			i := blk*perBlock + j
			c.Put(addr, key(i), val(i))
		}
		root = common.Hash{byte(blk + 1)}
		c.EndCommit(root)
	}

	if got, want := c.Len(), blocks*perBlock; got != want {
		t.Fatalf("cache holds %d entries, want %d", got, want)
	}
	for i := 0; i < blocks*perBlock; i++ {
		if got, ok := c.Get(root, addr, key(i)); !ok || got != val(i) {
			t.Fatalf("slot %d = %v %v, want %v true", i, got, ok, val(i))
		}
	}
	if st := c.Stats(); st.Evictions != 0 {
		t.Fatalf("evicted while inside the budget: %+v", st)
	}
}

// A self-destruct has to reach entries that were already folded into the base,
// not just the recent ones in the delta.
func TestStorageCacheInvalidateReachesBase(t *testing.T) {
	c := NewStorageCache(1024)
	addr := common.Address{0x42}
	other := common.Address{0x43}
	c.Put(addr, common.Hash{1}, common.Hash{9})
	c.Put(other, common.Hash{1}, common.Hash{8})
	c.EndCommit(common.Hash{1})

	// Idle commits push the entries out of the delta and into the base.
	var root common.Hash
	for i := 0; i <= foldEveryNth; i++ {
		root = common.Hash{0xa0, byte(i)}
		c.EndCommit(root)
	}
	if _, ok := c.Get(root, addr, common.Hash{1}); !ok {
		t.Fatal("entry lost across the fold")
	}

	c.InvalidateAccount(addr)
	root = common.Hash{0xb0}
	c.EndCommit(root)
	if _, ok := c.Get(root, addr, common.Hash{1}); ok {
		t.Fatal("self-destructed slot survived in the base")
	}
	if _, ok := c.Get(root, other, common.Hash{1}); !ok {
		t.Fatal("an unrelated account was dropped")
	}
}

func TestStorageCacheStatsGating(t *testing.T) {
	c := NewStorageCache(8)
	addr := common.Address{0xaa}
	slot, val := common.Hash{1}, common.Hash{9}
	c.Put(addr, slot, val)
	c.EndCommit(common.Hash{1})

	c.Get(common.Hash{1}, addr, slot)
	c.Get(common.Hash{1}, addr, common.Hash{0xee})
	st := c.Stats()
	if st.Hits != 0 || st.Misses != 0 {
		t.Fatalf("hits/misses counted while stats are off: %+v", st)
	}
	if st.Inserts == 0 {
		t.Fatal("inserts should be counted regardless of the stats flag")
	}

	c.SetStatsEnabled(true)
	c.Get(common.Hash{1}, addr, slot)
	c.Get(common.Hash{1}, addr, common.Hash{0xee})
	if st := c.Stats(); st.Hits != 1 || st.Misses != 1 {
		t.Fatalf("hits=%d misses=%d, want 1 and 1", st.Hits, st.Misses)
	}
}

func TestStorageCacheRace(t *testing.T) {
	c := NewStorageCache(256)
	c.SetStatsEnabled(true)
	root := common.Hash{1}
	c.EndCommit(root)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id byte) {
			defer wg.Done()
			addr := common.Address{id}
			for j := 0; j < 200; j++ {
				slot := common.Hash{byte(j)}
				c.Put(addr, slot, common.Hash{id, byte(j)})
				c.Get(root, addr, slot)
				c.Get(c.Root(), addr, slot)
			}
		}(byte(i + 1))
	}
	// Republish snapshots underneath the readers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			c.EndCommit(root)
		}
	}()
	wg.Wait()
}

func TestKeccakCacheHitRequiresFullInput(t *testing.T) {
	c := NewKeccakCache(32)
	in := make([]byte, 64)
	in[0] = 0x11
	hash := common.HexToHash("0xdead")
	c.Put(in, hash)

	got, ok := c.Get(in)
	if !ok || got != hash {
		t.Fatalf("hit = %v %v", got, ok)
	}
	in[1] = 0x22
	if _, ok := c.Get(in); ok {
		t.Fatal("truncated/partial key must not hit")
	}
}

func TestKeccakCacheRejectsLongInput(t *testing.T) {
	c := NewKeccakCache(8)
	long := make([]byte, MaxKeccakInput+1)
	c.Put(long, common.Hash{1})
	if _, ok := c.Get(long); ok {
		t.Fatal("inputs > 64 bytes must not be cached")
	}
}

func TestKeccakCacheDisable(t *testing.T) {
	c := NewKeccakCache(8)
	c.Put([]byte{1, 2, 3}, common.Hash{9})
	c.SetEnabled(false)
	if _, ok := c.Get([]byte{1, 2, 3}); ok {
		t.Fatal("disabled keccak cache must miss")
	}
}

func TestKeccakCacheRace(t *testing.T) {
	c := NewKeccakCache(64)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id byte) {
			defer wg.Done()
			in := make([]byte, 64)
			in[0] = id
			for j := 0; j < 200; j++ {
				in[1] = byte(j)
				c.Put(in, common.Hash{id, byte(j)})
				c.Get(in)
			}
		}(byte(i + 1))
	}
	wg.Wait()
}

func TestNilCachesAreDisabled(t *testing.T) {
	var s *StorageCache
	var k *KeccakCache
	if s.Enabled() || k.Enabled() {
		t.Fatal("nil caches must report disabled")
	}
	if _, ok := s.Get(common.Hash{}, common.Address{}, common.Hash{}); ok {
		t.Fatal("nil storage get")
	}
	if _, ok := k.Get(nil); ok {
		t.Fatal("nil keccak get")
	}
}
