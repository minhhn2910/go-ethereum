# EVM Cache Changes vs go-ethereum v1.14.13

**Base:** `v1.14.13` (`eb00f1694c9265f6909c19995a535eef246dcf1e`)
**HEAD:** `04f9a550b` — `modified version`
**Scope:** optional, default-off storage cache and SHA3/KECCAK256 result cache for EVM execution.

**The storage cache described in §2.1 below is the previous copy-on-write design.** It was replaced by Design D: retain each account's `originStorage` map across blocks (`Adopt` / `Retain` / `Commit` / `Drop` / `Purge`). There is no `Get`, `Put`, mutex, or delta/base snapshot. See `EVM_CACHE_REDESIGN_PLAN.md` for the rationale and `EVM_CACHE_EXPERIMENT_RESULTS.md` for numbers on the shipped design.

This file is an inspection report of the git delta: what was added, how it is wired, and why it looks the way it does. Measured numbers live in `EVM_CACHE_EXPERIMENT_RESULTS.md`. The original problem statement lives in `CACHE_ANALYSIS_SUMMARY.md` and `EVM_CACHE_EXPERIMENT_PLAN.md`.

```
git diff --stat v1.14.13 HEAD
```

```
 CACHE_ANALYSIS_SUMMARY.md                       | 1393 +++++++++++++++++++++++
 EVM_CACHE_EXPERIMENT_PLAN.md                    |  167 +++
 EVM_CACHE_EXPERIMENT_RESULTS.md                 |  438 +++++++
 block_data                                      |    1 +
 cmd/geth/chaincmd.go                            |    4 +
 cmd/geth/main.go                                |    4 +
 cmd/utils/flags.go                              |   34 +
 core/blockchain.go                              |   47 +
 core/blockchain_cache_test.go                   |   83 ++
 core/evmcache/cache_test.go                     |  379 ++++++
 core/evmcache/config.go                         |   84 ++
 core/evmcache/experiment/experiment_test.go     | 1052 +++++++++++++++++
 core/evmcache/experiment/mainnet_blocks_test.go |  924 +++++++++++++++
 core/evmcache/experiment/token.go               |  131 +++
 core/evmcache/keccak.go                         |  184 +++
 core/evmcache/stats.go                          |  108 ++
 core/evmcache/storage.go                        |  368 ++++++
 core/state/database.go                          |   16 +
 core/state/state_object.go                      |   25 +
 core/state/statedb.go                           |   57 +-
 core/state/storage_cache_test.go                |  310 +++++
 core/vm/evm.go                                  |    4 +
 core/vm/instructions.go                         |   14 +
 core/vm/interpreter.go                          |    6 +
 core/vm/keccak256_cache_test.go                 |  116 ++
 eth/backend.go                                  |    4 +
 eth/ethconfig/config.go                         |    6 +
 eth/ethconfig/gen_config.go                     |   24 +
 evm                                             |  Bin 0 -> 43703472 bytes
 tests/evm-benchmarks                            |    1 -
 tests/testdata                                  |    1 -
 31 files changed, 5982 insertions(+), 3 deletions(-)
```

Production code is a small overlay on stock geth: ~200 lines across `core/state`, `core/vm`, `core/blockchain`, `eth/`, and `cmd/`. The rest is a new package, tests, experiment harness, and docs.

---

## 1. Rationale

Stock geth already caches storage in several places:

| Layer | Lifetime | What it holds |
|---|---|---|
| `dirtyStorage` | current transaction | writes of this call |
| `pendingStorage` | current block | writes of earlier txs |
| `originStorage` | current block | slots already loaded this block |
| snapshot `fastcache` | process | committed slots, hashed keys, RLP |
| trie / disk | process | everything else |

Two gaps remain:

1. **Cross-block SLOAD.** At commit, geth drops the cheap per-object maps. The next block's first read of the same `(address, slot)` falls through to snapshot/trie. A small process-lifetime table tagged with the state root can serve that first-in-block read.
2. **SHA3 results.** Solidity mappings hash `keccak256(padded key ‖ slot)` — a 64-byte input that repeats. Geth caches trie hashing, not the SHA3 *opcode result*. A tiny table for ≤64-byte inputs can skip the hash.

Both must leave **gas and the post-state root unchanged**. They are performance only. They default **off**, so an unmodified flag set is bit-identical to v1.14.13.

Enable:

```
geth --cache.evm.storage [--cache.evm.storage.slots=65536]
geth --cache.evm.keccak  [--cache.evm.keccak.slots=16384]
```

---

## 2. Design

Both caches live in `core/evmcache`. Nothing is allocated unless a live object is attached. Disabled caches are `nil` pointers on the existing structs, so the hot path is one extra nil check.

### 2.1 Storage cache (`core/evmcache/storage.go`)

A bounded map `(address, slot) → committed value` tagged with a **state root**.

**Lookup order** (in `stateObject.GetCommittedState`):

```
dirty → pending → origin → [storage cache] → snapshot / trie
```

The cache is consulted only after the maps that already exist, and only for committed state. EIP-2929 warm/cold gas is independent and is not touched.

The rest of this section is why the table is not a plain `map` with a mutex.

#### Why writes happen only at block commit

The cache is allowed to hold **committed** values only. During a transaction, SSTORE can write a slot and then the whole call can revert. If the cache were updated on SSTORE, a later block could read a value that never became canonical.

Geth already solves that for its own maps: `dirtyStorage` is journaled and rolled back; `pendingStorage` / `originStorage` are dropped at commit. The cross-block cache must not be more eager than that. So `Put` is called from `StateDB.commit`, after the block has succeeded, and `Get` never sees those Puts until `EndCommit` publishes them.

Self-destruct is the same rule in reverse: `InvalidateAccount` is registered in the same commit, **before** the Puts, so an account destroyed and recreated in one block keeps its new slots and loses the old ones.

`BeginCommit(origin)` is a guard on top of that. The cache is tagged with the root it was built from. If this StateDB's pre-state root is not that root (side block, historical query), the commit is refused and the table is left alone. A root mismatch on `Get` is just a miss; only reorg / `SetHead` call `Purge`.

#### Why a copy-on-write snapshot, not a locked map

SLOAD is on the hot path and can run concurrently with block commit (prefetch, RPC, the next block starting while the previous commit finishes). A `sync.RWMutex` around one shared map would put a lock on every SLOAD. The first version of this cache did exactly that; a hit cost ~63 ns against ~8–23 ns for a plain map.

Copy-on-write removes the lock from readers:

1. Readers load `cur` (one atomic pointer) and look in maps that **nobody mutates**.
2. The commit path never edits those maps in place. It builds new maps and stores a new `storageSnapshot` into `cur`.
3. Old snapshots become unreachable and are GC'd. Until then, a reader that loaded the previous pointer still has a consistent view.

That is the whole of “copy-on-write”: mutate a private copy, then swing the pointer. Go maps are not safe for concurrent write+read, so the published maps **must** be immutable. Hence `dropDead` and `fold` always allocate a new map instead of `delete` on the live one.

`staged` / `destructed` are the private copy for the commit in progress. They sit behind `mu` because only commit touches them; `Get` never looks at them. That is also why a reverted call cannot poison the cache: it never reached `commit`, so it never reached `staged`.

The struct is two worlds sharing one object:

```
readers (SLOAD)                         writer (block commit)
───────────────                         ────────────────────
cur.Load()  →  snapshot {root,          mu.Lock()
                 delta, base}             staged[key] = value
look up, no lock                          destructed[addr] = {}
                                          rebuild maps
                                          cur.Store(new snapshot)
                                          mu.Unlock()
```

`enabled` is an atomic bool so `SetEnabled(false)` can turn the cache into a no-op without dropping it. `limit` is the max number of entries. `stats` is optional hit/miss counting (off by default so Get does not pay an atomic increment).

#### Why two maps (`delta` and `base`) instead of one

If `EndCommit` copied the entire table every block, a 65,536-entry cache cost **~6 ms per commit** — more than the lookups save. Most of that table did not change this block. The split exists only to avoid copying the unchanged part:

| Map | What it holds | When it is rebuilt |
|---|---|---|
| `delta` | slots from the last few commits | **every** `EndCommit` (copy old delta + this block's `staged`) |
| `base` | everything older | only on **fold** |

`deltaLimit` is `limit/8` (8,192 at the default 65,536). As long as `len(delta)` stays under that, `base` is reused by pointer: the new snapshot is `{root: newRoot, base: prev.base, delta: merged}`. Sharing `base` is safe because it is immutable.

**Fold** is “merge delta into a fresh base, start a new empty delta.” It runs when any of these is true:

- `len(delta) > deltaLimit` — the cheap map has grown too large
- `sinceFold >= 32` — even a quiet chain eventually rebuilds, so over-budget old entries can be dropped
- this block self-destructed an account — dead slots may sit in `base`, which we cannot `delete` in place, so both maps are copied without those addresses (`dropDead`) and then folded

On fold, delta is inserted first, then base, until `limit` is reached. That is recency-biased eviction: slots written recently survive; stale `base` entries are the ones dropped. After fold, `delta` is `nil` and `sinceFold` resets to 0.

A typical stretch of blocks, no self-destruct:

```
block N     staged → new delta_N,     base unchanged
block N+1   delta_N + staged → delta, base unchanged
...
block N+k   delta too big → fold →    new base, empty delta
```

Get checks `delta` first, then `base`, because a slot in both has its latest value in delta.

Measured commit cost with this split: ~0.54 ms/commit at 65,536 entries and 256 staged slots; ~0.14 ms at 4,096. That is the only reason `delta`/`base` exist. A single map plus a mutex would be simpler and was slower on the read path; a single map copied every commit would be simpler and was slower on the write path.

#### The fields, in that light

```go
type StorageCache struct {
    enabled atomic.Bool                      // on/off without dropping contents
    cur     atomic.Pointer[storageSnapshot]  // published, immutable, lock-free reads

    mu         sync.Mutex                    // commit path only
    staged     map[storageKey]common.Hash    // this block's Puts, invisible until EndCommit
    destructed map[common.Address]struct{}   // this block's self-destructs
    sinceFold  int                           // commits since last base rebuild

    limit      int                           // max entries after a fold
    deltaLimit int                           // fold when len(delta) exceeds this (limit/8)
    stats      counters
}
```

`sinceFold` is not a third cache. It is a counter that forces a fold every 32 commits so eviction cannot stall forever on a chain that writes only a handful of slots per block.

### 2.1a Critical questions (mutex, pending vs uncommitted, copies)

These three questions cut at the design rather than restating it.

#### 1. Do we need the mutex? Could we disable the cache during commit instead?

There are two different mutexes people mix up.

**`mu` on `staged` / `destructed` is almost not doing the job people think.** `Get` never looks at those maps. `Put` / `InvalidateAccount` / `EndCommit` run from `StateDB.commit` after `workers.Wait()`, on one goroutine. The blockchain insert path is already serialized by `chainmu`. The follow-up prefetcher never calls `StateDB.Commit`. So a single-writer assumption for *staging* is already true on the live node. The mutex is defensive, not load-bearing.

**The real concurrency is `Get` vs publishing a new snapshot**, and that is not what `mu` protects. `Get` is lock-free on `cur`. Copy-on-write exists so a reader can be inside `delta[key]` while the writer allocates a new map and `cur.Store`s it.

“Disable the cache during commit, then mutate in place, no mutex” is almost right, and it fails on a happens-before, not on a missing lock around `staged`:

```
enabled.Store(false)     // Get now returns miss
mutate the live map      // still racy if a Get already passed the enabled check
enabled.Store(true)
```

An atomic bool does not wait for in-flight Gets. A Get that loaded `enabled==true` can still be hashing a key while you `insert`/`delete`. That is a data race. To make disable-during-commit safe you still need one of: a `RWMutex` (readers hold RLock across the map lookup — back to the 63 ns path), a generation/wait-group of in-flight Gets, or copy-on-write (don't mutate what readers might hold).

And **SLOAD does happen while the canonical block is committing.** `insertChain` starts a throwaway `StateDB` on the same `CachingDB` and prefetches the next block concurrently:

```1843:1851:core/blockchain.go
		if !bc.cacheConfig.TrieCleanNoPrefetch {
			if followup, err := it.peek(); followup != nil && err == nil {
				throwaway, _ := state.New(parent.Root, bc.statedb)
				go func(...) {
					bc.prefetcher.Prefetch(followup, throwaway, vmCfg, &followupInterrupt)
```

`state.New(..., bc.statedb)` copies the storage-cache pointer. Prefetch runs `ApplyMessage` → SLOAD → `Get` on that cache while `processBlock` may be in `statedb.Commit` → `EndCommit`. RPC `eth_call` / `eth_getStorageAt` can do the same. So “assume SLOAD is not performed when committing” is false of a live geth, even if it is true of the experiment harness.

If we *don't attach the cache to throwaway/RPC StateDBs* and we *don't mutate maps until we know no Get is in flight*, then yes: no mutex, no COW, in-place insert at commit. That is a simpler cache. The current code did not take that restriction, so it paid for lock-free reads with copies.

#### 2. `pendingStorage` is not the trie, and `uncommittedStorage` is not a read cache

Four maps on `stateObject`. Only three are on the SLOAD/SSTORE *value* path. `uncommittedStorage` is a trie worklist.

```57:70:core/state/state_object.go
	originStorage  Storage // slots already loaded this block (committed-at-block-start, plus later overlays)
	dirtyStorage   Storage // writes of the current transaction (journaled, revertible)
	pendingStorage Storage // writes that survived Finalise (earlier txs in this block)
	uncommittedStorage Storage // original values of slots that still need a trie Update/Delete
```

**SSTORE** (`SetState`):

1. `getState` → `GetCommittedState` (original for this tx) plus `dirtyStorage` (current-tx overlay).
2. If new value equals current, no-op (still pays warm-read gas at the EVM layer).
3. Else journal `(prev, origin)` and write `dirtyStorage`. If the write returns to `origin`, the dirty entry is deleted — the tx is a net no-op and a revert of the journal restores the same.

A reverted CALL never reaches `Finalise`, so it never leaves `dirtyStorage`.

**End of tx** (`StateDB.Finalise` → `stateObject.finalise`), post-Byzantium every tx; pre-Byzantium also followed by `IntermediateRoot`:

- Each dirty key is copied into `pendingStorage` (latest value this block).
- If this is the first time the slot was dirtied since the last trie flush, `uncommittedStorage[key] = GetCommittedState(key)` — the **pre-mutation** value, used later as the trie “from”.
- If a later tx writes the slot back to that original, the `uncommittedStorage` entry is deleted (net no-op for the trie) but `pendingStorage` still holds the value (needed if a pre-Byzantium intermediate commit had already flushed an earlier write).
- `dirtyStorage` is replaced with an empty map. Journal is cleared. Cross-tx revert is impossible.

**SLOAD** (`GetState` → `getState`):

```
dirtyStorage          current tx, including nested CALLs not yet reverted
    ↓ miss
pendingStorage        earlier txs in this block (Finalise'd, will not revert)
    ↓ miss
originStorage         already loaded this block (block-start committed value,
                      or a pending value copied in at trie commit)
    ↓ miss
stateObjectsDestruct  account self-destructed this block → empty slot
    ↓ miss
[storage cache]       previous block's committed value at originalRoot
    ↓ miss
snapshot / trie
```

`GetCommittedState` is that pipeline without `dirtyStorage`. SSTORE gas (original slot) uses that path; SLOAD uses `GetState`, which is dirty-or-committed.

**`pendingStorage` is “committed” only in the EVM sense, not the disk sense.** After `Finalise`, a later tx in the same block must see the write (Yellow Paper: txs in a block apply sequentially). It will not revert. It is **not** in the storage trie until `updateTrie`, and not in the snapshot until `StateDB.Commit`. That is why the cross-block cache is filled at `StateDB.commit`, not at `Finalise`. Filling it at `Finalise` would publish values before the block has a canonical root, and before a reorg can still drop the block.

**`uncommittedStorage` is never consulted by SLOAD or SSTORE.** It answers “which slots still need `trie.UpdateStorage` / `DeleteStorage`, and what was the value before this epoch of mutations?”

- Pre-Byzantium: `IntermediateRoot` after every tx runs `updateTrie`, which writes those keys and **resets** `uncommittedStorage`. The map is per-tx.
- Post-Byzantium (mainnet today): `Finalise` only, trie flush at block `Commit`. The map accumulates the whole block.

`updateTrie` iterates `uncommittedStorage`, reads the live value from `pendingStorage`, writes the trie, then clears the worklist. Later `commitStorage` copies `pendingStorage` over `originStorage` (skipping no-ops) and clears `pendingStorage`. After a successful `StateDB.Commit`, `originStorage` holds the new committed values and `pending`/`uncommitted`/`dirty` are empty. The next block's `StateDB` is a new object; those maps start empty. That is the gap the cross-block cache is trying to fill.

#### 3. Why copy at all, if we drop the mutex?

Copying is not a substitute for `mu` on `staged`. It is the price of **lock-free readers on an immutable Go map**.

If `Get` never runs during `EndCommit` (harness, or cache not shared with prefetch/RPC) **and** in-flight Gets cannot overlap a map mutation, then `EndCommit` can `m[k]=v` / `delete(m,k)` on one live map. No snapshot, no `delta`/`base`, no 6 ms copy. The user's scenario is internally consistent.

The current code did not take that restriction. Readers may hold a map pointer. Therefore:

- You cannot insert into the published map.
- You publish a new map (or a new `delta` plus the old `base` pointer).
- The “copy the whole table every block” cost was the naive version of that. `delta`/`base` exists only to stop copying `base`. It is complexity purchased to make COW affordable, not to make the cache correct.

Disable-during-commit does **not** by itself remove the copy, unless you also prevent in-flight Gets (see Q1). If you do prevent them, disable-during-commit plus in-place mutation is the simpler design, and `delta`/`base` becomes unnecessary.

**Bottom line.** The mutex on `staged` is over-engineered for the actual writer set. The COW / delta / base machinery is a real answer to concurrent Get-during-commit, which does happen on a live node via prefetch. If we exclude those readers from the cache, the whole snapshot design can collapse to one map updated at `StateDB.Commit`. That is the fork in the road, not a missing comment.

A direct-mapped table of SHA3/KECCAK256 **results**, hooked only into the SHA3 opcode (`opKeccak256`). Not used for trie hashing.

- Inputs longer than **64 bytes** are never stored (Solidity mapping keys are 64 bytes).
- A hit requires a **full input-byte compare**; the table index is never trusted alone.
- Collision overwrites the occupant (no LRU).
- Entries never go stale (keccak is a pure function).
- `RWMutex` around the table; Get takes `RLock`, Put takes `Lock`.

### 2.3 Correctness constraints

- Gas and Merkle root must match cache-off on every differential test.
- Writes only at block commit; reverts cannot insert.
- Reorg / `SetHead` purges the storage cache.
- Disabled = stock path (nil object, or `SetEnabled(false)`).

---

## 3. Skeleton

How the pieces sit on top of v1.14.13. New files are marked **new**; everything else is a small hook.

```
cmd/geth, cmd/utils
  --cache.evm.storage / .slots
  --cache.evm.keccak  / .slots
        │
        ▼
eth/ethconfig.Config  ──►  core.CacheConfig
        │
        ▼
core.BlockChain.attachEVMCaches()
  ├─ CachingDB.SetStorageCache(StorageCache)     process lifetime
  └─ vm.Config.KeccakCache = KeccakCache         process lifetime
        │
        ├──────────────────────────────────────────┐
        ▼                                          ▼
core/state                                    core/vm
  GetCommittedState                           opKeccak256
    after originStorage,                      Get before hash
    before snapshot/trie                      Put after hash
  StateDB.commit
    InvalidateAccount (destructs)
    Put (originStorage + pendingStorage)
    EndCommit(newRoot)
        │
        ▼
core/evmcache  (new package)
  config.go      Config, defaults, nextPow2
  storage.go     COW map, Begin/EndCommit, Purge
  keccak.go      direct-mapped SHA3 table
  stats.go       gated hit/miss, StoragePathStats
  cache_test.go  unit tests
```

### 3.1 New package — `core/evmcache`

| File | Role |
|---|---|
| `config.go` | `Config{Enable*, *Slots}`, `DefaultStorageSlots=65536`, `DefaultKeccakSlots=16384`, `MaxKeccakInput=64` |
| `storage.go` | `StorageCache`: Get / Put / InvalidateAccount / BeginCommit / EndCommit / Purge / fold |
| `keccak.go` | `KeccakCache`: Get / Put / Purge, FNV-style index, full-input compare |
| `stats.go` | `Stats`, gated `counters`, `StoragePathStats` (dirty/pending/origin/cache/backend) |
| `cache_test.go` | collision, purge, disable, root mismatch, destruct/recreate, fold, stats gating, concurrent Get vs EndCommit |

### 3.2 Production hooks (minimal overlay)

**`core/state/database.go`** — `CachingDB` holds `*evmcache.StorageCache`. `SetStorageCache` / `StorageCache` attach or read it. Every `StateDB` opened from the db inherits the same object.

**`core/state/statedb.go`** — field `storageCache`; copied on `Copy()`; populated from the db in `New`. At the end of `commit`, if the cache is enabled and `BeginCommit(originalRoot)` accepts:

1. `InvalidateAccount` for every destroyed account
2. `Put` every `originStorage` and `pendingStorage` slot
3. `EndCommit(newRoot)`

`EnablePathStats` / `PathStats` exist only for the experiment harness.

**`core/state/state_object.go`** — in `GetCommittedState`, after origin miss and after the destructed-account empty-slot path, try `storageCache.Get(originalRoot, address, key)`. On hit, store into `originStorage` so later reads in the same block stay in the map. Optional `pathStats` increments on each path.

**`core/vm/interpreter.go`** — `Config.EnableKeccakCache`, `Config.KeccakCache`.

**`core/vm/evm.go`** — if the flag is on and the pointer is nil, allocate a default table (so tests can set the flag without constructing a cache).

**`core/vm/instructions.go`** — `opKeccak256`: Get before hashing; Put after. Preimage recording still happens on both the hit and miss paths.

**`core/blockchain.go`** — `CacheConfig` grows four fields. `attachEVMCaches` runs at construction and again after snapshot re-init (the storage cache object is preserved across the statedb swap). `purgeStorageCache` runs on `reorg` and `setHeadBeyondRoot`.

**`eth/ethconfig`**, **`eth/backend.go`**, **`cmd/utils/flags.go`**, **`cmd/geth/{main,chaincmd}.go`** — plumb the four flags into `ethconfig.Config` and `core.CacheConfig`. Toml marshal/unmarshal regenerated in `gen_config.go`.

### 3.3 Tests

| File | What it covers |
|---|---|
| `core/evmcache/cache_test.go` | cache internals |
| `core/state/storage_cache_test.go` | StateDB integration, including SSTORE same-value no-op |
| `core/vm/keccak256_cache_test.go` | opcode hit/miss, inputs over 64 bytes not cached |
| `core/blockchain_cache_test.go` | reorg purges the storage cache |

### 3.4 Experiment harness — `core/evmcache/experiment`

Not on the production path. Go tests that print markdown tables.

| Cell | File | Workload |
|---|---|---|
| S-A | `experiment_test.go` | cost per layer (map vs cache vs snapshot fastcache); commit rebuild cost |
| S-B | same | where ERC-20 SLOADs resolve |
| S-D | same | hit-rate sweep, N = 8/64/128/256; worst case (256 unique vs 8-entry budget) |
| S-D3 | same | 5 synthetic blocks, per-block ns/transfer and hit rate |
| S-E | same | SSTORE same value twice (gas +106, cache unchanged) |
| K-A / K-B / K-D | same | keccak micro, ERC-20 census, hit-rate sweep |
| M | `mainnet_blocks_test.go` | 2–3 consecutive mainnet blocks from `block_data/<n>/<tx>/txTest.json`, four configs: both off, storage only, keccak only, both on |

`token.go` is pinned ERC-20 bytecode for the synthetic cells (no solc at test time).

Timing discipline: interleaved on/off, alternating arm order, `runtime.GC()` before each measured run, 20 repeats for synthetic cells / 101 for the mainnet cell. A duplicate “both off” row is the noise floor.

---

## 4. Ancillary tree changes (not cache logic)

These show up in `git diff --stat` but are not part of the cache design:

| Path | What happened |
|---|---|
| `evm` | 42 MB debug ELF checked in. Build artifact; should not be in git. |
| `block_data` | gitlink / capture directory used by the mainnet replay. Live captures are incomplete (e.g. 30/150, 53/103, 128/197 fixtures for 22000001–3). |
| `tests/testdata`, `tests/evm-benchmarks` | submodule gitlinks present at `v1.14.13`, absent at HEAD. `.gitmodules` still lists them. |

---

## 5. What the current design already shows

From `EVM_CACHE_EXPERIMENT_RESULTS.md` (one binary, flags on vs off):

- **Correctness.** Gas and post-state root matched in every differential cell. Reorg, revert, staged-write, self-destruct, destroy-and-recreate, fold, over-budget eviction, and disabled-cache tests pass.
- **Synthetic ERC-20.** Storage cache ~+10% across five blocks when the same 64 addresses transfer every block (100% reuse). Keccak cache helps above ~80% hit rate and turns slightly negative below that.
- **Real consecutive blocks (22000001–3, 211 txs).** Cross-block slot reuse is 2.6% then 12.0%. `originStorage` already absorbs 73.7% of committed-state reads. The storage cache sees 1,116 of 4,248 reads and answers 56 (5.0%). Against a snapshot-backed node that is ~2 µs saved vs ~465 µs of commit staging — a net loss. The keccak cache hit 54.1% (820/1,517) with no commit-side cost, ~1.4% of a 20 ms run.

So the implementation is a correct, default-off overlay. The remaining design question is not “does the cache work on a reused ERC-20 set” (it does) but “can the worst case — almost no reuse — be made free enough that opportunistic speedup is safe to leave enabled.” That work is **not in this commit**.

---

## 6. How to re-run

```
# synthetic cells
go test ./core/evmcache/experiment -run TestEVMCacheExperiment -timeout 30m -v

# mainnet replay (optional frozen capture)
go test ./core/evmcache/experiment -run TestMainnetBlocksCacheImpact -timeout 40m -v
EVMCACHE_BLOCK_DATA=/path/to/frozen/copy go test ./core/evmcache/experiment -run TestMainnetBlocksCacheImpact -timeout 40m -v

# unit tests (no race detector required)
go test ./core/evmcache ./core/state ./core/vm -count=1
```
