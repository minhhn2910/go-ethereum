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

package state

import (
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/evmcache"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

func newTestState(t *testing.T, cache *evmcache.StorageCache) (*StateDB, *CachingDB) {
	t.Helper()
	sdb := NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)
	sdb.SetStorageCache(cache)
	st, err := New(types.EmptyRootHash, sdb)
	if err != nil {
		t.Fatal(err)
	}
	return st, sdb
}

func commitReload(t *testing.T, st *StateDB, sdb *CachingDB) (*StateDB, common.Hash) {
	t.Helper()
	root, err := st.Commit(0, true)
	if err != nil {
		t.Fatal(err)
	}
	st, err = New(root, sdb)
	if err != nil {
		t.Fatal(err)
	}
	return st, root
}

func TestStorageCacheWriteThenReadSameBlock(t *testing.T) {
	cache := evmcache.NewStorageCache(64)
	st, _ := newTestState(t, cache)
	addr := common.Address{0xaa}
	slot := common.Hash{0x01}
	want := common.Hash{0x42}

	st.CreateAccount(addr)
	st.SetNonce(addr, 1)
	st.SetState(addr, slot, want)
	if got := st.GetState(addr, slot); got != want {
		t.Fatalf("same-block read = %x, want %x", got, want)
	}
	// Cache is not written mid-execution.
	if _, ok := cache.Get(st.originalRoot, addr, slot); ok {
		t.Fatal("cache populated before commit")
	}
}

func TestStorageCacheWriteBlockNReadBlockN1(t *testing.T) {
	cache := evmcache.NewStorageCache(64)
	st, sdb := newTestState(t, cache)
	addr := common.Address{0xaa}
	slot := common.Hash{0x01}
	want := common.Hash{0x42}

	st.CreateAccount(addr)
	st.SetNonce(addr, 1)
	st.SetState(addr, slot, want)
	st, root := commitReload(t, st, sdb)

	stats := st.EnablePathStats()
	got := st.GetState(addr, slot)
	if got != want {
		t.Fatalf("block N+1 read = %x, want %x", got, want)
	}
	_, _, _, cacheHits, backend := stats.Snapshot()
	if cacheHits == 0 {
		t.Fatalf("expected a cross-block cache hit, stats cache=%d backend=%d root=%s", cacheHits, backend, root)
	}
}

func TestStorageCacheZeroSlot(t *testing.T) {
	cache := evmcache.NewStorageCache(64)
	st, sdb := newTestState(t, cache)
	addr := common.Address{0xaa}
	slot := common.Hash{0x01}

	st.CreateAccount(addr)
	st.SetNonce(addr, 1)
	st.SetState(addr, slot, common.Hash{0x01})
	st, _ = commitReload(t, st, sdb)
	st.SetState(addr, slot, common.Hash{})
	st, _ = commitReload(t, st, sdb)

	if got := st.GetState(addr, slot); got != (common.Hash{}) {
		t.Fatalf("cleared slot = %x, want zero", got)
	}
}

func TestStorageCacheTwoContractsSameSlot(t *testing.T) {
	cache := evmcache.NewStorageCache(64)
	st, sdb := newTestState(t, cache)
	a, b := common.Address{0x01}, common.Address{0x02}
	slot := common.Hash{0xff}
	st.CreateAccount(a)
	st.SetNonce(a, 1)
	st.CreateAccount(b)
	st.SetNonce(b, 1)
	st.SetState(a, slot, common.Hash{0x11})
	st.SetState(b, slot, common.Hash{0x22})
	st, _ = commitReload(t, st, sdb)

	if got := st.GetState(a, slot); got != (common.Hash{0x11}) {
		t.Fatalf("contract A = %x", got)
	}
	if got := st.GetState(b, slot); got != (common.Hash{0x22}) {
		t.Fatalf("contract B = %x", got)
	}
}

func TestStorageCacheRevertDoesNotPoison(t *testing.T) {
	cache := evmcache.NewStorageCache(64)
	st, sdb := newTestState(t, cache)
	addr := common.Address{0xaa}
	slot := common.Hash{0x01}

	st.CreateAccount(addr)
	st.SetNonce(addr, 1)
	st.SetState(addr, slot, common.Hash{0x01})
	st, _ = commitReload(t, st, sdb)

	snap := st.Snapshot()
	st.SetState(addr, slot, common.Hash{0xff})
	st.RevertToSnapshot(snap)

	st, _ = commitReload(t, st, sdb)
	if got := st.GetState(addr, slot); got != (common.Hash{0x01}) {
		t.Fatalf("after revert+commit = %x, want original", got)
	}
}

func TestStorageCacheSelfDestruct(t *testing.T) {
	cache := evmcache.NewStorageCache(64)
	st, sdb := newTestState(t, cache)
	addr := common.Address{0xaa}
	slot := common.Hash{0x01}

	st.CreateAccount(addr)
	st.SetNonce(addr, 1)
	st.SetCode(addr, []byte{0x00})
	st.SetState(addr, slot, common.Hash{0x42})
	st, _ = commitReload(t, st, sdb)

	st.SelfDestruct(addr)
	st, _ = commitReload(t, st, sdb)

	st.CreateAccount(addr)
	st.SetNonce(addr, 1)
	if got := st.GetState(addr, slot); got != (common.Hash{}) {
		t.Fatalf("recreated account still has %x", got)
	}
}

// Writing a slot the value it already holds is discarded before it reaches
// the journal, the trie or the cache, so repeating a write cannot change the
// root and cannot make the cache disagree with the committed state.
func TestStorageCacheSameValueWriteIsNoop(t *testing.T) {
	addr := common.Address{0xaa}
	slot := common.Hash{0x01}
	val := common.Hash{0x42}

	// Seed the slot, commit, then rewrite it `extra` more times with the
	// same value and commit again.
	run := func(extra int) (common.Hash, *evmcache.StorageCache) {
		cache := evmcache.NewStorageCache(64)
		st, sdb := newTestState(t, cache)
		st.CreateAccount(addr)
		st.SetNonce(addr, 1)
		st.SetState(addr, slot, val)
		st, _ = commitReload(t, st, sdb)

		for i := 0; i < extra; i++ {
			st.SetState(addr, slot, val)
		}
		if obj := st.getStateObject(addr); obj != nil && len(obj.dirtyStorage) != 0 {
			t.Fatalf("%d same-value writes left %d dirty slots", extra, len(obj.dirtyStorage))
		}
		_, root := commitReload(t, st, sdb)
		return root, cache
	}

	baseRoot, _ := run(0)
	repeatRoot, cache := run(2)
	if baseRoot != repeatRoot {
		t.Fatalf("same-value writes changed the root: %s vs %s", baseRoot, repeatRoot)
	}
	if got, ok := cache.Get(repeatRoot, addr, slot); !ok || got != val {
		t.Fatalf("cache after no-op writes = %x %v, want %x true", got, ok, val)
	}
}

// The same holds for a round trip: writing a different value and then writing
// the original back drops the dirty marker instead of committing a change.
func TestStorageCacheWriteBackToOriginIsNoop(t *testing.T) {
	cache := evmcache.NewStorageCache(64)
	st, sdb := newTestState(t, cache)
	addr := common.Address{0xaa}
	slot := common.Hash{0x01}
	val := common.Hash{0x42}

	st.CreateAccount(addr)
	st.SetNonce(addr, 1)
	st.SetState(addr, slot, val)
	st, seeded := commitReload(t, st, sdb)

	st.SetState(addr, slot, common.Hash{0xff})
	st.SetState(addr, slot, val)
	if obj := st.getStateObject(addr); obj != nil && len(obj.dirtyStorage) != 0 {
		t.Fatalf("round trip left %d dirty slots", len(obj.dirtyStorage))
	}
	st, root := commitReload(t, st, sdb)
	if root != seeded {
		t.Fatalf("round trip changed the root: %s vs %s", seeded, root)
	}
	if got := st.GetState(addr, slot); got != val {
		t.Fatalf("slot after round trip = %x, want %x", got, val)
	}
}

func TestStorageCacheDisabledIsNoop(t *testing.T) {
	st, sdb := newTestState(t, nil)
	addr := common.Address{0xaa}
	st.CreateAccount(addr)
	st.SetNonce(addr, 1)
	st.SetState(addr, common.Hash{1}, common.Hash{2})
	st, _ = commitReload(t, st, sdb)
	if got := st.GetState(addr, common.Hash{1}); got != (common.Hash{2}) {
		t.Fatalf("disabled cache broke reads: %x", got)
	}
}

func TestStorageCacheDifferentialRoot(t *testing.T) {
	run := func(cache *evmcache.StorageCache) common.Hash {
		st, sdb := newTestState(t, cache)
		addr := common.Address{0xaa}
		st.CreateAccount(addr)
		st.SetNonce(addr, 1)
		st.AddBalance(addr, uint256.NewInt(100), 0)
		st.SetCode(addr, crypto.Keccak256([]byte("code")))
		for i := byte(1); i <= 8; i++ {
			st.SetState(addr, common.Hash{i}, common.Hash{i, i})
		}
		st, _ = commitReload(t, st, sdb)
		for i := byte(1); i <= 8; i++ {
			_ = st.GetState(addr, common.Hash{i})
			st.SetState(addr, common.Hash{i}, common.Hash{i + 1})
		}
		root, err := st.Commit(1, true)
		if err != nil {
			t.Fatal(err)
		}
		return root
	}
	off := run(nil)
	on := run(evmcache.NewStorageCache(64))
	if off != on {
		t.Fatalf("state root diverged: off=%s on=%s", off, on)
	}
}

func TestStorageCacheConcurrentGets(t *testing.T) {
	cache := evmcache.NewStorageCache(128)
	st, sdb := newTestState(t, cache)
	addr := common.Address{0xaa}
	st.CreateAccount(addr)
	st.SetNonce(addr, 1)
	for i := byte(0); i < 32; i++ {
		st.SetState(addr, common.Hash{i}, common.Hash{i})
	}
	st, _ = commitReload(t, st, sdb)

	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cpy := st.Copy()
			for i := byte(0); i < 32; i++ {
				_ = cpy.GetState(addr, common.Hash{i})
			}
		}()
	}
	wg.Wait()
}
