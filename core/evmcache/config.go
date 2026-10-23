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

// Package evmcache implements optional, process-lifetime caches for EVM
// execution. Both caches are disabled unless an instance is attached; that
// keeps the default path bit-identical to stock geth.
package evmcache

const (
	// DefaultStorageSlots is accepted by NewStorageCache for call-site
	// compatibility. Design D retains maps without a hard entry budget.
	DefaultStorageSlots = 65536

	// DefaultKeccakSlots is a small table for SHA3 inputs of at most 64
	// bytes (~1.5 MB).
	DefaultKeccakSlots = 16384

	// MaxKeccakInput is the largest SHA3 payload admitted to the keccak
	// cache. Solidity mapping keys are keccak256(key ‖ slot) = 64 bytes.
	MaxKeccakInput = 64
)

// Config controls construction of the optional EVM caches. Zero values mean
// "disabled". Sizes of 0 with the corresponding Enable flag set use the
// package defaults.
type Config struct {
	EnableStorageCache bool
	StorageCacheSlots  int

	EnableKeccakCache bool
	KeccakCacheSlots  int
}

// StorageSlots returns the table size to allocate, or 0 if the storage cache
// is disabled.
func (c Config) StorageSlots() int {
	if !c.EnableStorageCache {
		return 0
	}
	if c.StorageCacheSlots <= 0 {
		return DefaultStorageSlots
	}
	return c.StorageCacheSlots
}

// KeccakSlots returns the table size to allocate, or 0 if the keccak cache
// is disabled.
func (c Config) KeccakSlots() int {
	if !c.EnableKeccakCache {
		return 0
	}
	if c.KeccakCacheSlots <= 0 {
		return DefaultKeccakSlots
	}
	return c.KeccakCacheSlots
}

func nextPow2(n int) int {
	if n <= 1 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n |= n >> 32
	return n + 1
}
