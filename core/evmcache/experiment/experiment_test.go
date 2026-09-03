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
	"bytes"
	"fmt"
	"math/big"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/evmcache"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	vmruntime "github.com/ethereum/go-ethereum/core/vm/runtime"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

var (
	tokenAddr = common.BytesToAddress([]byte("token"))
	origin    = common.BytesToAddress([]byte("origin"))
)

// benchRepeats is how many interleaved on/off pairs every EVM-level cell runs.
// The noise-floor row reports what this buys; below about 20 the same-config
// spread is wide enough to swallow the effects being measured.
const benchRepeats = 20

func TestTokenRuntimeTransfer(t *testing.T) {
	st, _, cfg := newHarness(t, nil, nil)
	alice := common.Address{0xa1}
	bob := common.Address{0xb0}
	seedBalance(st, alice, 1_000_000)
	seedBalance(st, bob, 0)
	st.Finalise(true)

	cfg.Origin = alice
	cfg.State = st
	ret, gas, err := vmruntime.Call(tokenAddr, TransferCalldata(bob, 7), cfg)
	if err != nil {
		t.Fatalf("transfer failed: %v", err)
	}
	if gas == cfg.GasLimit {
		t.Fatal("no gas consumed")
	}
	if len(ret) < 32 || ret[31] != 1 {
		t.Fatalf("return = %x", ret)
	}
	if got := st.GetState(tokenAddr, MappingSlot(alice, 0)); got != common.BigToHash(big.NewInt(1_000_000-7)) {
		t.Fatalf("alice balance = %x", got)
	}
	if got := st.GetState(tokenAddr, MappingSlot(bob, 0)); got != common.BigToHash(big.NewInt(7)) {
		t.Fatalf("bob balance = %x", got)
	}
}

func TestEVMCacheExperiment(t *testing.T) {
	// Timing + differential checks. The readable write-up is
	// EVM_CACHE_EXPERIMENT_RESULTS.md (maintained by hand; not overwritten).
	var b strings.Builder
	t.Cleanup(func() { t.Logf("collected report\n\n%s", b.String()) })
	t.Run("S-A/cost-per-layer", func(t *testing.T) { runSA(t, &b) })
	t.Run("S-B/erc20-distribution", func(t *testing.T) { runSB(t, &b) })
	t.Run("S-D/hit-rate-sweep", func(t *testing.T) { runSD(t, &b) })
	t.Run("S-E/sstore-same-value", func(t *testing.T) { runSE(t, &b) })
	t.Run("K-A/keccak-micro", func(t *testing.T) { runKA(t, &b) })
	t.Run("K-B/keccak-census", func(t *testing.T) { runKB(t, &b) })
	t.Run("K-D/keccak-sweep", func(t *testing.T) { runKD(t, &b) })
}

func newHarness(t testing.TB, sc *evmcache.StorageCache, kc *evmcache.KeccakCache) (*state.StateDB, *state.CachingDB, *vmruntime.Config) {
	t.Helper()
	sdb := state.NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)
	sdb.SetStorageCache(sc)
	st, err := state.New(types.EmptyRootHash, sdb)
	if err != nil {
		t.Fatal(err)
	}
	st.AddBalance(origin, uint256.NewInt(params.Ether), 0)
	st.SetCode(tokenAddr, TokenRuntime())
	cfg := &vmruntime.Config{
		ChainConfig: params.MergedTestChainConfig,
		Origin:      origin,
		GasLimit:    10_000_000,
		State:       st,
		EVMConfig: vm.Config{
			EnableKeccakCache: kc != nil,
			KeccakCache:       kc,
		},
	}
	return st, sdb, cfg
}

func seedBalance(st *state.StateDB, addr common.Address, amount uint64) {
	st.CreateAccount(addr)
	st.SetState(tokenAddr, MappingSlot(addr, 0), common.BigToHash(new(big.Int).SetUint64(amount)))
}

func commitReload(t testing.TB, st *state.StateDB, sdb *state.CachingDB) *state.StateDB {
	t.Helper()
	st, _ = mustCommit(t, st, sdb)
	return st
}

func mustCommit(t testing.TB, st *state.StateDB, sdb *state.CachingDB) (*state.StateDB, common.Hash) {
	t.Helper()
	root, err := st.Commit(0, true)
	if err != nil {
		t.Fatal(err)
	}
	st, err = state.New(root, sdb)
	if err != nil {
		t.Fatal(err)
	}
	return st, root
}

// Hit/miss accounting is off by default so the lookup path stays free of
// atomic increments. Every cache whose hit rate this file reports has to opt
// in; the S-A table measures both settings to show what it costs.
func newStorageCache(slots int) *evmcache.StorageCache {
	c := evmcache.NewStorageCache(slots)
	c.SetStatsEnabled(true)
	return c
}

func newKeccakCache(slots int) *evmcache.KeccakCache {
	c := evmcache.NewKeccakCache(slots)
	c.SetStatsEnabled(true)
	return c
}

func runSA(t *testing.T, b *strings.Builder) {
	fmt.Fprintf(b, "## S-A — Cost per layer\n\n")

	const wide = 65536 // evmcache.DefaultStorageSlots

	addr := common.Address{0xaa}
	slot := common.Hash{0x01}
	val := common.Hash{0x42}
	root := common.Hash{1}

	keys := make([]common.Hash, wide)
	for i := range keys {
		keys[i] = intHash(i)
	}

	// A one-entry map lives in L1 and flatters itself against any table
	// holding a realistic working set, so both cardinalities are measured.
	hotMap := map[common.Hash]common.Hash{slot: val}
	wideMap := make(map[common.Hash]common.Hash, wide)
	for _, k := range keys {
		wideMap[k] = val
	}

	hotCache := evmcache.NewStorageCache(1024)
	hotCache.Put(addr, slot, val)
	hotCache.EndCommit(root)

	wideCache := evmcache.NewStorageCache(wide)
	for _, k := range keys {
		wideCache.Put(addr, k, val)
	}
	wideCache.EndCommit(root)

	countedCache := evmcache.NewStorageCache(wide)
	for _, k := range keys {
		countedCache.Put(addr, k, val)
	}
	countedCache.EndCommit(root)
	countedCache.SetStatsEnabled(true)

	type cell struct {
		name string
		fn   func(*testing.B)
	}
	cells := []cell{
		{"originStorage map hit (1 key, in L1)", func(b *testing.B) {
			var sink common.Hash
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sink = hotMap[slot]
			}
			_ = sink
		}},
		{"originStorage map hit (65536 keys)", func(b *testing.B) {
			var sink common.Hash
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sink = wideMap[keys[i%wide]]
			}
			_ = sink
		}},
		{"storage cache hit (1 key, in L1)", func(b *testing.B) {
			var sink common.Hash
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sink, _ = hotCache.Get(root, addr, slot)
			}
			_ = sink
		}},
		{"storage cache hit (65536 keys)", func(b *testing.B) {
			var sink common.Hash
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sink, _ = wideCache.Get(root, addr, keys[i%wide])
			}
			_ = sink
		}},
		{"storage cache hit (65536 keys, stats on)", func(b *testing.B) {
			var sink common.Hash
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sink, _ = countedCache.Get(root, addr, keys[i%wide])
			}
			_ = sink
		}},
		{"snapshot fastcache hit (1 key, in L1)", func(b *testing.B) {
			fc := fastcache.New(1 << 20)
			acc, stor := crypto.Keccak256Hash(addr[:]), crypto.Keccak256Hash(slot[:])
			blob, _ := rlp.EncodeToBytes(common.TrimLeftZeroes(val[:]))
			fc.Set(append(acc[:], stor[:]...), blob)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				k := append(acc[:], stor[:]...)
				got, _ := fc.HasGet(nil, k)
				_, content, _, _ := rlp.Split(got)
				var out common.Hash
				out.SetBytes(content)
				_ = out
			}
		}},
		{"snapshot fastcache hit (65536 keys)", func(b *testing.B) {
			fc := fastcache.New(16 << 20)
			acc := crypto.Keccak256Hash(addr[:])
			blob, _ := rlp.EncodeToBytes(common.TrimLeftZeroes(val[:]))
			// The storage-key hash is what geth composes the lookup key from;
			// precomputed here so the cell times the cache, not keccak.
			stors := make([]common.Hash, wide)
			for i, k := range keys {
				stors[i] = crypto.Keccak256Hash(k[:])
				fc.Set(append(acc[:], stors[i][:]...), blob)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stor := stors[i%wide]
				k := append(acc[:], stor[:]...)
				got, _ := fc.HasGet(nil, k)
				_, content, _, _ := rlp.Split(got)
				var out common.Hash
				out.SetBytes(content)
				_ = out
			}
		}},
	}

	fmt.Fprintf(b, "| Level | ns/op | allocs/op | bytes/op |\n|---|---:|---:|---:|\n")
	var mapNs, cacheNs, snapNs float64
	for _, c := range cells {
		res := testing.Benchmark(c.fn)
		ns := float64(res.NsPerOp())
		fmt.Fprintf(b, "| %s | %.1f | %d | %d |\n", c.name, ns, res.AllocsPerOp(), res.AllocedBytesPerOp())
		switch c.name {
		case "originStorage map hit (65536 keys)":
			mapNs = ns
		case "storage cache hit (65536 keys)":
			cacheNs = ns
		case "snapshot fastcache hit (65536 keys)":
			snapNs = ns
		}
	}

	fmt.Fprintf(b, "\nAt equal cardinality the cache is %.1f ns above a plain map (%.1f vs %.1f); "+
		"both hash a comparable key with the runtime hasher, and the cache adds an atomic pointer load "+
		"plus a root comparison.\n", cacheNs-mapNs, cacheNs, mapNs)
	fmt.Fprintf(b, "\nH1 prize (snapshot hit − map hit, equal cardinality) = %.1f ns.\n", snapNs-mapNs)
	if snapNs-mapNs < 50 {
		fmt.Fprintf(b, "Gate: snapshot hit is within a few tens of ns of a map hit — H1 is weak; H2 (cross-block) is the remaining claim.\n")
	} else {
		fmt.Fprintf(b, "Gate: snapshot hit is substantially more expensive than a map hit — H1 is live.\n")
	}

	// Copy-on-write buys the lock-free lookup by rebuilding maps on the commit
	// path. That cost scales with how much the cache holds, not with how much
	// the block wrote, so it is priced separately and across sizes.
	fmt.Fprintf(b, "\n### Commit cost (staging + fold + publish), mean over 64 commits\n\n")
	fmt.Fprintf(b, "| Budget (entries) | Slots staged per block | ms/commit |\n|---:|---:|---:|\n")
	for _, budget := range []int{1024, 4096, 16384, 65536} {
		for _, staged := range []int{256, 2048} {
			const commits = 64
			c := evmcache.NewStorageCache(budget)
			for i := 0; i < budget && i < wide; i++ {
				c.Put(addr, keys[i], val)
			}
			c.EndCommit(root)

			start := time.Now()
			for n := 0; n < commits; n++ {
				for j := 0; j < staged; j++ {
					c.Put(addr, keys[(n*staged+j)%wide], common.Hash{byte(n)})
				}
				c.EndCommit(root)
			}
			ms := float64(time.Since(start).Microseconds()) / 1000 / commits
			fmt.Fprintf(b, "| %d | %d | %.2f |\n", budget, staged, ms)
		}
	}
	fmt.Fprintf(b, "\nThe base is only rebuilt when the delta outgrows an eighth of the budget, "+
		"after %d idle commits, or on a self-destruct, so these are amortized figures.\n", 32)
	fmt.Fprintln(b)
}

func runSB(t *testing.T, b *strings.Builder) {
	fmt.Fprintf(b, "## S-B — ERC-20 hit distribution\n\n")
	sc := newStorageCache(4096)
	st, sdb, cfg := newHarness(t, sc, nil)
	users := makeAddrs(64)
	for _, u := range users {
		seedBalance(st, u, 1_000_000)
	}
	st = commitReload(t, st, sdb)
	cfg.State = st
	st.EnablePathStats()

	const transfers = 256
	start := time.Now()
	gas := runTransfers(t, cfg, st, users, transfers)
	elapsed := time.Since(start)
	st = commitReload(t, st, sdb)
	cfg.State = st

	// Second block: originStorage starts empty; this is the H2 gap.
	st.EnablePathStats().Reset()
	sc.ResetStats()
	start = time.Now()
	gas2 := runTransfers(t, cfg, st, users, transfers)
	elapsed2 := time.Since(start)
	d, p, o, c, backend := st.PathStats().Snapshot()
	cst := sc.Stats()

	fmt.Fprintf(b, "Block 1: %d transfers in %s, gas=%d (warms originStorage + snapshot/trie).\n", transfers, elapsed, gas)
	fmt.Fprintf(b, "Block 2: %d transfers in %s, gas=%d.\n\n", transfers, elapsed2, gas2)
	fmt.Fprintf(b, "| Path | Count |\n|---|---:|\n")
	fmt.Fprintf(b, "| dirty | %d |\n| pending | %d |\n| origin | %d |\n| evm storage cache | %d |\n| backend (snapshot/trie) | %d |\n", d, p, o, c, backend)
	fmt.Fprintf(b, "\nCross-block cache: hits=%d misses=%d hit-rate=%.1f%%\n", cst.Hits, cst.Misses, 100*cst.HitRate())
	fmt.Fprintf(b, "\nGas is identical across cache on/off by construction of EIP-2929 (access list resets every tx); this table is performance-only.\n\n")
}

func runSD(t *testing.T, b *strings.Builder) {
	fmt.Fprintf(b, "## S-D — Hit-rate sweep\n\n")
	fmt.Fprintf(b, "### D1 — Synthetic (cache Get/Put, no EVM)\n\n")
	fmt.Fprintf(b, "Table budget is 128 entries, so pool=256 cannot fit and forces misses.\n\n")
	fmt.Fprintf(b, "| Key pool | Measured hit rate | ns/op | allocs/op |\n|---:|---:|---:|---:|\n")

	const cacheSlots = 128
	for _, pool := range []int{8, 64, 128, 256} {
		c := newStorageCache(cacheSlots)
		root := common.Hash{1}
		addr := common.Address{0x01}
		for i := 0; i < pool; i++ {
			c.Put(addr, intHash(i), intHash(i+1))
		}
		c.EndCommit(root)
		c.ResetStats()
		res := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c.Get(root, addr, intHash(i%pool))
			}
		})
		st := c.Stats()
		fmt.Fprintf(b, "| %d | %.1f%% | %d | %d |\n", pool, 100*st.HitRate(), res.NsPerOp(), res.AllocsPerOp())
	}

	fmt.Fprintf(b, "\n### D2 — ERC-20 address-set sweep\n\n")
	fmt.Fprintf(b, "| Addresses | Cache | ns/transfer | gas | hit rate | root match |\n|---:|---|---:|---:|---:|---|\n")

	noise := erc20Cell(t, 64, 256, false, false)
	fmt.Fprintf(b, "| noise floor (off vs off, N=64) | off | %.0f vs %.0f (%.2f%%) | %d | n/a | %v |\n",
		noise.onNs, noise.offNs, noise.speedup(), noise.gas, noise.rootMatch)

	for _, naddr := range []int{8, 64, 128, 256} {
		cell := erc20Cell(t, naddr, 256, true, false)
		fmt.Fprintf(b, "| %d | on vs off | %.0f vs %.0f (%.2f%%) | %d | %.1f%% | %v |\n",
			naddr, cell.onNs, cell.offNs, cell.speedup(), cell.gas, 100*cell.hitRate, cell.rootMatch)
		if !cell.rootMatch || cell.gasOff != cell.gas {
			t.Fatalf("differential failed for N=%d", naddr)
		}
	}

	// Worst case: every transfer touches a fresh pair of accounts and the
	// budget is 8 entries, so the cache is pure overhead. It must not regress.
	worst := erc20Cell(t, 256, 256, true, true)
	fmt.Fprintf(b, "| 256 unique, 8-entry budget (worst) | on vs off | %.0f vs %.0f (%.2f%%) | %d | %.1f%% | %v |\n",
		worst.onNs, worst.offNs, worst.speedup(), worst.gas, 100*worst.hitRate, worst.rootMatch)
	if !worst.rootMatch {
		t.Fatal("worst-case state root mismatch")
	}

	runCrossBlock(t, b)
}

// runCrossBlock is the case originStorage structurally cannot serve: the cache
// has to survive five consecutive commits, so block 1 pays the cold cost and
// blocks 2-5 read what earlier blocks committed.
func runCrossBlock(t *testing.T, b *strings.Builder) {
	const (
		naddr    = 64
		nblocks  = 5
		perBlock = 128
	)
	res := erc20CrossBlock(t, naddr, nblocks, perBlock)

	fmt.Fprintf(b, "\n### D3 — Cross-block reuse, %d blocks × %d transfers over %d addresses\n\n", nblocks, perBlock, naddr)
	fmt.Fprintf(b, "| Block | ns/transfer on | ns/transfer off | speedup | hit rate | cache entries | commit ms (on) |\n")
	fmt.Fprintf(b, "|---:|---:|---:|---:|---:|---:|---:|\n")
	for i := 0; i < nblocks; i++ {
		speedup := 0.0
		if res.offNs[i] != 0 {
			speedup = 100 * (res.offNs[i] - res.onNs[i]) / res.offNs[i]
		}
		fmt.Fprintf(b, "| %d | %.0f | %.0f | %.2f%% | %.1f%% | %d | %.2f |\n",
			i+1, res.onNs[i], res.offNs[i], speedup, 100*res.hitRate[i], res.entries[i], res.commitMs[i])
	}
	fmt.Fprintf(b, "\nWhole run: %.0f vs %.0f ns/transfer (%.2f%%), gas %d on / %d off, root match=%v.\n",
		res.totalOnNs, res.totalOffNs, res.speedup(), res.gas, res.gasOff, res.rootMatch)
	fmt.Fprintf(b, "Cache purges during the run: %d (must be 0 — the cache has to live across all %d blocks).\n\n",
		res.purges, nblocks)

	if !res.rootMatch {
		t.Fatal("cross-block state root or gas mismatch")
	}
	if res.purges != 0 {
		t.Fatalf("cache was purged %d times mid-run", res.purges)
	}
}

// runSE prices a repeated SSTORE of the value a slot already holds. The state
// layer discards it before the journal, the trie and the cache, but EIP-2929
// still charges for it, so "dismissed" is true of the state and false of gas.
func runSE(t *testing.T, b *strings.Builder) {
	fmt.Fprintf(b, "## S-E — Writing the same value twice\n\n")

	const (
		slot  = 0x01
		value = 0x2a
	)
	target := common.BytesToAddress([]byte("sstore"))
	slotKey := common.Hash{31: slot}

	// PUSH1 value, PUSH1 slot, SSTORE, repeated, then STOP.
	code := func(writes int) []byte {
		var out []byte
		for i := 0; i < writes; i++ {
			out = append(out, byte(vm.PUSH1), value, byte(vm.PUSH1), slot, byte(vm.SSTORE))
		}
		return append(out, byte(vm.STOP))
	}

	type result struct {
		gas     uint64
		stored  common.Hash
		entries int
		inserts uint64
	}
	measure := func(writes int) result {
		sc := newStorageCache(64)
		sdb := state.NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)
		sdb.SetStorageCache(sc)
		st, err := state.New(types.EmptyRootHash, sdb)
		if err != nil {
			t.Fatal(err)
		}
		st.CreateAccount(target)
		st.SetNonce(target, 1)
		st.SetCode(target, code(writes))
		st.AddBalance(origin, uint256.NewInt(params.Ether), 0)

		cfg := &vmruntime.Config{
			ChainConfig: params.MergedTestChainConfig,
			Origin:      origin,
			GasLimit:    1_000_000,
			State:       st,
		}
		_, leftover, err := vmruntime.Call(target, nil, cfg)
		if err != nil {
			t.Fatal(err)
		}
		st, _ = mustCommit(t, st, sdb)
		return result{
			gas:     cfg.GasLimit - leftover,
			stored:  st.GetState(target, slotKey),
			entries: sc.Len(),
			inserts: sc.Stats().Inserts,
		}
	}

	one, two, three := measure(1), measure(2), measure(3)

	fmt.Fprintf(b, "| Identical SSTOREs | gas | committed slot | cache entries |\n|---:|---:|---|---:|\n")
	for _, r := range []struct {
		n int
		r result
	}{{1, one}, {2, two}, {3, three}} {
		fmt.Fprintf(b, "| %d | %d | %x | %d |\n", r.n, r.r.gas, r.r.stored, r.r.entries)
	}

	// Each extra write costs a warm storage read plus the two PUSH1s that
	// set it up. It does not pay SSTORE_SET again.
	perExtra := uint64(params.WarmStorageReadCostEIP2929 + 2*vm.GasFastestStep)
	fmt.Fprintf(b, "\nEach repeat costs %d gas (%d warm storage read + %d for two PUSH1), "+
		"not the %d of the initial SSTORE_SET.\n",
		perExtra, params.WarmStorageReadCostEIP2929, 2*vm.GasFastestStep, params.SstoreSetGasEIP2200)
	fmt.Fprintf(b, "The committed slot and the cache contents are unchanged, and the cache holds one "+
		"entry however many times the slot is rewritten (%d inserts staged for 1 entry at 3 writes).\n\n",
		three.inserts)

	if got := two.gas - one.gas; got != perExtra {
		t.Fatalf("second identical SSTORE cost %d gas, want %d", got, perExtra)
	}
	if got := three.gas - two.gas; got != perExtra {
		t.Fatalf("third identical SSTORE cost %d gas, want %d", got, perExtra)
	}
	if one.stored != two.stored || two.stored != three.stored {
		t.Fatalf("committed slot diverged: %x %x %x", one.stored, two.stored, three.stored)
	}
	if one.entries != 1 || two.entries != 1 || three.entries != 1 {
		t.Fatalf("cache entries = %d/%d/%d, want 1 each", one.entries, two.entries, three.entries)
	}
}

func runKA(t *testing.T, b *strings.Builder) {
	fmt.Fprintf(b, "## K-A — Keccak microbenchmark\n\n")
	fmt.Fprintf(b, "| Path | size | ns/op | allocs/op |\n|---|---:|---:|---:|\n")
	hasher := crypto.NewKeccakState()
	for _, size := range []int{32, 64, 128, 256} {
		in := bytes.Repeat([]byte{0xab}, size)
		uncached := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			var h common.Hash
			for i := 0; i < b.N; i++ {
				hasher.Reset()
				hasher.Write(in)
				hasher.Read(h[:])
			}
		})
		fmt.Fprintf(b, "| uncached hash | %d | %d | %d |\n", size, uncached.NsPerOp(), uncached.AllocsPerOp())

		if size <= evmcache.MaxKeccakInput {
			c := evmcache.NewKeccakCache(1024)
			h := crypto.Keccak256Hash(in)
			c.Put(in, h)
			hit := testing.Benchmark(func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					c.Get(in)
				}
			})
			fmt.Fprintf(b, "| cache hit | %d | %d | %d |\n", size, hit.NsPerOp(), hit.AllocsPerOp())
			c2 := evmcache.NewKeccakCache(1024)
			miss := testing.Benchmark(func(b *testing.B) {
				b.ReportAllocs()
				buf := make([]byte, size)
				var h common.Hash
				for i := 0; i < b.N; i++ {
					buf[0] = byte(i)
					if _, ok := c2.Get(buf); !ok {
						hasher.Reset()
						hasher.Write(buf)
						hasher.Read(h[:])
						c2.Put(buf, h)
					}
				}
			})
			fmt.Fprintf(b, "| miss+insert | %d | %d | %d |\n", size, miss.NsPerOp(), miss.AllocsPerOp())
		}
	}
	fmt.Fprintln(b)
}

func runKB(t *testing.T, b *strings.Builder) {
	fmt.Fprintf(b, "## K-B — SHA3 census on ERC-20 transfer\n\n")
	kc := newKeccakCache(4096)
	st, sdb, cfg := newHarness(t, nil, kc)
	users := makeAddrs(64)
	for _, u := range users {
		seedBalance(st, u, 1_000_000)
	}
	st = commitReload(t, st, sdb)
	cfg.State = st
	kc.ResetStats()
	gas := runTransfers(t, cfg, st, users, 128)
	kst := kc.Stats()
	fmt.Fprintf(b, "128 transfers over 64 addresses, gas=%d, SHA3 cache lookups: hits=%d misses=%d inserts=%d hit-rate=%.1f%%\n",
		gas, kst.Hits, kst.Misses, kst.Inserts, 100*kst.HitRate())
	fmt.Fprintf(b, "Each transfer does two mapping-key hashes (sender + recipient), both 64-byte inputs.\n\n")
}

func runKD(t *testing.T, b *strings.Builder) {
	fmt.Fprintf(b, "## K-D — Keccak hit-rate sweep\n\n")
	fmt.Fprintf(b, "| Cell | Cache | ns/transfer | gas | hit rate | root match |\n|---|---|---:|---:|---:|---|\n")

	// Same workload with no cache on either arm, so the rows below can be
	// judged against the spread of the measurement itself.
	noise := erc20Cell(t, 64, 256, false, false)
	fmt.Fprintf(b, "| noise floor (no cache either arm, N=64) | off | %.0f vs %.0f (%.2f%%) | %d | n/a | %v |\n",
		noise.onNs, noise.offNs, noise.speedup(), noise.gas, noise.rootMatch)

	for _, naddr := range []int{8, 64, 128, 256} {
		cell := erc20KeccakCell(t, naddr, 256)
		fmt.Fprintf(b, "| ERC-20 N=%d | keccak on vs off | %.0f vs %.0f (%.2f%%) | %d | %.1f%% | %v |\n",
			naddr, cell.onNs, cell.offNs, cell.speedup(), cell.gas, 100*cell.hitRate, cell.rootMatch)
		if !cell.rootMatch || cell.gasOff != cell.gas {
			t.Fatalf("keccak differential failed for N=%d", naddr)
		}
	}

	// Cross-contract: ten tokens sharing one address set and one storage
	// layout, so the same 64-byte mapping keys recur across contracts.
	cross := erc20KeccakCrossContract(t, 64, 10, 128)
	fmt.Fprintf(b, "| 10 tokens × 64 addrs | keccak on vs off | %.0f vs %.0f (%.2f%%) | %d | %.1f%% | %v |\n\n",
		cross.onNs, cross.offNs, cross.speedup(), cross.gas, 100*cross.hitRate, cross.rootMatch)
	if !cross.rootMatch {
		t.Fatal("keccak cross-contract root or gas mismatch")
	}
}

type benchCell struct {
	onNs, offNs float64
	gas, gasOff uint64
	hitRate     float64
	rootMatch   bool
}

func (c benchCell) speedup() float64 {
	if c.offNs == 0 {
		return 0
	}
	return 100 * (c.offNs - c.onNs) / c.offNs
}

// interleave measures both configurations inside each repeat and alternates
// which one goes first.
//
// Measuring one arm to completion and then the other made two *identical*
// configurations differ by 16% on this machine. Interleaving alone still left
// 13%, because whichever arm ran first in a pair absorbed the GC cycle that
// the run before it had set up. Collecting before each run and swapping the
// order on odd repeats removes both biases.
func interleave(t *testing.T, repeats int, runOnce func(on bool) (float64, uint64, common.Hash, float64)) benchCell {
	measure := func(on bool) (float64, uint64, common.Hash, float64) {
		goruntime.GC()
		return runOnce(on)
	}

	// Discarded: warms the code paths, the allocator and the trie caches.
	runOnce(false)
	runOnce(true)

	var (
		cell            benchCell
		offRoot, onRoot common.Hash
	)
	for i := 0; i < repeats; i++ {
		var (
			offNs, onNs   float64
			offGas, onGas uint64
			offR, onR     common.Hash
			hit           float64
		)
		if i%2 == 0 {
			offNs, offGas, offR, _ = measure(false)
			onNs, onGas, onR, hit = measure(true)
		} else {
			onNs, onGas, onR, hit = measure(true)
			offNs, offGas, offR, _ = measure(false)
		}
		cell.offNs += offNs
		cell.onNs += onNs
		cell.gasOff, cell.gas, cell.hitRate = offGas, onGas, hit
		if i == 0 {
			offRoot, onRoot = offR, onR
			continue
		}
		if offR != offRoot || onR != onRoot {
			t.Fatalf("state root jittered across repeats: off %s vs %s, on %s vs %s",
				offR, offRoot, onR, onRoot)
		}
	}
	cell.offNs /= float64(repeats)
	cell.onNs /= float64(repeats)
	cell.rootMatch = offRoot == onRoot && cell.gasOff == cell.gas
	return cell
}

func erc20Cell(t *testing.T, naddr, transfers int, storageOn, unique bool) benchCell {
	runOnce := func(enabled bool) (ns float64, gas uint64, root common.Hash, hit float64) {
		var sc *evmcache.StorageCache
		if enabled {
			slots := 4096
			if unique {
				slots = 8 // force misses: working set ≫ budget
			}
			sc = newStorageCache(slots)
		}
		st, sdb, cfg := newHarness(t, sc, nil)
		users := makeAddrs(naddr)
		for _, u := range users {
			seedBalance(st, u, 1_000_000)
		}
		st = commitReload(t, st, sdb)
		cfg.State = st
		if sc != nil {
			sc.ResetStats()
		}
		// runUniqueTransfers is capped by the address set, so the divisor has
		// to be the number of transfers actually issued.
		issued := transfers
		start := time.Now()
		if unique {
			gas = runUniqueTransfers(t, cfg, st, users, transfers)
			if issued > len(users)-1 {
				issued = len(users) - 1
			}
		} else {
			gas = runTransfers(t, cfg, st, users, transfers)
		}
		ns = float64(time.Since(start).Nanoseconds()) / float64(issued)
		st, root = mustCommit(t, st, sdb)
		if sc != nil {
			hit = sc.Stats().HitRate()
		}
		return
	}
	if !storageOn {
		// Noise floor: the same configuration on both arms, so the reported
		// spread is pure measurement error.
		return interleave(t, benchRepeats, func(bool) (float64, uint64, common.Hash, float64) {
			return runOnce(false)
		})
	}
	return interleave(t, benchRepeats, runOnce)
}

func erc20KeccakCell(t *testing.T, naddr, transfers int) benchCell {
	runOnce := func(enabled bool) (ns float64, gas uint64, root common.Hash, hit float64) {
		var kc *evmcache.KeccakCache
		if enabled {
			kc = newKeccakCache(4096)
		}
		st, sdb, cfg := newHarness(t, nil, kc)
		users := makeAddrs(naddr)
		for _, u := range users {
			seedBalance(st, u, 1_000_000)
		}
		st = commitReload(t, st, sdb)
		cfg.State = st
		if kc != nil {
			kc.ResetStats()
		}
		start := time.Now()
		gas = runTransfers(t, cfg, st, users, transfers)
		ns = float64(time.Since(start).Nanoseconds()) / float64(transfers)
		st, root = mustCommit(t, st, sdb)
		if kc != nil {
			hit = kc.Stats().HitRate()
		}
		return
	}
	return interleave(t, benchRepeats, runOnce)
}

// crossBlockResult reports one figure per block, so the cold first block can
// be told apart from the warm blocks that follow.
type crossBlockResult struct {
	onNs, offNs           []float64
	hitRate               []float64
	entries               []int
	commitMs              []float64
	totalOnNs, totalOffNs float64
	gas, gasOff           uint64
	purges                uint64
	rootMatch             bool
}

func (c crossBlockResult) speedup() float64 {
	if c.totalOffNs == 0 {
		return 0
	}
	return 100 * (c.totalOffNs - c.totalOnNs) / c.totalOffNs
}

func erc20CrossBlock(t *testing.T, naddr, nblocks, perBlock int) crossBlockResult {
	type blockRun struct {
		ns       []float64
		hit      []float64
		entries  []int
		commitMs []float64
		gas      uint64
		root     common.Hash
		purges   uint64
	}
	run := func(sc *evmcache.StorageCache) blockRun {
		var out blockRun
		st, sdb, cfg := newHarness(t, sc, nil)
		users := makeAddrs(naddr)
		for _, u := range users {
			seedBalance(st, u, 1_000_000)
		}
		st = commitReload(t, st, sdb)

		var prevHits, prevMisses uint64
		for i := 0; i < nblocks; i++ {
			cfg.State = st
			start := time.Now()
			out.gas += runTransfers(t, cfg, st, users, perBlock)
			out.ns = append(out.ns, float64(time.Since(start).Nanoseconds())/float64(perBlock))

			commitStart := time.Now()
			st, out.root = mustCommit(t, st, sdb)
			out.commitMs = append(out.commitMs, float64(time.Since(commitStart).Microseconds())/1000)

			// Differencing the cumulative counters gives a per-block hit rate
			// without resetting the purge count the caller asserts on.
			s := sc.Stats()
			dh, dm := s.Hits-prevHits, s.Misses-prevMisses
			prevHits, prevMisses = s.Hits, s.Misses
			rate := 0.0
			if dh+dm > 0 {
				rate = float64(dh) / float64(dh+dm)
			}
			out.hit = append(out.hit, rate)
			out.entries = append(out.entries, sc.Len())
			out.purges = s.Purges

			// The tag must follow the chain head for the whole run; if it
			// ever lagged, later blocks would silently stop hitting.
			if sc != nil && sc.Root() != out.root {
				t.Fatalf("block %d: cache root tag = %s, want %s", i+1, sc.Root(), out.root)
			}
		}
		return out
	}
	// A single 128-transfer block takes about a millisecond, well inside
	// scheduling and GC noise, so every per-block figure is a mean of
	// interleaved runs rather than one measurement.
	const repeats = benchRepeats
	accumulate := func(acc *blockRun, got blockRun, first bool) {
		if first {
			*acc = got
			return
		}
		if got.root != acc.root || got.gas != acc.gas {
			t.Fatalf("cross-block run diverged: root %s vs %s, gas %d vs %d",
				got.root, acc.root, got.gas, acc.gas)
		}
		for i := range got.ns {
			acc.ns[i] += got.ns[i]
			acc.hit[i] += got.hit[i]
			acc.commitMs[i] += got.commitMs[i]
		}
		acc.entries = got.entries
		acc.purges += got.purges
	}
	mean := func(acc *blockRun) {
		for i := range acc.ns {
			acc.ns[i] /= repeats
			acc.hit[i] /= repeats
			acc.commitMs[i] /= repeats
		}
	}

	measure := func(on bool) blockRun {
		goruntime.GC()
		if on {
			return run(newStorageCache(4096))
		}
		return run(nil)
	}

	run(nil) // discarded warm-up
	run(newStorageCache(4096))

	var off, on blockRun
	for r := 0; r < repeats; r++ {
		// Alternate which arm goes first, as in interleave.
		if r%2 == 0 {
			accumulate(&off, measure(false), r == 0)
			accumulate(&on, measure(true), r == 0)
		} else {
			accumulate(&on, measure(true), r == 0)
			accumulate(&off, measure(false), r == 0)
		}
	}
	mean(&off)
	mean(&on)

	res := crossBlockResult{
		onNs:      on.ns,
		offNs:     off.ns,
		hitRate:   on.hit,
		entries:   on.entries,
		commitMs:  on.commitMs,
		gas:       on.gas,
		gasOff:    off.gas,
		purges:    on.purges,
		rootMatch: on.root == off.root && on.gas == off.gas,
	}
	for i := range on.ns {
		res.totalOnNs += on.ns[i]
		res.totalOffNs += off.ns[i]
	}
	res.totalOnNs /= float64(len(on.ns))
	res.totalOffNs /= float64(len(off.ns))
	return res
}

func erc20KeccakCrossContract(t *testing.T, naddr, ntokens, transfers int) benchCell {
	run := func(kc *evmcache.KeccakCache) (ns float64, gas uint64, root common.Hash, hit float64) {
		sdb := state.NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)
		st, err := state.New(types.EmptyRootHash, sdb)
		if err != nil {
			t.Fatal(err)
		}
		st.CreateAccount(origin)
		st.AddBalance(origin, uint256.NewInt(params.Ether), 0)
		users := makeAddrs(naddr)
		tokens := make([]common.Address, ntokens)
		for i := 0; i < ntokens; i++ {
			tokens[i] = common.BytesToAddress([]byte{0x10, byte(i)})
			st.CreateAccount(tokens[i])
			st.SetCode(tokens[i], TokenRuntime())
			for _, u := range users {
				st.SetState(tokens[i], MappingSlot(u, 0), common.BigToHash(big.NewInt(1_000_000)))
			}
		}
		st = commitReload(t, st, sdb)
		cfg := &vmruntime.Config{
			ChainConfig: params.MergedTestChainConfig,
			GasLimit:    10_000_000,
			State:       st,
			EVMConfig:   vm.Config{EnableKeccakCache: kc != nil, KeccakCache: kc},
		}
		if kc != nil {
			kc.ResetStats()
		}
		start := time.Now()
		var total uint64
		for i := 0; i < transfers; i++ {
			from := users[i%naddr]
			to := users[(i+1)%naddr]
			tok := tokens[i%ntokens]
			cfg.Origin = from
			_, leftover, err := vmruntime.Call(tok, TransferCalldata(to, 1), cfg)
			if err != nil {
				t.Fatal(err)
			}
			total += cfg.GasLimit - leftover
		}
		ns = float64(time.Since(start).Nanoseconds()) / float64(transfers)
		st, root = mustCommit(t, st, sdb)
		gas = total
		if kc != nil {
			hit = kc.Stats().HitRate()
		}
		return
	}
	return interleave(t, benchRepeats, func(on bool) (float64, uint64, common.Hash, float64) {
		if on {
			return run(newKeccakCache(8192))
		}
		return run(nil)
	})
}

func runTransfers(t testing.TB, cfg *vmruntime.Config, st *state.StateDB, users []common.Address, n int) uint64 {
	t.Helper()
	var gas uint64
	for i := 0; i < n; i++ {
		from := users[i%len(users)]
		to := users[(i+1)%len(users)]
		cfg.Origin = from
		cfg.State = st
		_, leftover, err := vmruntime.Call(tokenAddr, TransferCalldata(to, 1), cfg)
		if err != nil {
			t.Fatal(err)
		}
		gas += cfg.GasLimit - leftover
	}
	return gas
}

func runUniqueTransfers(t testing.TB, cfg *vmruntime.Config, st *state.StateDB, users []common.Address, n int) uint64 {
	t.Helper()
	if n > len(users)-1 {
		n = len(users) - 1
	}
	var gas uint64
	for i := 0; i < n; i++ {
		from := users[i]
		to := users[i+1]
		cfg.Origin = from
		cfg.State = st
		_, leftover, err := vmruntime.Call(tokenAddr, TransferCalldata(to, 1), cfg)
		if err != nil {
			t.Fatal(err)
		}
		gas += cfg.GasLimit - leftover
	}
	return gas
}

func makeAddrs(n int) []common.Address {
	out := make([]common.Address, n)
	for i := 0; i < n; i++ {
		out[i] = common.BigToAddress(big.NewInt(int64(i + 1)))
	}
	return out
}

func intHash(i int) common.Hash {
	var h common.Hash
	h[30] = byte(i >> 8)
	h[31] = byte(i)
	return h
}
