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

package core

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
)

// TestStorageCacheReorgPurges confirms the cache is dropped on reorg and that
// subsequent reads follow the new canonical state.
func TestStorageCacheReorgPurges(t *testing.T) {
	var (
		addr   = common.Address{0xaa}
		slot   = common.Hash{0x01}
		engine = ethash.NewFaker()
		gspec  = &Genesis{
			Config:  params.TestChainConfig,
			BaseFee: big.NewInt(params.InitialBaseFee),
			Alloc:   types.GenesisAlloc{addr: {Balance: big.NewInt(1)}},
		}
	)
	db, canonical, _ := GenerateChainWithGenesis(gspec, engine, 2, func(i int, b *BlockGen) {
		b.SetExtra([]byte{0x01})
	})
	fork, _ := GenerateChain(gspec.Config, gspec.ToBlock(), engine, db, 3, func(i int, b *BlockGen) {
		b.SetExtra([]byte{0x02})
	})

	cfg := DefaultCacheConfigWithScheme(rawdb.HashScheme)
	cfg.EnableStorageCache = true
	cfg.SnapshotLimit = 0
	chain, err := NewBlockChain(db, cfg, gspec, nil, engine, vm.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer chain.Stop()

	if _, err := chain.InsertChain(canonical); err != nil {
		t.Fatal(err)
	}
	cache := chain.statedb.StorageCache()
	if cache == nil {
		t.Fatal("storage cache was not attached")
	}
	cache.Put(addr, slot, common.Hash{0x11})
	cache.EndCommit(chain.CurrentBlock().Root)
	if _, ok := cache.Get(chain.CurrentBlock().Root, addr, slot); !ok {
		t.Fatal("pre-reorg cache insert missed")
	}
	cache.ResetStats()

	if _, err := chain.InsertChain(fork); err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Purges == 0 {
		t.Fatal("reorg did not purge the storage cache")
	}
	if _, ok := cache.Get(chain.CurrentBlock().Root, addr, slot); ok {
		t.Fatal("stale storage cache entry survived the reorg")
	}
}
