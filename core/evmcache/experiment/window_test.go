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

package experiment

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ethereum/go-ethereum/core/evmcache"
	"github.com/ethereum/go-ethereum/tests"
)

// TestWindowCacheImpact replays a caller-chosen window of consecutive blocks
// under each cache configuration. It is opt-in: EVMCACHE_WINDOW=0 (the
// default) skips, so a normal package test does not start this run.
//
//	EVMCACHE_BLOCK_DATA=txTest_all \
//	EVMCACHE_START=23500000 \
//	EVMCACHE_WINDOW=16 \
//	EVMCACHE_REPEATS=5 \
//	go test ./core/evmcache/experiment -run '^TestWindowCacheImpact$' -count=1 -timeout 90m -v
//
// Each transaction is applied in order through the interpreter, which steps
// every opcode. The timed region is that loop. The storage cache is purged
// after the genesis commit, so only slots this run actually committed are
// cacheable. Each block's gain is the mean execution time over the passes,
// compared with both off. Positive means faster.
func TestWindowCacheImpact(t *testing.T) {
	start := envUint(t, "EVMCACHE_START", 23500000)
	window := envUint(t, "EVMCACHE_WINDOW", 0)
	repeats := envUint(t, "EVMCACHE_REPEATS", 5)
	if window == 0 {
		t.Skip("EVMCACHE_WINDOW is unset or 0; set it to the number of blocks to replay")
	}
	if repeats == 0 || repeats > 100000 {
		t.Fatal("EVMCACHE_REPEATS must be in 1..100000")
	}

	dataDir := resolveWindowDataDir()
	names := make([]string, window)
	for i := uint64(0); i < window; i++ {
		names[i] = strconv.FormatUint(start+i, 10)
	}
	blocks := loadNamedBlocksAt(t, dataDir, names, true)
	t.Logf("loaded %d blocks from %s", len(blocks), dataDir)

	chain := tests.Forks["Prague"]
	if chain == nil {
		t.Fatal("missing Prague fork config")
	}

	const (
		offIdx     = 0
		storageIdx = 1
		keccakIdx  = 2
		bothIdx    = 3
	)
	configs := []cacheConfig{
		{name: "both off"},
		{name: "storage only", storage: true, storageSize: evmcache.DefaultStorageSlots},
		{name: "keccak only", keccak: true},
		{name: "both on", storage: true, keccak: true, storageSize: evmcache.DefaultStorageSlots},
	}

	cold := measureConfigs(t, blocks, configs, false, int(repeats), chain)
	requireSameWork(t, configs, cold)

	report := windowReport{
		Start:   start,
		Window:  window,
		Repeats: repeats,
		Blocks:  make([]windowBlockGain, len(blocks)),
	}
	var offSum, storageSum, keccakSum, bothSum float64
	for i, blk := range blocks {
		off := meanBlockNs(cold[offIdx], i)
		storage := meanBlockNs(cold[storageIdx], i)
		keccak := meanBlockNs(cold[keccakIdx], i)
		both := meanBlockNs(cold[bothIdx], i)
		report.Blocks[i] = windowBlockGain{
			Block:       blk.number,
			Txs:         len(blk.txs),
			StorageOnly: round2(gainPct(off, storage)),
			KeccakOnly:  round2(gainPct(off, keccak)),
			BothOn:      round2(gainPct(off, both)),
		}
		offSum += off
		storageSum += storage
		keccakSum += keccak
		bothSum += both
	}

	root := repoRoot()
	if root == "" {
		t.Fatal("repo root not found")
	}
	outPath := filepath.Join(root, "EVM_CACHE_WINDOW_RESULTS.json")
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	fmt.Printf("mean of %d passes, positive = faster than both off\n", repeats)
	fmt.Printf("storage only  %+.2f%%\n", gainPct(offSum, storageSum))
	fmt.Printf("keccak only   %+.2f%%\n", gainPct(offSum, keccakSum))
	fmt.Printf("both on       %+.2f%%\n", gainPct(offSum, bothSum))
	fmt.Printf("wrote %s\n", filepath.Base(outPath))
}

// windowReport is one row per block. Gains are percents: 0.67 means 0.67%
// faster than both off.
type windowReport struct {
	Start   uint64            `json:"start"`
	Window  uint64            `json:"window"`
	Repeats uint64            `json:"repeats"`
	Blocks  []windowBlockGain `json:"blocks"`
}

type windowBlockGain struct {
	Block       uint64  `json:"block"`
	Txs         int     `json:"txs"`
	StorageOnly float64 `json:"storageOnly"`
	KeccakOnly  float64 `json:"keccakOnly"`
	BothOn      float64 `json:"bothOn"`
}

// meanBlockNs is the arithmetic mean of one block's execution time across passes.
func meanBlockNs(m measurement, block int) float64 {
	if len(m.perBlock) == 0 {
		return 0
	}
	var sum float64
	for _, pass := range m.perBlock {
		sum += pass[block]
	}
	return sum / float64(len(m.perBlock))
}

// gainPct is (off-arm)/off as a percent. Positive means the arm was faster.
func gainPct(off, arm float64) float64 {
	if off == 0 {
		return 0
	}
	return (off - arm) / off * 100
}

func round2(x float64) float64 {
	return math.Round(x*100) / 100
}

func envUint(t *testing.T, key string, def uint64) uint64 {
	t.Helper()
	s := os.Getenv(key)
	if s == "" {
		return def
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return v
}

// resolveWindowDataDir finds the fixture root. EVMCACHE_BLOCK_DATA may be
// absolute, relative to the process working directory, or relative to the
// module root (go test runs the binary in the package directory, so a bare
// "txTest_all" would otherwise miss the repo-root symlink).
func resolveWindowDataDir() string {
	dir := os.Getenv("EVMCACHE_BLOCK_DATA")
	if dir == "" {
		dir = "txTest_all"
	}
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	if root := repoRoot(); root != "" {
		alt := filepath.Join(root, dir)
		if info, err := os.Stat(alt); err == nil && info.IsDir() {
			return alt
		}
	}
	return dir
}

func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
