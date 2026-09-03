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

package vm

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/evmcache"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

func TestOpKeccak256CacheHit(t *testing.T) {
	cache := evmcache.NewKeccakCache(32)
	cache.SetStatsEnabled(true)
	env := NewEVM(BlockContext{}, TxContext{}, nil, params.TestChainConfig, Config{
		EnableKeccakCache: true,
		KeccakCache:       cache,
	})
	interp := NewEVMInterpreter(env)
	env.interpreter = interp

	mem := NewMemory()
	input := bytes.Repeat([]byte{0xab}, 64)
	mem.Resize(64)
	copy(mem.store, input)

	run := func() common.Hash {
		stack := newstack()
		stack.push(uint256.NewInt(64))
		stack.push(uint256.NewInt(0))
		pc := uint64(0)
		if _, err := opKeccak256(&pc, interp, &ScopeContext{Memory: mem, Stack: stack}); err != nil {
			t.Fatal(err)
		}
		got := stack.pop()
		return common.Hash(got.Bytes32())
	}

	first := run()
	st := cache.Stats()
	if st.Misses != 1 || st.Inserts != 1 {
		t.Fatalf("after first hash: %+v", st)
	}
	second := run()
	if first != second {
		t.Fatalf("hash diverged: %x vs %x", first, second)
	}
	st = cache.Stats()
	if st.Hits != 1 {
		t.Fatalf("expected a cache hit, stats=%+v", st)
	}
}

func TestOpKeccak256CacheDisabledMatches(t *testing.T) {
	mem := NewMemory()
	input := bytes.Repeat([]byte{0xcd}, 32)
	mem.Resize(32)
	copy(mem.store, input)

	hash := func(cfg Config) common.Hash {
		env := NewEVM(BlockContext{}, TxContext{}, nil, params.TestChainConfig, cfg)
		interp := NewEVMInterpreter(env)
		env.interpreter = interp
		stack := newstack()
		stack.push(uint256.NewInt(32))
		stack.push(uint256.NewInt(0))
		pc := uint64(0)
		if _, err := opKeccak256(&pc, interp, &ScopeContext{Memory: mem, Stack: stack}); err != nil {
			t.Fatal(err)
		}
		got := stack.pop()
		return common.Hash(got.Bytes32())
	}

	off := hash(Config{})
	on := hash(Config{EnableKeccakCache: true, KeccakCache: evmcache.NewKeccakCache(16)})
	if off != on {
		t.Fatalf("keccak cache changed the hash: off=%x on=%x", off, on)
	}
}

func TestOpKeccak256LongInputNotCached(t *testing.T) {
	cache := evmcache.NewKeccakCache(16)
	env := NewEVM(BlockContext{}, TxContext{}, nil, params.TestChainConfig, Config{KeccakCache: cache})
	interp := NewEVMInterpreter(env)
	env.interpreter = interp
	mem := NewMemory()
	mem.Resize(128)
	stack := newstack()
	stack.push(uint256.NewInt(128))
	stack.push(uint256.NewInt(0))
	pc := uint64(0)
	if _, err := opKeccak256(&pc, interp, &ScopeContext{Memory: mem, Stack: stack}); err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Inserts != 0 {
		t.Fatal("inputs > 64 bytes must not be inserted")
	}
}
