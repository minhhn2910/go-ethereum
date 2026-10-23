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

func seed(c *StorageCache, root common.Hash, addr common.Address, slot, val common.Hash) {
	m := c.Adopt(root, addr)
	if m == nil {
		m = make(map[common.Hash]common.Hash)
	}
	m[slot] = val
	c.Retain(addr, m)
	c.Commit(root)
}

func lookup(c *StorageCache, root common.Hash, addr common.Address, slot common.Hash) (common.Hash, bool) {
	m := c.Adopt(root, addr)
	if m == nil {
		return common.Hash{}, false
	}
	v, ok := m[slot]
	return v, ok
}

func TestStorageCacheHitMiss(t *testing.T) {
	c := NewStorageCache(16)
	root := common.HexToHash("0x01")
	addr := common.Address{0xaa}
	slot := common.HexToHash("0x02")
	val := common.HexToHash("0x03")

	if _, ok := lookup(c, root, addr, slot); ok {
		t.Fatal("empty cache hit")
	}
	seed(c, root, addr, slot, val)

	got, ok := lookup(c, root, addr, slot)
	if !ok || got != val {
		t.Fatalf("hit = %v %v, want %v true", got, ok, val)
	}
	if _, ok := lookup(c, common.HexToHash("0xff"), addr, slot); ok {
		t.Fatal("hit with mismatched root")
	}
	if _, ok := lookup(c, root, common.Address{0xbb}, slot); ok {
		t.Fatal("hit with different address, same slot")
	}
}

func TestStorageCacheDisable(t *testing.T) {
	c := NewStorageCache(8)
	root := common.Hash{4}
	addr, slot, val := common.Address{1}, common.Hash{2}, common.Hash{3}
	seed(c, root, addr, slot, val)
	c.SetEnabled(false)
	if c.Enabled() {
		t.Fatal("expected disabled")
	}
	if _, ok := lookup(c, root, addr, slot); ok {
		t.Fatal("disabled cache must miss")
	}
	c.SetEnabled(true)
	if _, ok := lookup(c, root, addr, slot); !ok {
		t.Fatal("re-enabled cache should still hold the entry")
	}
}

func TestStorageCacheDrop(t *testing.T) {
	c := NewStorageCache(32)
	addr := common.Address{0x42}
	root1, root2 := common.Hash{1}, common.Hash{2}
	seed(c, root1, addr, common.Hash{1}, common.Hash{9})
	seed(c, root1, addr, common.Hash{2}, common.Hash{8})

	c.Drop(addr)
	c.Commit(root2)
	if _, ok := lookup(c, root2, addr, common.Hash{1}); ok {
		t.Fatal("dropped account still hits")
	}
	seed(c, root2, addr, common.Hash{1}, common.Hash{7})
	got, ok := lookup(c, root2, addr, common.Hash{1})
	if !ok || got != (common.Hash{7}) {
		t.Fatalf("got %v %v", got, ok)
	}
}

func TestStorageCacheDropThenRetain(t *testing.T) {
	c := NewStorageCache(32)
	addr := common.Address{0x42}
	seed(c, common.Hash{1}, addr, common.Hash{1}, common.Hash{9})
	seed(c, common.Hash{1}, addr, common.Hash{2}, common.Hash{8})

	c.Drop(addr)
	fresh := map[common.Hash]common.Hash{common.Hash{1}: common.Hash{7}}
	c.Retain(addr, fresh)
	c.Commit(common.Hash{2})

	got, ok := lookup(c, common.Hash{2}, addr, common.Hash{1})
	if !ok || got != (common.Hash{7}) {
		t.Fatalf("recreated slot = %v %v, want 0x07 true", got, ok)
	}
	if _, ok := lookup(c, common.Hash{2}, addr, common.Hash{2}); ok {
		t.Fatal("slot from the destroyed incarnation still hits")
	}
}

func TestStorageCacheInPlaceGrow(t *testing.T) {
	c := NewStorageCache(16)
	addr := common.Address{0xaa}
	root1, root2 := common.Hash{1}, common.Hash{2}
	slot, val := common.Hash{1}, common.Hash{9}

	c.Commit(root1)
	m := c.Adopt(root1, addr)
	if m != nil {
		t.Fatal("empty cache should not adopt")
	}
	m = map[common.Hash]common.Hash{slot: val}
	c.Retain(addr, m)
	c.Commit(root1)

	// The adopted map is the same pointer; growing it in place is the hit.
	held := c.Adopt(root1, addr)
	if held == nil {
		t.Fatal("expected adopt")
	}
	held[common.Hash{2}] = common.Hash{8}
	c.Retain(addr, held)
	c.Commit(root2)

	if got := c.Len(); got != 2 {
		t.Fatalf("len = %d, want 2", got)
	}
	if _, ok := lookup(c, root2, addr, common.Hash{2}); !ok {
		t.Fatal("in-place write lost")
	}
}

func TestStorageCachePurge(t *testing.T) {
	c := NewStorageCache(8)
	seed(c, common.Hash{2}, common.Address{1}, common.Hash{1}, common.Hash{1})
	c.Purge()
	if c.Root() != (common.Hash{}) {
		t.Fatal("purge should drop the root tag")
	}
	if _, ok := lookup(c, common.Hash{2}, common.Address{1}, common.Hash{1}); ok {
		t.Fatal("purged cache still hits")
	}
}

func TestStorageCacheRetag(t *testing.T) {
	c := NewStorageCache(8)
	addr := common.Address{1}
	seed(c, common.Hash{1}, addr, common.Hash{1}, common.Hash{9})
	c.Commit(common.Hash{2})
	if c.Root() != (common.Hash{2}) {
		t.Fatalf("root = %s", c.Root())
	}
	if _, ok := lookup(c, common.Hash{1}, addr, common.Hash{1}); ok {
		t.Fatal("old root still hits")
	}
	if _, ok := lookup(c, common.Hash{2}, addr, common.Hash{1}); !ok {
		t.Fatal("new root missed")
	}
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
	if s.Adopt(common.Hash{}, common.Address{}) != nil {
		t.Fatal("nil storage adopt")
	}
	if _, ok := k.Get(nil); ok {
		t.Fatal("nil keccak get")
	}
}
