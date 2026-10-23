# EVM cache — handoff

Read this file first. It replaces `EVM_CACHE_CHANGE_SUMMARY.md` (what was committed, and why the old design looked the way it did) and `EVM_CACHE_EXPERIMENT_RESULTS.md` (what the working tree measures). Those two files are still in the tree; this one is the current picture.

**Base:** geth `v1.14.13` (`eb00f169`).
**Committed HEAD:** `04f9a550b` — copy-on-write storage cache. That is what `EVM_CACHE_CHANGE_SUMMARY.md` describes. It is **not** the code in the working tree.
**Working tree:** Design D. The storage cache keeps each account's `originStorage` map across blocks. There is no `Get`, `Put`, mutex, or delta/base snapshot.
**Both caches default off.** Gas and the post-state root must match cache-off.

```
geth --cache.evm.storage
geth --cache.evm.keccak  [--cache.evm.keccak.slots=16384]
```

`EVM_CACHE_REDESIGN_PLAN.md` is the measurement that motivated Design D. `EVM_CACHE_HOT_ACCOUNTS.md` and `EVM_CACHE_DIFFLAYER_ANALYSIS.md` describe experiments whose **code was removed** (see the last section). Do not look for `SetHotAccounts`, `DelayStats`, or snapshot `noteDelay` in this tree.

---

## 1. The two gaps

Stock geth already has `dirtyStorage` (this tx), `pendingStorage` (earlier txs in the block), `originStorage` (slots already loaded this block), snapshot `fastcache`, and the trie. Two things still miss:

1. **Cross-block SLOAD.** Commit drops the per-object maps. The next block's first read of the same `(address, slot)` falls through to snapshot or trie.
2. **SHA3 opcode results.** Solidity mapping keys are `keccak256(padded key ‖ slot)`, a 64-byte input that repeats. Geth caches trie hashing, not the SHA3 opcode result.

EIP-2929 warm/cold gas is a separate access list and is not touched.

---

## 2. What HEAD shipped (copy-on-write) — superseded

A process-lifetime `map[(address, slot)]value`, tagged with a state root, consulted in `GetCommittedState` after `originStorage` and before snapshot/trie. Writes only at `StateDB.commit`, after the block succeeded, so a reverted call cannot insert. `BeginCommit(origin)` refuses a commit whose pre-state root is not the cache's root. Reorg / `SetHead` calls `Purge`.

Readers never take a lock. Commit builds new maps and swings an `atomic.Pointer`. A single shared `RWMutex` made a hit cost ~63 ns against ~8–23 ns for a plain map, so that was rejected. Copying the whole table every block cost ~6 ms at 65,536 entries, so the published snapshot was split:

| Map | Holds | Rebuilt |
|---|---|---|
| `delta` | slots from the last few commits | every `EndCommit` |
| `base` | everything older | only on fold (`len(delta) > limit/8`, or 32 commits, or a self-destruct) |

Measured commit cost with that split: ~0.54 ms at 65,536 entries and 256 staged slots. A hit cost **49 ns**.

The mutex around `staged` was not load-bearing: `Get` never looked at those maps, and commit runs on one goroutine after `workers.Wait()`. The copy-on-write existed because live geth **does** SLOAD during commit. `insertChain` prefetches the next block on a throwaway `StateDB` that shares the cache, and RPC `eth_call` can do the same. An `enabled.Store(false)` around an in-place mutation does not wait for a `Get` that already passed the flag.

`pendingStorage` is committed only in the EVM sense (later txs in the block must see it). It is not in the trie until `updateTrie` and not in the snapshot until `StateDB.Commit`. `uncommittedStorage` is a trie worklist, never read by SLOAD.

---

## 3. What the working tree does (Design D)

After `commitStorage`, `stateObject.originStorage` already holds that account's committed slots. `Retain` keeps the map. The next block's `newObject` calls `Adopt` and uses it as `originStorage`. `GetCommittedState` is unchanged on the hit path: the existing `originStorage[key]` probe **is** the cache. A miss is the backend read stock geth was going to do, and it fills `originStorage` as before.

| | COW map (HEAD) | Design D (working tree) |
|---|---|---|
| Read hit | 49 ns, extra code | **23 ns**, existing probe |
| Read miss | extra map probe, then backend | the originStorage miss that was going to happen |
| Commit | copy delta; periodic ~6 ms fold | in-place update, length counter, root retag |
| Self-destruct | rebuild both maps | `delete` one map |
| Budget | 65,536, recency eviction | none; maps grow with the working set |

API in `core/evmcache/storage.go`:

- `Adopt(root, addr)` — return the retained map if the root matches
- `Retain(addr, m)` — keep the map after commit
- `Commit(newRoot)` — retag, no copy
- `Drop(addr)` — self-destruct
- `Purge()` — reorg / `SetHead`

`NewStorageCache(slots)` still takes `slots` so call sites compile. The argument is ignored.

Assumptions, taken as given and not enforced in code: no SLOAD during `Commit`, and no reorgs (a reorg still `Purge`s). Exclusive ownership, witness, `StateDB.Copy` cost, and an entry budget were left out of scope. `EVM_CACHE_REDESIGN_PLAN.md` §0 and §5.1 are the place those assumptions are spelled out.

Keccak is unchanged from HEAD: direct-mapped table, inputs longer than 64 bytes are not stored, a hit compares the full input, collision overwrites, `RWMutex`, hooked only into `opKeccak256`. Preimage recording still runs on both hit and miss.

---

## 4. How it is wired

Production code is a small overlay. New package is `core/evmcache`. Nothing is allocated unless a live object is attached. Disabled means a nil pointer and one nil check.

```
--cache.evm.storage / --cache.evm.keccak
        │
eth/ethconfig.Config → core.CacheConfig
        │
BlockChain.attachEVMCaches()
  ├─ CachingDB.SetStorageCache     process lifetime, shared by every StateDB
  └─ vm.Config.KeccakCache
        │
stateObject.newObject → Adopt into originStorage
GetCommittedState     → existing originStorage probe, then snapshot/trie
StateDB.commit        → Drop destructs, Retain origin+pending maps, Commit(newRoot)
opKeccak256           → Get before hash, Put after
reorg / SetHead       → Purge
```

| File | Role |
|---|---|
| `core/evmcache/config.go` | `Config`, `DefaultStorageSlots=65536` (ignored by Design D), `DefaultKeccakSlots=16384`, `MaxKeccakInput=64` |
| `core/evmcache/storage.go` | Adopt / Retain / Commit / Drop / Purge |
| `core/evmcache/keccak.go` | direct-mapped SHA3 table |
| `core/evmcache/stats.go` | gated counters; `StoragePathStats` (dirty / pending / origin / backend). `Cache` is unused: a cross-block hit is an origin hit |
| `core/state/database.go` | `CachingDB` holds the `*StorageCache` |
| `core/state/statedb.go` | field, `Copy`, commit calls Retain/Drop/Commit. `EnablePathStats` is harness-only |
| `core/state/state_object.go` | `originStorageFor` adopts on `newObject` |
| `core/vm/instructions.go` | SHA3 opcode |
| `core/blockchain.go` | flags, `attachEVMCaches`, `purgeStorageCache` on reorg and `setHeadBeyondRoot` |
| `cmd/utils/flags.go`, `eth/ethconfig`, `eth/backend.go` | flag plumbing |

SLOAD order, cache on:

```
dirtyStorage → pendingStorage → originStorage (includes retained maps)
  → destructed account → empty
  → snapshot / trie
```

Tests: `core/evmcache/cache_test.go`, `core/state/storage_cache_test.go`, `core/vm/keccak256_cache_test.go`, `core/blockchain_cache_test.go`. Experiment harness (not on the production path): `core/evmcache/experiment/`. `token.go` is pinned ERC-20 bytecode.

Not part of the design, still in the git delta vs v1.14.13: a 42 MB `evm` debug ELF, the `block_data` capture gitlink, and missing `tests/testdata` / `tests/evm-benchmarks` submodule gitlinks.

---

## 5. Measured results

Machine for the Design D report: Go 1.22, linux/amd64, Intel i7-14700, 2026-09-03. One binary; on vs off is a flag. Synthetic cells: mean of 20 interleaved pairs, GC before each run. Mainnet cells: median of 101 passes. Effects inside the noise-floor row are not real.

### Per-lookup and per-commit cost

| Level | ns/op |
|---|---:|
| originStorage hit, 1 key | 7.0 |
| originStorage hit, 65,536 keys | 23.0 |
| adopted originStorage hit, 65,536 keys | **23.0** |
| snapshot fastcache hit, 65,536 keys | 106.0 |

The adopted map is the same pointer. Extra cost per SLOAD over stock geth is zero. `Adopt` is once per contract per block. The prize versus snapshot is about **83 ns** (fastcache+RLP minus the map probe; the report also uses 90 ns for the same gap).

Commit (in-place update + Retain + retag), mean over 64 commits. Cost tracks **writes**, not resident size. At 65,536 resident slots: **5.0 µs** for 256 writes, **85 µs** for 2,048 writes. The COW design was 5.7–6.1 ms to copy 65,536 entries, or ~0.5 ms amortized.

Keccak, unchanged: uncached hash 263 ns (32 B) / 246 ns (64 B); hit 19 / 37 ns; miss+insert 20 / 39 ns. Inputs over 64 bytes are not stored.

### Synthetic ERC-20

256 transfers, cache on vs off. Origin share is origin/(origin+backend) on the on-arm. Noise floor (off vs off) was **4.99%**.

| Addresses | speedup | origin share | root |
|---:|---:|---:|---|
| 8 | 0.55% (inside noise) | 100% | match |
| 64 | **+14.7%** | 100% | match |
| 128 | **+17.9%** | 100% | match |
| 256 | **+16.4%** | 100% | match |

Five blocks × 128 transfers over 64 addresses: whole run **+6.8%** (7694 vs 8252 ns/transfer), gas identical, 0 purges, commit ~0.1 ms/block. One block in the middle was negative; that is noise on a ~1 ms block.

SSTORE of the same value is still a state-layer no-op and still costs 106 gas per extra write. The cache holds one entry regardless of repeat count.

Keccak on the same ERC-20 shape: **+16.6%** at N=8 (98% hit), **+11.6%** at N=64 (87% hit). At N=128 (75% hit) the cell was −5%, inside a miss-heavy run. No commit cost.

### Three consecutive mainnet blocks (22000001–22000003)

211 of the captured txs replayed, 45.9M gas, same post-state root in every arm, 0 rejected, 0 purges. Fixtures are a fraction of each block (about 20% / 51% / 65%).

Slots in the declared pre-state that an earlier block also touched:

| Block | touched | also earlier | share |
|---:|---:|---:|---:|
| 22000001 | 229 | 0 | 0% |
| 22000002 | 817 | 21 | **2.6%** |
| 22000003 | 624 | 75 | **12.0%** |

That share is the ceiling. A cross-block cache cannot beat it.

Noise floor (two "both off" arms): **−1.04%**. Median-vs-best spread 11.6–16%. Storage-only was **−0.90%**, keccak-only **−1.45%**, both-on **+0.57%**. All inside the noise. Cold path with storage on: origin 3,188 / backend 1,060 (75% origin, mostly same-block re-reads). The extra origin versus cache-off is **56** reads moved off the backend. Keccak hit **54.1%** (820/1,517).

Counted value of those hits, not the noisy ns/tx column. The cold run is 19.8 ms. Snapshot saving per hit = 90 ns. Trie saving (cold-vs-warm gap) = 607 ns. Keccak hit = 341 ns.

| | saved | commit overhead | net |
|---|---:|---:|---:|
| storage, cold, trie backend | 34 µs | 94 µs | **−60 µs** |
| storage, pre-warmed, trie | 399 µs | 94 µs | **+305 µs** |
| storage, cold, snapshot node | 5 µs | 94 µs | −89 µs |
| storage, pre-warmed, snapshot | 59 µs | 94 µs | −35 µs |
| keccak | 280 µs | 0 | **+280 µs (1.4% of the run)** |

Pre-warmed means genesis already held every slot the txs declare. Its noise floor was +2.42%, so the +3.33% storage-only cell there is barely outside noise.

### Verdict from those runs

| Cache | Synthetic ERC-20 | Three mainnet blocks | Default |
|---|---|---|---|
| Storage, Design D | +15–18% at N≥64; commit ~0.1 ms | Inside noise when cold. +305 µs net if pre-warmed against a trie. Slightly negative against snapshot | **off** |
| Keccak | +12–17% at high reuse | ~1.4% of the run, no commit cost | **off** |

Design D does not create hits the traffic does not have. What it changes versus COW is the loss: a miss is free, and commit on this sample is ~30 µs/block average instead of 0.5–6 ms. On a snapshot-backed node with this traffic the storage cache is tens of microseconds in the red, which is inside the noise. The keccak cache is the one with a visible, commit-free win on both workloads.

Correctness that passed: same-block read is not adopted until commit; block N write is an origin hit in N+1 with backend=0; reverted writes; self-destruct then retain of the new incarnation; reorg purge; nil cache; differential root; same-value SSTORE; keccak rejects long inputs and compares the full input.

---

## 6. How to re-run

```
go test ./core/evmcache/experiment -run TestEVMCacheExperiment -timeout 30m -v
go test ./core/evmcache/experiment -run TestMainnetBlocksCacheImpact -timeout 40m -v
EVMCACHE_BLOCK_DATA=/path/to/frozen/copy go test ./core/evmcache/experiment -run TestMainnetBlocksCacheImpact -timeout 40m -v

go test ./core/evmcache ./core/state ./core/vm -count=1
```

---

## 7. Removed from the tree (do not restore without a reason)

Two later experiments were taken back out so this tree is Design D only.

**Hot-account filter.** `StorageCache.SetHotAccounts` limited `Retain` to an allowlist built offline from the fixtures (an address was hot if one of its slots appeared in two or more blocks). The same list gated keccak to the executing contract. Harness was `TestHotAccountSelectiveCache` on mainnet 23000000–23000009. Writeup: `EVM_CACHE_HOT_ACCOUNTS.md`. The test file is gone.

What it found, so it does not have to be re-measured to remember the shape: on 2 / 5 / 10 blocks the hot-only arm served the same origin hits as retaining every account (backend reads saved vs off: 42, ~289, ~1,025) while keeping 23% / 33% / 64% of the slots. Names at the top were WETH, USDC, USDT, Aave V2, Uniswap v4, plus a long tail. At zero emulated miss delay, ns/tx did not separate the arms (inside a 5–12% spread). The delay columns were a linear model, `delay × backend reads`, not a sleep. They turned positive around 10 µs/miss and were clearly positive at 100 µs, and hot-only tracked all-accounts there.

**Diff-layer walk timing.** Hooks in `core/state/snapshot/difflayer.go` and `disklayer.go`, `delayhint.go`, `core/evmcache/delay.go`, `cmd/evmcache-delay`, and `difflayer_depth_bench_test.go` recorded where an `originStorage` miss resolved and how long it took. Those files are gone. `snapshot/difflayer.go` and `disklayer.go` match HEAD again. Writeup of the microbench: `EVM_CACHE_DIFFLAYER_ANALYSIS.md`.

What that bench found, on the same i7: a diff-layer slot hit is **0.66 µs at depth 1, 0.98 µs at depth 16, 4.44 µs at depth 127**. About **599 ns of the 659 ns** saved by skipping a depth-1 read is the two keccaks of address and slot, not the layer walk. Clean-cache hit ~0.63 µs. An `originStorage` probe in that bench was ~8 ns. Plugging 0.66 µs into the hot-account delay model gave about **+1.3% to +1.9%** on the 5- and 10-block windows, inside the harness spread. The structural point: slots the cache retains were written recently, so they sit at small depth, which is the snapshot's cheap case. The bench's upside was slots that miss the clean cache (~15 µs warm Pebble, ~43 µs NAND in that writeup). A suggested cheaper change, not implemented: cache `addrHash` on the `stateObject` (~300 ns, paid on every slot read today).

The 1.17.6 delay tool and its zip are a separate tree (`go-ethereum-1.17.6`, a symlink). They are not part of this cache and were not reverted.
