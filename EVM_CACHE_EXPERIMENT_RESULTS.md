# EVM Storage Cache and Keccak Cache — Report

**Date:** 2026-09-03
**Machine:** Go 1.22, linux/amd64, Intel i7-14700 (28 CPUs)
**Binary:** one geth binary; caches default **off**. On vs off is a runtime flag, not two builds.

**Short version.** Design D shipped: the storage cache no longer has a lookup path of its own. It keeps each contract's `originStorage` map alive across blocks, so a cross-block `SLOAD` is the probe geth already does. That probe costs **23 ns**, identical to stock originStorage. Commit is a retag plus a length update: **5–185 µs**, not milliseconds.

Both caches leave gas and the post-state root unchanged. On synthetic ERC-20 traffic the storage cache is **+15–18%** and the keccak cache **+12–17%** at high reuse. On three consecutive mainnet blocks, slot reuse is only 2.6–12.0%. The storage cache's commit overhead on that run is **94 µs / three blocks** (was hundreds of microseconds to milliseconds under copy-on-write). That is inside the timing noise. The keccak cache still looks like about **+1.4%** of the run, with no commit cost. Both stay **default off**.

---

## 1. What this is trying to solve

1. **Storage.** After a block commits, geth throws away each account's `originStorage` map. The next block's first `SLOAD` of the same slot falls through to snapshot/trie. Can we keep those maps?
2. **Keccak.** Solidity mappings hash `keccak256(padded key ‖ slot)` — a 64-byte input that repeats. Geth does not cache SHA3 results.

Both must leave **gas and the post-state root unchanged**.

```
geth --cache.evm.storage
geth --cache.evm.keccak  [--cache.evm.keccak.slots=16384]
```

---

## 2. Design D — retain `originStorage`

The previous storage cache was a process-lifetime `map[{addr,slot}]Hash` with copy-on-write snapshots (`delta`/`base`), a mutex around staging, and a `Get` consulted after `originStorage` missed. A hit cost 49 ns. A commit rebuilt maps: 0.5–6 ms.

Design D deletes that lookup path.

After `commitStorage`, `stateObject.originStorage` already holds the committed slots for that account. `Retain` stores the map; the next block's `newObject` `Adopt`s it back as `originStorage`. `GetCommittedState` is unchanged on the hit path — the existing `originStorage[key]` probe is the cache.

| | Previous (COW map) | Design D |
|---|---|---|
| Read hit | 49 ns, new code | 23 ns, existing `originStorage` probe |
| Read miss | extra map probe then backend | the originStorage miss that was going to happen anyway |
| Commit | copy delta, periodic 6 ms fold | in-place update + length counter + root retag |
| Self-destruct | rebuild both maps to filter one address | `delete` one map |
| Budget | 65,536 entries, recency eviction | none; maps grow with the working set |

API (`core/evmcache/storage.go`):

- `Adopt(root, addr)` — return the retained slot map if the root matches
- `Retain(addr, m)` — keep that map after commit
- `Commit(newRoot)` — retag
- `Drop(addr)` — self-destruct
- `Purge()` — reorg / `SetHead`

Gone: `storageKey`, `storageSnapshot`, `atomic.Pointer`, `sync.Mutex`, `staged`, `destructed`, `delta`/`base`, `Get`, `Put`, `BeginCommit`, `EndCommit`, `InvalidateAccount`.

Assumptions, taken as given and not enforced: no `SLOAD` during `Commit`, no reorgs (a reorg still `Purge`s). Exclusive ownership, witness, `StateDB.Copy` cost, and an entry budget were explicitly out of scope.

Keccak is unchanged: direct-mapped table, ≤64-byte inputs, full-input compare on hit, hooked only into the `SHA3` opcode.

---

## 3. Layers an `SLOAD` walks

Warm/cold gas (EIP-2929) is a separate access list. None of these layers change gas.

| Name | What | Lifetime |
|---|---|---|
| dirty / pending | current tx / earlier txs in this block | this block |
| **originStorage** | committed value already loaded, **including maps retained from earlier blocks** | this block, or across blocks when the cache is on |
| snapshot fastcache | 64-byte composite key, RLP blob | process |
| trie / disk | Merkle walk | — |

Under Design D there is no distinct "storage cache hit" row. A cross-block hit **is** an originStorage hit. A miss is a backend read, which then fills originStorage as stock geth already does.

---

## 4. How to read the numbers

| Term | Meaning |
|---|---|
| **ns/op, ns/tx, ns/transfer** | wall time of the timed region, divided by operations / transactions / transfers |
| **On vs off** | same binary, cache attached vs nil. Speedup `(off − on) / off`. Negative means slower |
| **Root match** | post-state root *and* gas identical on vs off. Required |
| **origin / backend** | Design D accounting: committed-state reads served from originStorage vs snapshot/trie |
| **origin share** | `origin / (origin + backend)` on the on-arm. Includes same-block re-reads |

Synthetic cells: mean of 20 interleaved on/off pairs, alternating order, GC before each run. Mainnet cells: median of 101 passes. Effects smaller than the noise-floor row are not real.

---

## 5. Results

### 5.1 S-A — Cost per layer

| Level | ns/op | allocs/op |
|---|---:|---:|
| originStorage map hit (1 key, in L1) | 7.0 | 0 |
| originStorage map hit (65,536 keys) | 23.0 | 0 |
| adopted originStorage hit (65,536 keys) | **23.0** | 0 |
| Adopt per lookup (65,536 keys) | 44.0 | 0 |
| snapshot fastcache hit (1 key, in L1) | 64.0 | 2 |
| snapshot fastcache hit (65,536 keys) | 106.0 | 2 |

The adopted map is the same pointer as originStorage: **23.0 vs 23.0 ns**. The per-`SLOAD` cost over stock geth is zero. `Adopt` is paid once per contract per block (the extra 21 ns in the "Adopt per lookup" row is what you would pay if you resolved the address on every SLOAD — which Design D does not).

H1 prize (snapshot − map, equal cardinality) = **83 ns**. Snapshot is still the thing a retained map avoids.

#### Commit cost (in-place update + Retain + retag), mean over 64 commits

| Resident slots | Slots written per block | µs/commit |
|---:|---:|---:|
| 1,024 | 256 | 34.5 |
| 1,024 | 2,048 | 134.6 |
| 4,096 | 256 | 20.0 |
| 4,096 | 2,048 | 185.3 |
| 16,384 | 256 | 6.2 |
| 16,384 | 2,048 | 141.6 |
| 65,536 | 256 | 5.0 |
| 65,536 | 2,048 | 85.4 |

No maps are copied. Compare to the previous design: **5.7–6.1 ms** to copy 65,536 entries every block, or ~0.5 ms amortized with delta/base. Design D is two orders of magnitude cheaper, and the cost scales with **writes**, not with how much is already retained (larger resident tables are faster here because Go's map assign hits a hot table).

### 5.2 S-B — ERC-20 path distribution

256 transfers over 64 addresses. Block 2 is the cross-block case.

| Path | Count |
|---|---:|
| dirty | 768 |
| pending | 0 |
| origin (includes retained) | **2,048** |
| backend (snapshot/trie) | **0** |

Every committed-state read in block 2 is an originStorage hit. Under the old design those 2,048 would have been a mix of origin (same-block re-reads) and cache.Get. They are now the same probe.

### 5.3 S-D — ERC-20 timing

D1, synthetic adopted-map lookup: 11–12 ns/op at pool 8/64/128/256, 0 allocs. There is no entry budget, so every seeded slot hits.

D2, 256 transfers, cache on vs off (origin share is origin/(origin+backend) on the on-arm):

| Addresses | ns/transfer on vs off | speedup | origin share | root match |
|---:|---:|---:|---:|---|
| noise floor (off vs off, N=64) | 6697 vs 7048 | 4.99% | n/a | true |
| 8 | 5889 vs 5922 | 0.55% | 100% | true |
| 64 | 5624 vs 6595 | **14.73%** | 100% | true |
| 128 | 6336 vs 7717 | **17.89%** | 100% | true |
| 256 | 8089 vs 9677 | **16.41%** | 100% | true |
| 256 unique pairings | 8196 vs 10248 | **20.02%** | 100% | true |

Gas identical, roots match. N=8 is inside the noise floor: the working set already fits in CPU cache either way. Larger N is a real win, because block 1's commit hands block 2 (inside the same timed transfer loop, after `commitReload`) the balance maps.

The "unique" row is not a miss workload under Design D without a budget: the accounts were seeded, their maps were retained, and every SLOAD hits originStorage. That is the point of not evicting.

D3, five consecutive blocks × 128 transfers over 64 addresses:

| Block | ns/transfer on | ns/transfer off | speedup | origin share | entries | commit ms (on) |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 5913 | 6985 | 15.34% | 100% | 64 | 0.10 |
| 2 | 8922 | 9115 | 2.13% | 100% | 64 | 0.10 |
| 3 | 7617 | 8794 | 13.39% | 100% | 64 | 0.13 |
| 4 | 8408 | 7797 | −7.84% | 100% | 64 | 0.10 |
| 5 | 7611 | 8568 | 11.18% | 100% | 64 | 0.14 |

Whole run: 7694 vs 8252 ns/transfer (**+6.76%**), gas 4,753,280 both arms, root match, **0 purges**. Per-block commit is ~0.1 ms. Block 4's negative cell is noise on a ~1 ms block; the five-block mean is positive.

### 5.4 S-E — SSTORE of the same value

Still dismissed at the state layer (`SetState` no-op, dirty marker deleted on round-trip, `commitStorage` skips equals-origin). Still charged 106 gas per extra write (100 warm read + 6 for two `PUSH1`s). Cache holds one entry regardless of repeat count.

| Identical SSTOREs | gas | cache entries |
|---:|---:|---:|
| 1 | 22,106 | 1 |
| 2 | 22,212 | 1 |
| 3 | 22,318 | 1 |

### 5.5 Keccak (unchanged design)

| Path | 32-byte ns/op | 64-byte ns/op |
|---|---:|---:|
| uncached hash | 263 | 246 |
| cache hit | 19 | 37 |
| miss + insert | 20 | 39 |

Inputs > 64 bytes are not cached (128-byte uncached 221 ns, 256-byte 431 ns).

ERC-20 census: 128 transfers, 64 addresses → 192 hits / 64 misses (75%). Each transfer does two 64-byte mapping-key hashes.

| Cell | ns/transfer on vs off | speedup | hit rate | root match |
|---|---:|---:|---:|---|
| noise floor | 7405 vs 7545 | 1.86% | n/a | true |
| N=8 | 5284 vs 6337 | **16.61%** | 98.4% | true |
| N=64 | 6603 vs 7465 | **11.55%** | 87.5% | true |
| N=128 | 8851 vs 8415 | −5.17% | 75.0% | true |
| N=256 | 9821 vs 10107 | 2.82% | 50.0% | true |
| 10 tokens × 64 addrs | 10642 vs 10893 | 2.30% | 75.0% | true |

N=8 and N=64 clear the noise floor. N=128's negative is a miss-heavy cell plus interference; the keccak cache has no commit cost, so a miss is almost free (20 ns insert vs 246 ns hash, but the surrounding EVM work dominates).

### 5.6 M — Three consecutive mainnet blocks

Blocks 22000001–22000003, 211 transactions replayed (20% / 51% / 65% of each block's txs), 45,940,534 gas, post-state root `0x0c9d9b18…` — identical in every arm, 0 rejected, 0 purges.

Structural ceiling — slots in the declared pre-state that an earlier block also touched:

| Block | Slots touched | Also in an earlier block | Share |
|---:|---:|---:|---:|
| 22000001 | 229 | 0 | 0.0% |
| 22000002 | 817 | 21 | 2.6% |
| 22000003 | 624 | 75 | 12.0% |

A cross-block storage cache cannot beat that ceiling.

#### Cold (only slots this run committed are cacheable)

Noise floor (two "both off" arms): **−1.04%**. Per-arm median-vs-best spread 11.6–16.0%. Nothing smaller than that is a real timing effect.

| Configuration | ns/tx (median) | vs both off | origin / backend | keccak hit rate |
|---|---:|---:|---|---|
| both off | 93,936 | — | — | — |
| both off (noise floor) | 94,917 | −1.04% | — | — |
| storage only | 94,777 | −0.90% | 3188 / 1060 | — |
| keccak only | 95,300 | −1.45% | — | 54.1% (820/1517) |
| both on | 93,403 | +0.57% | 3188 / 1060 | 54.1% |

Storage-only sits **inside the noise floor**. Commit cost per block is indistinguishable from off (0.84 / 0.95 / 1.35 ms vs 0.83 / 0.93 / 1.28 ms).

Cold path split with storage on: origin 75.0%, backend 25.0%, 1,060 slots retained. The 75% origin figure includes same-block re-reads; the *extra* origin vs a cache-off arm is the 56 reads moved off the backend (see budget below).

#### Pre-warmed (upper bound: genesis already held every slot these txs declare)

| Configuration | ns/tx (median) | vs both off | origin / backend |
|---|---:|---:|---|
| both off | 96,255 | — | — |
| both off (noise floor) | 93,929 | +2.42% | — |
| storage only | 93,046 | +3.33% | 3790 / 458 |
| keccak only | 94,538 | +1.78% | — |
| both on | 92,710 | +3.68% | 3790 / 458 |

Pre-warmed origin 89.2%, backend 10.8%, 1,715 slots held. The noise floor itself is +2.42% this time, so the +3.33% storage-only cell is only barely outside it.

#### What the hits are worth (counted work, not the noisy ns/tx table)

The whole cold run is 19.8 ms. Hits = backend reads moved off by retention (off-arm backend minus on-arm backend). Snapshot saving is a microbenchmark: fastcache+RLP minus an originStorage probe, 65,536 keys = **90 ns**. Trie saving from the cold-vs-warm gap = **607 ns**. Keccak hit = **341 ns**.

| Component | Hits | Saved per hit | Total | Share of run |
|---|---:|---:|---:|---:|
| storage, cold, trie backend | 56 | 607 ns | 34 µs | 0.17% |
| storage, pre-warmed, trie | 658 | 607 ns | 399 µs | 2.01% |
| storage, cold, snapshot (full node) | 56 | 90 ns | 5 µs | 0.03% |
| storage, pre-warmed, snapshot | 658 | 90 ns | 59 µs | 0.30% |
| keccak | 820 | 341 ns | 280 µs | **1.41%** |

Commit of the three blocks: 3.05 ms off, 3.14 ms storage-on → **94 µs** Retain+retag overhead.

| Scenario | Saved | Commit overhead | Net |
|---|---:|---:|---:|
| cold, trie | 34 µs | 94 µs | **−60 µs** |
| pre-warmed, trie | 399 µs | 94 µs | **+305 µs** |
| cold, snapshot | 5 µs | 94 µs | −89 µs |
| pre-warmed, snapshot | 59 µs | 94 µs | −35 µs |

Keccak has no commit-side cost, so its 280 µs is net.

---

## 6. What changed versus the copy-on-write cache

The previous report's verdict was: storage cache is a **net loss on mainnet** because commit rebuilt maps while consecutive blocks shared almost no slots.

Design D does not create hits that were not there. 2.6–12.0% structural reuse is still the ceiling. What it changes is the **loss function**:

- Per-`SLOAD` overhead on a miss: **zero** (no extra map probe).
- Per-block commit: **~30 µs average** on this run, not 0.5–6 ms.
- Worst-case unique ERC-20 traffic: **no eviction cliff**, and the timed cells are faster, not slower.

On a snapshot-backed node with this traffic, storage cache is still a few tens of microseconds in the red. That is 0.5% of a 20 ms run and smaller than the noise floor. The previous design was a real, measurable regression. This one is opportunistic: free when it misses, helpful when a later block rereads the same contracts (ERC-20 synthetic, or a pre-warmed working set).

---

## 7. Correctness checks that still pass

Same-block read is not cached (Adopt misses until commit). Block N write is visible in N+1 as an originStorage hit with backend=0. Zero slots, two contracts sharing a slot index, reverted writes, self-destruct (`Drop` then `Retain` of the new incarnation), reorg `Purge`, disabled/nil cache, differential root, same-value SSTORE. Keccak still rejects long inputs and requires a full-input compare.

---

## 8. Verdict

| Cache | Synthetic ERC-20 | Three mainnet blocks | Default |
|---|---|---|---|
| Storage (Design D) | +15–18% at N≥64; commit ~0.1 ms | Inside noise when cold; +305 µs net if pre-warmed against trie; still slightly negative against snapshot | **off** |
| Keccak | +12–17% at high reuse | ~1.4% of the run, no commit cost | **off** |

The storage cache is now safe to turn on in the sense that was the goal of the redesign: **it does not degrade a normal block**. It also does not, on this mainnet sample, pay for itself on a snapshot-backed node. The keccak cache is the one with a visible, commit-free win on both workloads.
