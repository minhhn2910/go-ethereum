# EVM Storage Cache and Keccak Cache — Report

**Date:** 2026-08-27
**Machine:** Go 1.22.2, linux/amd64, 28 CPUs
**Binary:** one geth binary; caches default **off**. On vs off is a runtime flag, not two builds.

This report explains the two optional caches added to go-ethereum v1.14.13, how they sit on top of caches that already exist, what was measured, and what the numbers mean. You do not need any other document to read it.

**Short version.** Both caches are correct — gas and the post-state root are unchanged in every differential test. On synthetic ERC-20 workloads both look worthwhile. On three consecutive real mainnet blocks (§9.7) only the Keccak cache survives, at about **+1.4%**; the storage cache is a **net loss**, because consecutive blocks share only 2.6–12.0% of their touched slots and the per-block map already absorbs 73.7% of committed-state reads before the cache is consulted. Both stay **default off**.

---

## 1. What problem is this trying to solve?

When the EVM runs `SLOAD` (read a contract storage slot) or `SHA3`/`KECCAK256` (hash some bytes), those operations are already cached in several places. This work asks a narrower question:

1. **Storage.** After a block is committed, geth throws away the cheap per-contract maps. The next block's first read of the same slot falls through to the snapshot/trie. Can a small, long-lived `(contract, slot) → value` table skip that?
2. **Keccak.** Solidity mappings hash `keccak256(padded key ‖ slot)` — a 64-byte input that repeats across transfers, blocks, and even different ERC-20s with the same layout. Geth does not cache SHA3 *results*. Is a tiny table for those 64-byte hashes worth it?

Both caches must leave **gas and the post-state root unchanged**. They are performance only.

Enable them:

```
geth --cache.evm.storage [--cache.evm.storage.slots=65536]
geth --cache.evm.keccak  [--cache.evm.keccak.slots=16384]
```

Disable: omit the flag, or call `SetEnabled(false)` on a live cache object.

---

## 2. Design

Both live in `core/evmcache`. They are nil unless attached, so the default execution path is stock geth.

### 2.1 Storage cache

A map from `(address, slot)` to the **committed** value at a tagged **state root**.

| Rule | Why |
|---|---|
| Read after the per-contract maps, before snapshot/trie | Same data `GetCommittedState` already wants |
| Write only at block commit, never mid-transaction | A reverted CALL cannot poison the table |
| Ignore a lookup if the tagged root does not match | Never serve another block's state |
| Purge everything on reorg / `SetHead` | No fine-grained fork handling |
| Self-destruct drops that account's slots at the next commit | A destroyed account must not keep serving old storage |

Default budget: 65,536 entries.

**Structure.** Block commit is the only writer, and it runs once per block. That makes *copy-on-write* affordable: writes stage into a side map, and commit publishes a rebuilt map with a single atomic pointer store. Readers therefore take **no lock at all** — a lookup is one atomic pointer load, one root comparison, and a map lookup. Because published maps are never mutated, there is no race for readers to guard against.

The published contents are split into a **delta** and a **base**:

- the **delta** holds slots written by the last few blocks and is rebuilt on every commit;
- the **base** holds everything else and is only rebuilt when the delta outgrows an eighth of the budget, after 32 idle commits, or when an account self-destructs.

The split exists purely for commit cost. Copying the whole map every block costs 6.3 ms at the default size, which is thousands of times more than the lookups save (§7.1). Splitting brings that to 0.5 ms. A lookup checks the delta first, then the base, so it is at most two map lookups.

### 2.2 Keccak cache

Hooked into the **`SHA3` opcode only** (not trie hashing, where inputs are nearly unique).

- Caches inputs of **at most 64 bytes**, which is what a Solidity mapping key is
- Direct-mapped; overwrite on collision
- On a possible hit, **compare the full input bytes** before returning. Trusting only the table index would be a silent wrong hash, and so a wrong storage key and a wrong state root

Default size: 16,384 entries (~1.5 MB). Entries never go stale, since keccak is a pure function.

A 32-byte-only variant was considered and rejected: a hit on 32 bytes is about twice as cheap as on 64, but Solidity mapping keys *are* 64 bytes, so a 32-byte cap would take the ERC-20 hit rate to zero.

---

## 3. "Map hit" vs "cache hit" vs "snapshot hit"

geth already has several storage layers. An `SLOAD` walks them top-down. **Warm/cold gas (EIP-2929) is a separate access list** that resets every transaction; none of these layers change gas.

| Name in the numbers | What it is | Lifetime |
|---|---|---|
| **Map hit** (`originStorage` / `dirtyStorage` / `pendingStorage`) | Ordinary Go `map` on the in-memory account object | Dies when the block is committed |
| **Storage cache hit** (this work) | Process-lifetime `(address, slot)` table | Survives block boundaries; purged on reorg |
| **Snapshot hit** | Global fastcache of flat state. Builds a 64-byte composite key, looks up a blob, RLP-decodes it | Survives blocks |
| **Backend miss** | Snapshot miss, so walk the Merkle storage trie and maybe disk | — |

So:

- **Map hit** is the floor: the slot was already loaded on this account *in this block*.
- **Storage cache hit** is what was added: the slot was loaded in a *previous* block, the account object was thrown away, and the committed value is still on hand.
- **Snapshot hit** is today's cross-block path, and it is what the new cache actually competes with.

---

## 4. Why is a cache hit slower than a map hit?

The first version of this cache took 63 ns per hit against 7 ns for a plain Go map. Four things accounted for the gap, and three of them were avoidable:

| Cost | ns | Fixed by |
|---|---:|---|
| `sync.RWMutex` read lock and unlock | ~20 | Copy-on-write: readers take no lock |
| FNV-1a hash, byte at a time over 52 bytes | ~20 | A comparable struct key, so the runtime hasher (AES-accelerated) does it |
| A *second* map lookup for the per-account generation, inside the lock | ~15 | Self-destructs are handled at commit instead, so there is no generation to check |
| `atomic.AddUint64` on a hit counter | ~7 | Hit/miss accounting is off unless requested |

That took the hit from 63 ns to 49 ns at 65,536 entries, and to 17 ns on a single hot key.

The remaining gap is not a defect. The original 7 ns figure was measured on a **one-entry map** sitting in L1; the same map holding 65,536 keys costs 23 ns. Against that fair baseline the cache costs 49 ns. The residual 26 ns is inherent: the key is 52 bytes (address plus slot) rather than 32, so there is more to hash and compare, and the lookup adds an atomic pointer load and a root comparison.

The comparison that decides whether the cache is worth anything is not against the map — a map hit means the slot was already loaded this block, in which case the cache is never consulted. It is against the **snapshot**, which at equal cardinality costs 94 ns. That is the number the cache has to beat, and 49 ns beats it.

---

## 5. Does the map design work across blocks?

Yes, and that is what shipped. The cache is now a Go map, not a hand-rolled table. Three things had to be solved to make a map safe and affordable across block boundaries:

1. **Concurrent readers.** Solved by never mutating a published map. Commit builds new maps and swaps a pointer.
2. **Knowing which block the contents belong to.** The state root is a field on the published snapshot, so checking it is a plain comparison with no bookkeeping.
3. **Commit cost.** Rebuilding a 65,536-entry map every block costs 6.3 ms. Solved by the delta/base split in §2.1, which only rebuilds the big map occasionally.

Eviction also improved as a side effect. The old table was direct-mapped, so an index collision threw out a live entry even when the table was nearly empty. The map only evicts when it is actually over budget, and it evicts by inserting the current block's slots first, so what survives is what was touched most recently.

---

## 6. SSTORE: is writing the same value twice dismissed?

**At the state layer, completely. On gas, no.**

Three separate places drop it:

- `stateObject.SetState` returns early when the new value equals the current one, so nothing is journalled and no dirty entry is created
- `stateObject.setState` *deletes* the dirty marker when a slot is written back to its original value, so a round trip also collapses
- `stateObject.commitStorage` skips slots whose value equals `originStorage`, so a no-op never reaches the trie

Gas is charged anyway. Under EIP-2929, `current == value` costs `WARM_STORAGE_READ_COST` (100), plus 2100 if the slot was not yet in the access list.

Measured with a contract that writes `42` to slot 1 one, two and three times:

| Identical SSTOREs | Gas | Committed slot | Cache entries |
|---:|---:|---|---:|
| 1 | 22,106 | `0x…2a` | 1 |
| 2 | 22,212 | `0x…2a` | 1 |
| 3 | 22,318 | `0x…2a` | 1 |

Each repeat costs exactly 106 gas: 100 for the warm storage read plus 6 for the two `PUSH1`s that set it up. It does not pay `SSTORE_SET` (20,000) again. The committed slot never changes, and the cache holds one entry no matter how many times the slot is rewritten, so a no-op write cannot make the cache disagree with committed state.

---

## 7. How to read the experiment columns

| Term | Meaning |
|---|---|
| **ns/op** | Nanoseconds per operation (one map lookup, one cache `Get`, one hash, or one ERC-20 `transfer`) |
| **allocs/op** | Heap allocations per operation. 0 is ideal |
| **N** | Number of distinct addresses in the ERC-20 workload. Every sweep uses N = 8, 64, 128, 256. Small N means the same accounts transfer repeatedly, so reuse and hit rate are high |
| **Key pool** | How many distinct keys a synthetic loop draws from. A pool larger than the budget cannot fit, which forces misses |
| **Hit rate** | `hits / (hits + misses)`, **measured**, never inferred from N or pool |
| **On vs off** | Same binary, cache attached vs nil. Speedup is `(off − on) / off`. Negative means the cache was slower |
| **Root match** | Post-state Merkle root *and* gas identical with the cache on and off. Required; a mismatch fails the test |

### 7.0 About the noise floor

Every EVM-level cell is the mean of 20 interleaved on/off pairs, with the arm order swapped on alternate repeats and a garbage collection before each run. That is not ceremony — it is the difference between usable and useless numbers:

| Method | Spread between two *identical* configurations |
|---|---:|
| All of one arm, then all of the other | 16.6% |
| Interleaved, off always first | 13.0% |
| Interleaved, alternating order | 3.8% |
| Interleaved, alternating order, GC before each run | **0.9%** |

The 13% case is the instructive one: whichever arm ran first in a pair absorbed the garbage collection that the previous run had set up. With the final method the same-configuration spread is **0.9%**, so effects above roughly 2% are real. Each results table below carries its own noise-floor row.

This holds for the synthetic cells, where a measured run is a tight loop over one contract. It does not carry over to the mainnet replay in §9.7: a pass there is 20 ms of varied real bytecode across 211 transactions, and even at 101 repeats two identical configurations came out 3.8% apart. That cell reports what the caches save by counting avoided work rather than by timing the run.

---

## 8. What the tests do

Two groups: **correctness** (must pass) and **timing** (§9).

### Correctness

| Test | What it checks |
|---|---|
| Write then read in the same block | The cache is *not* written mid-transaction; in-block maps serve the value |
| Write in block N, read in N+1 | The cross-block table's actual job |
| Slot cleared to zero | Zero is a real value, not "missing" |
| Two contracts, same slot index | Address is part of the key |
| Reverted internal write | Commit-only population, so a revert cannot poison |
| Staged slot before commit | A staged write is invisible until the commit publishes it |
| Self-destruct, and self-destruct after a fold | Dead slots are dropped whether they sit in the delta or the base |
| Destroy and recreate in one block | The new incarnation's slots win over the invalidation |
| 40 blocks of writes | Entries stay readable across repeated base rebuilds |
| Over-budget eviction | The current block's slots are the ones retained |
| Reorg | Everything is purged; no stale hits |
| Cache disabled, and nil cache | Behaviour identical to stock geth |
| Differential root | Same sequence with the cache on vs off gives the same root and gas |
| Same-value SSTORE, and write-back-to-origin | No dirty marker, unchanged root, unchanged cache |
| Stats gating | Hit/miss counters stay at zero until enabled; inserts always count |
| SHA3 hit vs miss | Same hash with the cache on or off; inputs over 64 bytes are not stored |
| `-race` | Concurrent `Get` while a writer republishes snapshots |

### Timing workloads

All ERC-20 cells use a pinned bytecode fragment, not solc at test time: `balances[msg.sender] -= amount; balances[to] += amount`. That is the same SHA3 plus SLOAD/SSTORE pattern as a Solidity mapping.

- **Layer costs (S-A):** one lookup at the map, at the cache, and at the snapshot, in isolation, at one key and at 65,536 keys.
- **Commit cost (S-A):** the price copy-on-write pays on the write path, across budgets.
- **Hit distribution (S-B):** 256 transfers, then a second block. Count where each SLOAD was satisfied.
- **Synthetic pool sweep (S-D1):** only `cache.Get`, varying how many keys are cycled through.
- **ERC-20 sweep (S-D2):** vary N. Plus a **worst case**: 256 unique transfers against an 8-entry budget, so almost every lookup misses and the cache is pure overhead.
- **Cross-block (S-D3):** 5 blocks × 128 transfers over 64 addresses, reported per block.
- **Same-value SSTORE (S-E):** §6.
- **Keccak micro (K-A):** hash 32/64/128/256-byte buffers vs a table hit. Inputs over 64 bytes are not cached.
- **Keccak census and sweep (K-B, K-D):** how often SHA3 repeats in ERC-20 transfers, including ten identical-layout tokens sharing one address set.
- **Mainnet replay (M):** three consecutive real blocks, all four cache configurations, in `TestMainnetBlocksCacheImpact` rather than the main harness. This is the only cell not built from a synthetic contract, and §9.7 is where the synthetic conclusions get checked against real traffic.

---

## 9. Results

### 9.1 Cost of one storage lookup (no EVM)

| Path | ns/op | allocs | bytes |
|---|---:|---:|---:|
| `originStorage` map hit, 1 key in L1 | 8 | 0 | 0 |
| `originStorage` map hit, 65,536 keys | 23 | 0 | 0 |
| **Storage cache hit, 1 key in L1** | **17** | 0 | 0 |
| **Storage cache hit, 65,536 keys** | **49** | 0 | 0 |
| Storage cache hit, 65,536 keys, stats on | 52 | 0 | 0 |
| Snapshot fastcache hit, 1 key in L1 | 53 | 2 | 112 |
| Snapshot fastcache hit, 65,536 keys | 94 | 2 | 112 |

Read this at matching cardinality. At 65,536 keys the ordering is map 23 < cache 49 < snapshot 94, so the cache is roughly **half the cost of the snapshot path it replaces** and allocates nothing where the snapshot allocates 112 bytes. Comparing a one-entry map (8 ns) against a full cache (49 ns) is the mistake the earlier version of this report made.

Hit/miss accounting costs 3 ns of the 49, which is why it is off by default.

**Synthetic `cache.Get` vs pool size (budget 128 entries):** 23–27 ns whether the hit rate is 100% or 50%. A miss is the same pointer load and map probe, then "not found". There is no time-versus-hit-rate curve *inside* the cache; hit rate only matters because a miss falls through to snapshot or trie.

### 9.2 Commit cost of copy-on-write

Mean over 64 commits, so the occasional base rebuild is amortized.

| Budget (entries) | Slots staged per block | ms/commit |
|---:|---:|---:|
| 1,024 | 256 | 0.14 |
| 1,024 | 2,048 | 0.43 |
| 4,096 | 256 | 0.14 |
| 4,096 | 2,048 | 0.66 |
| 16,384 | 256 | 0.21 |
| 16,384 | 2,048 | 0.93 |
| 65,536 | 256 | 0.54 |
| 65,536 | 2,048 | 1.96 |

This is the real cost of the design and the reason the delta/base split exists. A naive full copy at 65,536 entries measured **6.3 ms per commit** regardless of how little the block wrote; the split brings the same case to 0.54 ms. It remains true that a busy block (2,048 slots) at the default budget pays about 2 ms on the commit path, which is the largest single argument against turning this on by default.

### 9.3 Where ERC-20 SLOADs actually go (2nd block, 64 addresses, 256 transfers)

| Resolved at | Count | Meaning |
|---|---:|---|
| dirty (current tx writes) | 768 | Reading a slot this tx already wrote |
| origin (loaded earlier **this block**) | 1,984 | Cheap map; already the common case |
| **new storage cache** | **64** | First touch of that slot in this block — the only reads the new table can intercept |
| snapshot / trie | 0 | Nothing fell through in this in-memory run |
| pending (earlier tx in the block) | 0 | Not used on this path |

The cache hit 64 of 64 of those first-in-block reads. Gas for both blocks was identical (1,901,312). Most SLOADs never reach the new table, because `originStorage` already has them after the first access — which caps how much this cache can ever be worth.

### 9.4 ERC-20 timing, storage cache on vs off

Mean of 20 interleaved pairs. **N** is the number of distinct addresses.

| Workload | Hit rate | ns/transfer on vs off | Speedup | Root / gas |
|---|---:|---|---:|---|
| noise floor (off vs off, N=64) | n/a | 6149 vs 6207 | +0.9% | match |
| N = 8 | 100% | 5188 vs 5535 | +6.3% | match |
| N = 64 | 100% | 5415 vs 5351 | −1.2% | match |
| N = 128 | 100% | 6426 vs 8362 | +23.2% | match |
| N = 256 | 100% | 8455 vs 10347 | +18.3% | match |
| 256 unique, 8-entry budget (worst) | 3.1% | 9695 vs 10488 | +7.6% | match |

The N = 128 and N = 256 cells are the meaningful ones, at roughly 20 times the noise floor. The mechanism is straightforward: with more distinct addresses, a larger share of reads is the first touch of that slot in the block, and those are exactly the reads the cache intercepts. N = 8 and N = 64 are small enough that `originStorage` absorbs nearly everything, so there is little left to win and the two cells land inside noise of each other.

The worst case did not regress. With a deliberately tiny budget and no reuse at all it came out slightly ahead, which is within the range where the honest statement is "no measurable penalty".

### 9.5 Cross-block reuse: 5 blocks × 128 transfers over 64 addresses

This is the case `originStorage` structurally cannot serve, since it is discarded at every commit.

| Block | ns/transfer on | ns/transfer off | Speedup | Hit rate | Cache entries | Commit ms |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 5627 | 6044 | +6.9% | 50% | 64 | 0.18 |
| 2 | 7544 | 8510 | +11.4% | 100% | 64 | 0.12 |
| 3 | 8443 | 8728 | +3.3% | 100% | 64 | 0.13 |
| 4 | 6553 | 7499 | +12.6% | 100% | 64 | 0.16 |
| 5 | 8011 | 9260 | +13.5% | 100% | 64 | 0.12 |

Whole run: 7236 vs 8008 ns/transfer, **+9.7%**, gas identical (4,753,280), root match. **Purges during the run: 0** — the cache stayed on the chain head for all five blocks, and the test asserts the root tag advanced with every commit.

Block 1 hits only 50% because half its reads are for slots the seeding commit did not put in the cache; from block 2 onward every first-in-block read hits. The per-block speedups scatter between +3% and +14% around a mean of +10%, which is what a roughly 10% effect looks like on 1 ms blocks even after averaging.

### 9.6 Keccak

**Micro (hash vs table):**

| Input size | Uncached hash | Cache hit | Cached? |
|---:|---:|---:|---|
| 32 B | 249 ns | 19 ns | yes |
| 64 B | 240 ns | 37 ns | yes |
| 128 B | 218 ns | — | no, over the cap |
| 256 B | 423 ns | — | no, over the cap |

A 64-byte hit is roughly **6 times cheaper** than hashing. That part is not in doubt.

**Census (128 ERC-20 transfers, 64 addresses):** 192 hits / 64 misses, 75%. Each transfer hashes two 64-byte mapping keys, one for the sender and one for the recipient, and repeating addresses reuse those hashes.

**EVM-level on vs off:**

| Workload | Hit rate | Speedup | Root / gas |
|---|---:|---:|---|
| noise floor (no cache either arm, N=64) | n/a | −0.8% | match |
| N = 8 | 98% | +7.9% | match |
| N = 64 | 88% | +9.2% | match |
| N = 128 | 75% | −1.4% | match |
| N = 256 | 50% | −4.5% | match |
| 10 tokens × 64 addrs | 75% | −3.6% | match |

The sign follows the hit rate, which is a coherent result rather than noise: at 98% and 88% the saved hashing shows up as a real gain, and by 50% the cost of the misses and inserts has eaten it. The break-even sits somewhere around a 75–85% hit rate. Sharing a storage layout across ten tokens did not help, because the mapping key includes the slot index but not the contract address, so those hashes were already being reused within a single token.

---

## 9.7 Three consecutive mainnet blocks

Everything above is synthetic: an ERC-20 contract driven over a fixed address set, chosen to give the cross-block cache the workload it was designed for. This section runs the same four configurations against real traffic — mainnet blocks 22,000,001 through 22,000,003, replayed in order against a single state database with a commit at each block boundary.

The fixtures come from `block_data/<block>/<tx>/txTest.json`, one standard state test per transaction. The capture was incomplete when this was measured, so the replay covers a subset of each block:

| Block | Transactions replayed | In the block | Coverage |
|---:|---:|---:|---:|
| 22000001 | 30 | 150 | 20% |
| 22000002 | 53 | 103 | 51% |
| 22000003 | 128 | 197 | 65% |

211 transactions, 45,940,534 gas, and the post-state root was identical in all four arms.

### The structural ceiling

Before timing anything, count how much reuse actually exists. A cross-block cache can only serve a slot that some earlier block already touched:

| Block | Slots touched | Also touched by an earlier block | Share |
|---:|---:|---:|---:|
| 22000001 | 229 | 0 | 0% |
| 22000002 | 817 | 21 | **2.6%** |
| 22000003 | 624 | 75 | **12.0%** |

Against the 100% reuse of §9.5, real consecutive blocks share almost nothing. Blocks touch largely disjoint contracts, and the slots they do share are few.

The read-path census says the same thing from the other side:

| Path | Count | Share |
|---|---:|---:|
| origin (already loaded earlier in this block) | 3,132 | 73.7% |
| backend (snapshot / trie) | 1,060 | 25.0% |
| cross-block storage cache | 56 | **1.3%** |

Three quarters of all committed-state reads never reach the cache at all — `originStorage`, the per-block map that already exists, absorbs them. Only 1,116 reads across 211 transactions get as far as the cache layer, and it answers 56 of them, a 5.0% hit rate.

Pre-warming the cache with every slot the three blocks will read lifts that to 658 hits (59.0%), but that is not a realistic state; it is an upper bound constructed by seeding the cache from the union of all the declared pre-states.

The Keccak cache does much better on the same traffic: **820 hits out of 1,517 lookups, 54.1%**, with no equivalent structural limit, because hashing repeats within a block as well as across blocks.

### Why the timing tables cannot answer this

One pass over these blocks is about 20 ms of execution. At 101 repeats per arm the two identical `both off` arms still landed 3.8% apart, and the per-arm spread between the median and the best pass ran 13.5–16.5%. Every effect under test is smaller than that. The measured deltas — `storage only` at −1.35%, `keccak only` at −0.64%, `both on` at −0.22% — are noise, and reading a sign off them would be wrong. The budget sweep makes this obvious: four cache sizes with an identical 5.0% hit rate scattered across +3.0% to +4.0%.

So instead of timing the whole run, count the work each cache avoids and price it.

### What the hits are worth

| Component | Hits | Saved per hit | Total saved | Share of run |
|---|---:|---:|---:|---:|
| storage cache, cold, snapshot backend | 56 | 40 ns | 2 µs | 0.01% |
| storage cache, pre-warmed, snapshot backend | 658 | 40 ns | 26 µs | 0.13% |
| storage cache, cold, trie backend | 56 | ~850 ns | 48 µs | 0.24% |
| storage cache, pre-warmed, trie backend | 658 | ~850 ns | 562 µs | 2.82% |
| keccak cache | 820 | 342 ns | 281 µs | **1.41%** |

The two backend rows matter. This harness has no snapshot layer, so a backend read walks the trie, which costs on the order of a microsecond. A full node answers most of those reads from the snapshot's fastcache instead, where the cache only saves 40 ns per hit — the figure measured directly, and the one that applies in production. Serving reads the snapshot would have served anyway is worth very little.

### Net of the commit overhead

The storage cache is not free. Every commit stages the block's writes and republishes the snapshot, whether or not the block got any hits. Across these three blocks that added **465 µs** (3.16 ms of commits with the cache off, 3.63 ms with it on — about 0.11–0.22 ms per block, consistent with the §9.2 figures).

| Scenario | Saved | Commit overhead | Net |
|---|---:|---:|---:|
| cold, snapshot backend | 2 µs | 465 µs | **−463 µs** |
| pre-warmed, snapshot backend | 26 µs | 465 µs | **−439 µs** |
| cold, trie backend | 48 µs | 465 µs | −417 µs |
| pre-warmed, trie backend | 562 µs | 465 µs | +97 µs |

On a snapshot-backed node the storage cache loses on this workload by roughly an order of magnitude, and it still loses when handed a perfectly pre-warmed cache. It only comes out ahead in the one combination that does not describe a normal full node: no snapshot layer *and* a pre-warmed cache.

The Keccak cache has no commit-side cost, so its 281 µs is net — a real if modest **1.4%** of execution time.

### Caveats

The replay is faithful about the work the EVM does and deliberately loose about chain plumbing. Genesis is the union of every transaction's declared pre-state, which is not a perfectly consistent snapshot, so nonce and EOA checks are skipped and sender balances are topped up when short. All of that happens identically in every arm, and the differential check on root, gas and applied count is what makes the comparison valid. Absolute times are not a mainnet block-processing benchmark; the comparison between arms is the result.

---

## 10. Verdict

**Correctness: pass.** Gas and the post-state root matched with the flags on in every differential cell. Reorg, revert, staged-write, self-destruct, destroy-and-recreate, fold, over-budget eviction and disabled-cache tests all pass, under `-race` as well.

**Storage cache: do not ship it on.** The synthetic evidence looked good — a lookup is half the cost of the snapshot path it replaces (49 ns vs 94 ns, no allocations), the cross-block workload showed about +10% across five blocks, and the address-set sweep reached +18% to +23%. Real blocks did not reproduce any of it.

The reason is structural, not an implementation problem. On mainnet blocks 22,000,001–3, consecutive blocks shared 2.6% and 12.0% of their touched slots, against the 100% the synthetic cell was built to have. Worse, `originStorage` already absorbs 73.7% of committed-state reads before the cache is consulted at all, so the cache saw 1,116 of 4,248 reads and answered 56. At the 40 ns a hit saves against a snapshot-backed read, that is 2 µs on a 20 ms run — against 465 µs of commit-side staging and republishing charged across the same three blocks. It loses by two orders of magnitude, and it still loses when handed a pre-warmed cache holding every slot the blocks will read.

The §9.5 result was not wrong, it was just a workload chosen to have the reuse that real blocks lack. Keep the cache **default off**. Before revisiting it, the thing to change is the commit path: the benefit is capped by how little reaches the cache, so the only route to a positive result is making the cache free at commit time.

**Keccak cache: the better candidate, but modest.** A 64-byte hit costs 37 ns against 240 ns to hash, and it held up on real traffic where the storage cache did not — 54.1% hit rate over 1,517 lookups, worth 281 µs on a 20 ms run, or **1.4%**, with no commit-side cost to offset it. Hashing repeats within a block as well as across blocks, so it does not depend on the block-to-block reuse the storage cache needs. The synthetic sweep still shows the win turning negative below roughly an 80% hit rate at whole-transaction granularity, so keep it **default off** and enable it where the key set is known to be small.

Neither cache should be on by default. Both are safe to enable for further measurement (`--cache.evm.storage`, `--cache.evm.keccak`).

One caveat on all of it: the effects are small enough that whole-run timing cannot resolve them. On the mainnet replay two identical configurations landed 3.8% apart, so the numbers above come from counting the work each cache avoids and pricing it, not from a stopwatch on the run.

Re-run the timing (this file is maintained by hand and is not overwritten):

```
go test ./core/evmcache/experiment -run TestEVMCacheExperiment -timeout 30m -v
```

The mainnet replay of §9.7 is a separate cell. The capture writes one directory per transaction and fills it in afterwards, so a live `block_data` is a moving target; point the harness at a frozen copy for a reproducible run:

```
go test ./core/evmcache/experiment -run TestMainnetBlocksCacheImpact -timeout 40m -v
EVMCACHE_BLOCK_DATA=/path/to/frozen/copy go test ./core/evmcache/experiment -run TestMainnetBlocksCacheImpact -timeout 40m -v
```
