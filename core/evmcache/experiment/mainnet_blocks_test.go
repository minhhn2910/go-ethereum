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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/evmcache"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/tests"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

// defaultBlockDataDir holds per-transaction state tests captured from mainnet,
// laid out as <block number>/<tx hash>/txTest.json. The capture writes one
// directory per transaction and fills it in later, so a live directory can be
// a moving target; point EVMCACHE_BLOCK_DATA at a frozen copy to get a
// reproducible run.
const defaultBlockDataDir = "../../../block_data"

func blockDataDir() string {
	if dir := os.Getenv("EVMCACHE_BLOCK_DATA"); dir != "" {
		return dir
	}
	return defaultBlockDataDir
}

// mainnetBlocks are three consecutive mainnet blocks. Consecutive matters:
// the cross-block storage cache can only pay off if block N+1 reads slots
// that block N committed.
var mainnetBlocks = []string{"22000001", "22000002", "22000003"}

// -----------------------------------------------------------------------------
// Fixture parsing
// -----------------------------------------------------------------------------

type fixtureAccount struct {
	Balance string            `json:"balance"`
	Nonce   string            `json:"nonce"`
	Code    string            `json:"code"`
	Storage map[string]string `json:"storage"`
}

type fixtureEnv struct {
	Coinbase   string `json:"currentCoinbase"`
	Difficulty string `json:"currentDifficulty"`
	GasLimit   string `json:"currentGasLimit"`
	Number     string `json:"currentNumber"`
	Timestamp  string `json:"currentTimestamp"`
	BaseFee    string `json:"currentBaseFee"`
	Random     string `json:"currentRandom"`
}

type fixtureTx struct {
	Data     []string `json:"data"`
	GasLimit []string `json:"gasLimit"`
	Value    []string `json:"value"`
	GasPrice string   `json:"gasPrice"`
	Nonce    string   `json:"nonce"`
	Sender   string   `json:"sender"`
	To       string   `json:"to"`
}

type fixture struct {
	EnvInfo struct {
		Env fixtureEnv                `json:"env"`
		Pre map[string]fixtureAccount `json:"pre"`
		Tx  fixtureTx                 `json:"transaction"`
	} `json:"envinfo"`
}

// blockFixture is one block's worth of transactions, in the order the capture
// wrote them out. captured may be below total when the capture only produced
// fixtures for some of the block's transactions.
type blockFixture struct {
	number uint64
	env    fixtureEnv
	txs    []fixture
	total  int
}

func mustBig(t testing.TB, s string) *big.Int {
	t.Helper()
	if s == "" || s == "0x" {
		return new(big.Int)
	}
	v, ok := math.ParseBig256(s)
	if !ok {
		t.Fatalf("bad big int %q", s)
	}
	return v
}

func mustU64(t testing.TB, s string) uint64 {
	t.Helper()
	return mustBig(t, s).Uint64()
}

func mustBytes(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func loadBlocks(t testing.TB) []blockFixture {
	t.Helper()
	var out []blockFixture
	for _, num := range mainnetBlocks {
		dir := filepath.Join(blockDataDir(), num)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Skipf("block data unavailable (%v)", err)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)

		blk := blockFixture{total: len(names)}
		for _, name := range names {
			raw, err := os.ReadFile(filepath.Join(dir, name, "txTest.json"))
			if err != nil {
				continue
			}
			var f fixture
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatalf("%s/%s: %v", num, name, err)
			}
			if len(f.EnvInfo.Tx.Data) == 0 {
				continue
			}
			blk.env = f.EnvInfo.Env
			blk.txs = append(blk.txs, f)
		}
		if len(blk.txs) == 0 {
			t.Skipf("no transactions parsed for block %s", num)
		}
		blk.number = mustU64(t, blk.env.Number)
		out = append(out, blk)
	}
	return out
}

// -----------------------------------------------------------------------------
// Replay
// -----------------------------------------------------------------------------

// seedGenesis writes the union of every transaction's declared pre-state into
// one StateDB and commits it. Earlier transactions win a conflict, so the
// result approximates the chain state just before the first block. Committing
// it separately means the caches start cold, as they would on a real node.
func seedGenesis(t testing.TB, st *state.StateDB, blocks []blockFixture) {
	t.Helper()
	seen := make(map[common.Address]bool)
	for _, blk := range blocks {
		for _, f := range blk.txs {
			for addrHex, acc := range f.EnvInfo.Pre {
				addr := common.HexToAddress(addrHex)
				if seen[addr] {
					continue
				}
				seen[addr] = true
				st.CreateAccount(addr)
				bal, _ := uint256.FromBig(mustBig(t, acc.Balance))
				st.SetBalance(addr, bal, 0)
				st.SetNonce(addr, mustU64(t, acc.Nonce))
				if code := mustBytes(t, acc.Code); len(code) > 0 {
					st.SetCode(addr, code)
				}
				for k, v := range acc.Storage {
					st.SetState(addr, common.HexToHash(k), common.HexToHash(v))
				}
			}
		}
	}
}

// replayResult is what one full pass over the blocks produced. Everything here
// must be identical across cache configurations except the timings.
type replayResult struct {
	blockNs  []float64 // total execution nanoseconds per block
	blockTxs []int     // transactions replayed per block
	commitMs []float64 // commit milliseconds per block
	gas      uint64
	applied  int
	failed   int
	root     common.Hash

	storageHits, storageMisses uint64
	keccakHits, keccakMisses   uint64
	pathOrigin, pathCache      uint64
	pathPending, pathBackend   uint64
	cacheEntries               int
	purges                     uint64
}

func (r replayResult) storageHitRate() float64 {
	if n := r.storageHits + r.storageMisses; n > 0 {
		return float64(r.storageHits) / float64(n)
	}
	return 0
}

func (r replayResult) keccakHitRate() float64 {
	if n := r.keccakHits + r.keccakMisses; n > 0 {
		return float64(r.keccakHits) / float64(n)
	}
	return 0
}

// totalNs is the wall time spent executing transactions, excluding the
// commits, which are reported separately.
func (r replayResult) totalNs() float64 {
	var sum float64
	for _, n := range r.blockNs {
		sum += n
	}
	return sum
}

func (r replayResult) txCount() int {
	var n int
	for _, c := range r.blockTxs {
		n += c
	}
	return n
}

// replayOpts selects a cache configuration for one pass over the blocks.
type replayOpts struct {
	storage *evmcache.StorageCache
	keccak  *evmcache.KeccakCache

	// warmStart keeps whatever the genesis commit staged into the storage
	// cache. Genesis here is the union of every transaction's declared
	// pre-state, so a warm start hands the cache exactly the slots the blocks
	// are about to read — an upper bound, not a realistic steady state. A cold
	// start purges it, so only slots that block N actually committed are
	// available to block N+1.
	warmStart bool

	pathStats bool
}

// replay runs every transaction of every block against one StateDB, committing
// at each block boundary so the storage cache is exercised across blocks.
func replay(t testing.TB, blocks []blockFixture, opts replayOpts) replayResult {
	config := tests.Forks["Shanghai"]
	sc, kc := opts.storage, opts.keccak

	sdb := state.NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)
	sdb.SetStorageCache(sc)
	st, err := state.New(types.EmptyRootHash, sdb)
	if err != nil {
		t.Fatal(err)
	}
	seedGenesis(t, st, blocks)
	root, err := st.Commit(0, true)
	if err != nil {
		t.Fatal(err)
	}
	if st, err = state.New(root, sdb); err != nil {
		t.Fatal(err)
	}
	if !opts.warmStart {
		sc.Purge()
	}

	// Only count what the measured blocks do, not the genesis commit.
	sc.ResetStats()
	kc.ResetStats()

	var res replayResult

	for _, blk := range blocks {
		// Path counters live on the StateDB, which is replaced after every
		// commit, so they have to be re-armed and drained per block.
		var stats *evmcache.StoragePathStats
		if opts.pathStats {
			stats = st.EnablePathStats()
		}
		coinbase := common.HexToAddress(blk.env.Coinbase)
		random := common.HexToHash(blk.env.Random)
		blockCtx := vm.BlockContext{
			CanTransfer: core.CanTransfer,
			Transfer:    core.Transfer,
			GetHash:     func(n uint64) common.Hash { return common.BigToHash(new(big.Int).SetUint64(n)) },
			Coinbase:    coinbase,
			BlockNumber: new(big.Int).SetUint64(blk.number),
			Time:        mustU64(t, blk.env.Timestamp),
			Difficulty:  new(big.Int),
			GasLimit:    mustU64(t, blk.env.GasLimit),
			BaseFee:     mustBig(t, blk.env.BaseFee),
			Random:      &random,
		}
		vmConfig := vm.Config{EnableKeccakCache: kc != nil, KeccakCache: kc}

		start := time.Now()
		for i, f := range blk.txs {
			tx := f.EnvInfo.Tx
			to := common.HexToAddress(tx.To)
			gasLimit := mustU64(t, tx.GasLimit[0])
			gasPrice := mustBig(t, tx.GasPrice)
			value := mustBig(t, tx.Value[0])
			from := common.HexToAddress(tx.Sender)

			// The merged genesis is not a perfectly consistent snapshot, so
			// top the sender up rather than let the transaction bounce off an
			// insufficient balance. This happens identically in every arm.
			need := new(big.Int).Add(new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), gasPrice), value)
			if have := st.GetBalance(from).ToBig(); have.Cmp(need) < 0 {
				topUp, _ := uint256.FromBig(new(big.Int).Sub(need, have))
				st.AddBalance(from, topUp, 0)
			}

			msg := &core.Message{
				From:     from,
				To:       &to,
				Nonce:    st.GetNonce(from),
				Value:    value,
				GasLimit: gasLimit,
				GasPrice: gasPrice,
				// Legacy-priced captures: fee cap and tip both track gasPrice.
				GasFeeCap:        gasPrice,
				GasTipCap:        gasPrice,
				Data:             mustBytes(t, tx.Data[0]),
				SkipNonceChecks:  true,
				SkipFromEOACheck: true,
			}

			st.SetTxContext(common.BigToHash(big.NewInt(int64(i))), i)
			evm := vm.NewEVM(blockCtx, core.NewEVMTxContext(msg), st, config, vmConfig)
			gp := new(core.GasPool).AddGas(blockCtx.GasLimit)
			out, err := core.ApplyMessage(evm, msg, gp)
			if err != nil {
				res.failed++
				continue
			}
			res.applied++
			res.gas += out.UsedGas
		}
		res.blockNs = append(res.blockNs, float64(time.Since(start).Nanoseconds()))
		res.blockTxs = append(res.blockTxs, len(blk.txs))

		if stats != nil {
			_, pending, origin, cached, backend := stats.Snapshot()
			res.pathPending += pending
			res.pathOrigin += origin
			res.pathCache += cached
			res.pathBackend += backend
		}

		commitStart := time.Now()
		root, err = st.Commit(blk.number, true)
		if err != nil {
			t.Fatal(err)
		}
		res.commitMs = append(res.commitMs, float64(time.Since(commitStart).Microseconds())/1000)
		if st, err = state.New(root, sdb); err != nil {
			t.Fatal(err)
		}
	}

	res.root = root
	scStats, kcStats := sc.Stats(), kc.Stats()
	res.storageHits, res.storageMisses = scStats.Hits, scStats.Misses
	res.keccakHits, res.keccakMisses = kcStats.Hits, kcStats.Misses
	res.purges = scStats.Purges
	res.cacheEntries = sc.Len()
	return res
}

// -----------------------------------------------------------------------------
// Experiment
// -----------------------------------------------------------------------------

type cacheConfig struct {
	name        string
	storage     bool
	keccak      bool
	storageSize int
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	} else {
		return (s[n/2-1] + s[n/2]) / 2
	}
}

func minOf(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

// measurement is the outcome of repeated passes for one configuration.
// Scheduling and GC noise only ever adds time, so the median and the best pass
// are both reported: the median is the honest central estimate, the best pass
// is the closest thing to an interference-free run.
type measurement struct {
	samples  []float64   // total execution ns per pass
	perBlock [][]float64 // execution ns per block per pass
	commit   [][]float64 // commit ms per block per pass
	last     replayResult
	txs      int
}

func (m measurement) nsPerTx() float64     { return median(m.samples) / float64(m.txs) }
func (m measurement) bestNsPerTx() float64 { return minOf(m.samples) / float64(m.txs) }

// spread is the gap between the median and the best pass, as a fraction of the
// median. It is the per-arm share of the run-to-run interference.
func (m measurement) spread() float64 {
	return (median(m.samples) - minOf(m.samples)) / median(m.samples)
}

func minSpread(ms []measurement) float64 {
	out := ms[0].spread()
	for _, m := range ms[1:] {
		if s := m.spread(); s < out {
			out = s
		}
	}
	return out
}

func maxSpread(ms []measurement) float64 {
	out := ms[0].spread()
	for _, m := range ms[1:] {
		if s := m.spread(); s > out {
			out = s
		}
	}
	return out
}

// measureConfigs runs every configuration repeats times, rotating which one
// goes first so no arm keeps a privileged position in the cache-warmth and
// scheduling order, and forcing a GC before each measured pass.
func measureConfigs(t *testing.T, blocks []blockFixture, configs []cacheConfig, warmStart bool, repeats int) []measurement {
	build := func(c cacheConfig) replayOpts {
		o := replayOpts{warmStart: warmStart}
		if c.storage {
			o.storage = newStorageCache(c.storageSize)
		}
		if c.keccak {
			o.keccak = newKeccakCache(evmcache.DefaultKeccakSlots)
		}
		return o
	}

	// Discarded: warms the code paths, the allocator and the trie caches.
	for _, c := range configs {
		replay(t, blocks, build(c))
	}

	out := make([]measurement, len(configs))
	for r := 0; r < repeats; r++ {
		for k := range configs {
			idx := (k + r) % len(configs)
			goruntime.GC()
			got := replay(t, blocks, build(configs[idx]))

			m := &out[idx]
			m.samples = append(m.samples, got.totalNs())
			m.perBlock = append(m.perBlock, got.blockNs)
			m.commit = append(m.commit, got.commitMs)
			m.last = got
			m.txs = got.txCount()
		}
	}
	return out
}

// blockStat reduces one block's per-pass samples to a median.
func blockStat(rows [][]float64, block int) float64 {
	xs := make([]float64, len(rows))
	for i, r := range rows {
		xs[i] = r[block]
	}
	return median(xs)
}

// requireSameWork fails unless every arm produced the same post-state, so the
// timings are known to be comparing identical work.
func requireSameWork(t *testing.T, configs []cacheConfig, ms []measurement) {
	t.Helper()
	base := ms[0].last
	for k, c := range configs {
		got := ms[k].last
		if got.root != base.root {
			t.Fatalf("%s: post-state root %s, want %s", c.name, got.root, base.root)
		}
		if got.gas != base.gas {
			t.Fatalf("%s: gas %d, want %d", c.name, got.gas, base.gas)
		}
		if got.applied != base.applied || got.failed != base.failed {
			t.Fatalf("%s: applied %d/failed %d, want %d/%d",
				c.name, got.applied, got.failed, base.applied, base.failed)
		}
	}
}

func writeSummary(b *strings.Builder, configs []cacheConfig, ms []measurement) {
	fmt.Fprintf(b, "| Configuration | ns/tx (median) | vs both off | ns/tx (best pass) | vs both off | storage hit rate | keccak hit rate |\n")
	fmt.Fprintf(b, "|---|---:|---:|---:|---:|---:|---:|\n")
	for k, c := range configs {
		shr, khr := "—", "—"
		if c.storage {
			r := ms[k].last
			shr = fmt.Sprintf("%.1f%% (%d/%d)", 100*r.storageHitRate(), r.storageHits, r.storageHits+r.storageMisses)
		}
		if c.keccak {
			r := ms[k].last
			khr = fmt.Sprintf("%.1f%% (%d/%d)", 100*r.keccakHitRate(), r.keccakHits, r.keccakHits+r.keccakMisses)
		}
		fmt.Fprintf(b, "| %s | %.0f | %+.2f%% | %.0f | %+.2f%% | %s | %s |\n", c.name,
			ms[k].nsPerTx(), 100*(ms[0].nsPerTx()-ms[k].nsPerTx())/ms[0].nsPerTx(),
			ms[k].bestNsPerTx(), 100*(ms[0].bestNsPerTx()-ms[k].bestNsPerTx())/ms[0].bestNsPerTx(),
			shr, khr)
	}
}

// writeOverlap reports how many of each block's touched slots an earlier block
// also touched. A cross-block cache can only ever serve that share, so this is
// the structural ceiling on the hit rate before any timing is measured.
func writeOverlap(b *strings.Builder, blocks []blockFixture) {
	type slotKey struct {
		addr common.Address
		slot common.Hash
	}
	touched := func(blk blockFixture) map[slotKey]bool {
		out := make(map[slotKey]bool)
		for _, f := range blk.txs {
			for addrHex, acc := range f.EnvInfo.Pre {
				addr := common.HexToAddress(addrHex)
				for s := range acc.Storage {
					out[slotKey{addr, common.HexToHash(s)}] = true
				}
			}
		}
		return out
	}

	fmt.Fprintf(b, "| Block | Slots touched | Also touched by an earlier block | Share |\n|---:|---:|---:|---:|\n")
	seen := make(map[slotKey]bool)
	for _, blk := range blocks {
		cur := touched(blk)
		reused := 0
		for k := range cur {
			if seen[k] {
				reused++
			}
		}
		fmt.Fprintf(b, "| %d | %d | %d | %.1f%% |\n", blk.number, len(cur), reused,
			100*float64(reused)/float64(len(cur)))
		for k := range cur {
			seen[k] = true
		}
	}
	fmt.Fprintf(b, "\nSlots are taken from each transaction's declared pre-state. The reuse "+
		"column is the structural ceiling on what a cross-block cache can serve.\n\n")
}

// mainnetRepeats is deliberately far above benchRepeats. One pass over these
// blocks is only ~75 ms, so the effects under test are close to the scheduling
// floor and need many samples to separate from it. Odd, so the median is an
// actual observation.
const mainnetRepeats = 101

// TestMainnetBlocksCacheImpact replays three consecutive mainnet blocks under
// each cache configuration and reports the isolated contribution of each.
func TestMainnetBlocksCacheImpact(t *testing.T) {
	blocks := loadBlocks(t)

	var b strings.Builder
	t.Cleanup(func() { t.Logf("collected report\n\n%s", b.String()) })

	fmt.Fprintf(&b, "## M — Three consecutive mainnet blocks\n\n")
	fmt.Fprintf(&b, "| Block | Transactions replayed | In the block | Coverage |\n|---:|---:|---:|---:|\n")
	total := 0
	for _, blk := range blocks {
		total += len(blk.txs)
		fmt.Fprintf(&b, "| %d | %d | %d | %.0f%% |\n", blk.number, len(blk.txs), blk.total,
			100*float64(len(blk.txs))/float64(blk.total))
	}
	fmt.Fprintf(&b, "\n%d transactions replayed in block order against one state database, "+
		"committing at each block boundary.\n\n", total)
	writeOverlap(&b, blocks)

	// A duplicate of the baseline. Any gap between the two "both off" rows is
	// measurement noise, and no smaller effect than that is worth reading.
	configs := []cacheConfig{
		{name: "both off"},
		{name: "both off (noise floor)"},
		{name: "storage only", storage: true, storageSize: evmcache.DefaultStorageSlots},
		{name: "keccak only", keccak: true},
		{name: "both on", storage: true, keccak: true, storageSize: evmcache.DefaultStorageSlots},
	}

	cold := measureConfigs(t, blocks, configs, false, mainnetRepeats)
	requireSameWork(t, configs, cold)

	base := cold[0].last
	fmt.Fprintf(&b, "Applied %d transactions, %d rejected, %d gas, post-state root %s — identical in every arm.\n\n",
		base.applied, base.failed, base.gas, base.root)

	fmt.Fprintf(&b, "### Cold cache (realistic): only slots this run committed are cacheable\n\n")
	writeSummary(&b, configs, cold)
	fmt.Fprintf(&b, "\nThe two \"both off\" arms are the same configuration measured twice, "+
		"so the %+.2f%% between them is pure measurement noise. Per-arm spread between the "+
		"median and the best pass runs %.1f–%.1f%%. Nothing smaller than that is a real effect.\n",
		100*(cold[0].nsPerTx()-cold[1].nsPerTx())/cold[0].nsPerTx(),
		100*minSpread(cold), 100*maxSpread(cold))

	fmt.Fprintf(&b, "\n### Per block, execution ns/tx (cold, median)\n\n| Block | txs |")
	for _, c := range configs {
		fmt.Fprintf(&b, " %s |", c.name)
	}
	fmt.Fprintf(&b, "\n|---:|---:|")
	for range configs {
		fmt.Fprintf(&b, "---:|")
	}
	fmt.Fprintln(&b)
	for i, blk := range blocks {
		fmt.Fprintf(&b, "| %d | %d |", blk.number, len(blk.txs))
		for k := range configs {
			fmt.Fprintf(&b, " %.0f |", blockStat(cold[k].perBlock, i)/float64(len(blk.txs)))
		}
		fmt.Fprintln(&b)
	}

	fmt.Fprintf(&b, "\n### Commit cost per block, ms (cold, median)\n\n| Block |")
	for _, c := range configs {
		fmt.Fprintf(&b, " %s |", c.name)
	}
	fmt.Fprintf(&b, "\n|---:|")
	for range configs {
		fmt.Fprintf(&b, "---:|")
	}
	fmt.Fprintln(&b)
	for i, blk := range blocks {
		fmt.Fprintf(&b, "| %d |", blk.number)
		for k := range configs {
			fmt.Fprintf(&b, " %.2f |", blockStat(cold[k].commit, i))
		}
		fmt.Fprintln(&b)
	}

	// Upper bound. Genesis here is the union of every transaction's declared
	// pre-state, so keeping what it staged hands the cache exactly the slots
	// the three blocks are about to read.
	warm := measureConfigs(t, blocks, configs, true, mainnetRepeats)
	requireSameWork(t, configs, warm)
	fmt.Fprintf(&b, "\n### Pre-warmed cache (upper bound): every slot these blocks read is already cached\n\n")
	writeSummary(&b, configs, warm)

	// Where the committed-state reads landed. This is what caps the win.
	for _, w := range []struct {
		label string
		warm  bool
	}{{"cold", false}, {"pre-warmed", true}} {
		paths := replay(t, blocks, replayOpts{
			storage:   newStorageCache(evmcache.DefaultStorageSlots),
			warmStart: w.warm,
			pathStats: true,
		})
		fmt.Fprintf(&b, "\n### Where committed-state reads resolved (%s, storage cache on)\n\n", w.label)
		fmt.Fprintf(&b, "| Path | Count | Share |\n|---|---:|---:|\n")
		sum := paths.pathPending + paths.pathOrigin + paths.pathCache + paths.pathBackend
		for _, row := range []struct {
			name string
			n    uint64
		}{
			{"pending (written by an earlier tx in this block)", paths.pathPending},
			{"origin (already loaded earlier in this block)", paths.pathOrigin},
			{"cross-block storage cache", paths.pathCache},
			{"backend (snapshot / trie)", paths.pathBackend},
		} {
			fmt.Fprintf(&b, "| %s | %d | %.1f%% |\n", row.name, row.n, 100*float64(row.n)/float64(sum))
		}
		fmt.Fprintf(&b, "\n%d entries held at the end, %d purges.\n", paths.cacheEntries, paths.purges)
		if paths.purges != 0 {
			t.Fatalf("%s: cache purged %d times across three consecutive blocks", w.label, paths.purges)
		}
	}

	// The commit-side rebuild scales with the budget while the hit rate does
	// not, so on a workload with little cross-block reuse a smaller table can
	// net out ahead of a larger one.
	fmt.Fprintf(&b, "\n### Storage cache budget sweep (cold, storage only)\n\n")
	fmt.Fprintf(&b, "| Budget | ns/tx | vs both off | hit rate | entries held |\n|---:|---:|---:|---:|---:|\n")
	sweep := []cacheConfig{{name: "off"}}
	for _, size := range []int{1024, 4096, 16384, 65536} {
		sweep = append(sweep, cacheConfig{
			name: fmt.Sprintf("%d", size), storage: true, storageSize: size,
		})
	}
	sm := measureConfigs(t, blocks, sweep, false, mainnetRepeats)
	requireSameWork(t, sweep, sm)
	for k, c := range sweep[1:] {
		r := sm[k+1]
		fmt.Fprintf(&b, "| %s | %.0f | %+.2f%% | %.1f%% | %d |\n", c.name, r.nsPerTx(),
			100*(sm[0].nsPerTx()-r.nsPerTx())/sm[0].nsPerTx(), 100*r.last.storageHitRate(), r.last.cacheEntries)
	}

	writeBudget(&b, cold, warm)
}

// writeBudget converts hit counts into the wall time they can possibly save.
// Timings on a 75 ms run sit close to the interference floor, so counting the
// saved work is the more trustworthy estimate of what the caches are worth
// here.
func writeBudget(b *strings.Builder, cold, warm []measurement) {
	const (
		offIdx     = 0
		storageIdx = 2
		keccakIdx  = 3
	)
	runNs := median(cold[offIdx].samples)

	// Marginal value of a storage cache hit, measured rather than assumed:
	// the pre-warmed arm does identical work to the cold arm except that this
	// many reads resolve in the cache instead of going to the backend.
	coldHits := float64(cold[storageIdx].last.storageHits)
	warmHits := float64(warm[storageIdx].last.storageHits)
	perStorageHit := (median(cold[storageIdx].samples) - median(warm[storageIdx].samples)) / (warmHits - coldHits)

	// Marginal value of a keccak cache hit, from the same microbenchmark the
	// K-A cell uses: a 64-byte mapping-key hash against a table hit.
	perKeccakHit := keccakSaving()
	keccakHits := float64(cold[keccakIdx].last.keccakHits)

	// This harness has no snapshot layer, so a backend read walks the trie.
	// A full node answers most of those from the snapshot's fastcache instead,
	// which is an order of magnitude cheaper and leaves the cache far less to
	// win. Both are priced so the gap is explicit.
	perSnapHit := snapshotSaving()

	fmt.Fprintf(b, "\n### What the hits can possibly be worth\n\n")
	fmt.Fprintf(b, "The whole run executes in %.1f ms. Multiplying each cache's hit count by "+
		"the work one hit avoids bounds the win independently of the timing noise.\n\n", runNs/1e6)
	fmt.Fprintf(b, "| Component | Hits | Saved per hit | Total saved | Share of run |\n|---|---:|---:|---:|---:|\n")
	for _, row := range []struct {
		name   string
		hits   float64
		perHit float64
	}{
		{"storage cache, cold, trie backend (this harness)", coldHits, perStorageHit},
		{"storage cache, pre-warmed, trie backend (this harness)", warmHits, perStorageHit},
		{"storage cache, cold, snapshot backend (full node)", coldHits, perSnapHit},
		{"storage cache, pre-warmed, snapshot backend (full node)", warmHits, perSnapHit},
		{"keccak cache", keccakHits, perKeccakHit},
	} {
		saved := row.hits * row.perHit
		fmt.Fprintf(b, "| %s | %.0f | %.0f ns | %.0f µs | %.2f%% |\n",
			row.name, row.hits, row.perHit, saved/1e3, 100*saved/runNs)
	}
	fmt.Fprintf(b, "\nThe trie figure comes from the run itself: the cold and pre-warmed arms do "+
		"identical work apart from %.0f reads moving off the backend, so the gap between them "+
		"divided by that count is the marginal value of one hit. It is a difference of two noisy "+
		"medians, so read it as a microsecond-scale trie walk rather than a precise number. The "+
		"snapshot figure is a direct microbenchmark — a fastcache lookup and decode less a cache "+
		"lookup, both tables at 65536 keys — and is the one that applies to a full node.\n",
		warmHits-coldHits)

	// The storage cache is not free: every commit restages and republishes.
	// That cost lands whether or not the block gets any hits.
	var commitOff, commitOn float64
	for i := range cold[offIdx].commit[0] {
		commitOff += blockStat(cold[offIdx].commit, i)
		commitOn += blockStat(cold[storageIdx].commit, i)
	}
	overhead := (commitOn - commitOff) * 1e6 // ms -> ns

	fmt.Fprintf(b, "\n### Storage cache, net of its own commit overhead\n\n")
	fmt.Fprintf(b, "Committing the three blocks takes %.2f ms with the cache off and %.2f ms "+
		"with it on, so staging and republishing costs %.0f µs per three blocks. Against that:\n\n",
		commitOff, commitOn, overhead/1e3)
	fmt.Fprintf(b, "| Scenario | Saved | Commit overhead | Net |\n|---|---:|---:|---:|\n")
	for _, row := range []struct {
		name  string
		saved float64
	}{
		{"cold, trie backend", coldHits * perStorageHit},
		{"pre-warmed, trie backend", warmHits * perStorageHit},
		{"cold, snapshot backend", coldHits * perSnapHit},
		{"pre-warmed, snapshot backend", warmHits * perSnapHit},
	} {
		fmt.Fprintf(b, "| %s | %.0f µs | %.0f µs | %+.0f µs |\n",
			row.name, row.saved/1e3, overhead/1e3, (row.saved-overhead)/1e3)
	}
	fmt.Fprintf(b, "\nThe keccak cache has no commit-side cost, so its %.0f µs is net.\n",
		keccakHits*perKeccakHit/1e3)
}

// snapshotSaving is what a storage cache hit avoids on a node whose snapshot
// layer answers the read: a fastcache lookup and RLP decode, less the cache
// lookup that replaces it. Both tables hold 65536 keys so neither gets an
// unrepresentative all-in-L1 result.
func snapshotSaving() float64 {
	const wide = 65536
	addr := common.BytesToAddress([]byte("snapbench"))
	val := common.BigToHash(big.NewInt(0x2a))

	keys := make([]common.Hash, wide)
	stors := make([]common.Hash, wide)
	for i := range keys {
		keys[i] = common.BigToHash(big.NewInt(int64(i)))
		stors[i] = crypto.Keccak256Hash(keys[i][:])
	}

	fc := fastcache.New(16 << 20)
	acc := crypto.Keccak256Hash(addr[:])
	blob, _ := rlp.EncodeToBytes(common.TrimLeftZeroes(val[:]))
	for i := range keys {
		fc.Set(append(acc[:], stors[i][:]...), blob)
	}

	root := common.HexToHash("0x01")
	sc := evmcache.NewStorageCache(wide)
	sc.BeginCommit(common.Hash{})
	for _, k := range keys {
		sc.Put(addr, k, val)
	}
	sc.EndCommit(root)

	const iters = 2_000_000
	goruntime.GC()
	start := time.Now()
	for i := 0; i < iters; i++ {
		stor := stors[i%wide]
		got, _ := fc.HasGet(nil, append(acc[:], stor[:]...))
		_, content, _, _ := rlp.Split(got)
		var out common.Hash
		out.SetBytes(content)
	}
	snap := float64(time.Since(start).Nanoseconds()) / iters

	goruntime.GC()
	start = time.Now()
	for i := 0; i < iters; i++ {
		sc.Get(root, addr, keys[i%wide])
	}
	cached := float64(time.Since(start).Nanoseconds()) / iters

	return snap - cached
}

// keccakSaving is the wall time a keccak cache hit avoids, for the 64-byte
// mapping-key hashes that dominate this workload.
func keccakSaving() float64 {
	data := make([]byte, 64)
	for i := range data {
		data[i] = byte(i)
	}
	cache := evmcache.NewKeccakCache(evmcache.DefaultKeccakSlots)
	cache.Put(data, crypto.Keccak256Hash(data))

	const iters = 200000
	goruntime.GC()
	start := time.Now()
	for i := 0; i < iters; i++ {
		crypto.Keccak256Hash(data)
	}
	uncached := float64(time.Since(start).Nanoseconds()) / iters

	goruntime.GC()
	start = time.Now()
	for i := 0; i < iters; i++ {
		cache.Get(data)
	}
	cached := float64(time.Since(start).Nanoseconds()) / iters

	return uncached - cached
}
