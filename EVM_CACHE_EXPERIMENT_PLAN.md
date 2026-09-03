# EVM Cache — Staged Experiment Plan

**Audience:** an implementing agent working on a fork of go-ethereum.
**Stage 1: storage cache. Stage 2: SHA3 cache.** Both use the same harness and the same method.
**Time budget:** days, not weeks. If a step balloons, stop and report.

---

## 1. The one question, asked twice

> **How much execution time is saved as a function of cache hit rate, and at what hit rate does it break even?**

Every experiment feeds one plot per stage: **speedup (%) vs. hit rate (0–100%)**. Two numbers come off it — the break-even hit rate, and the speedup at the hit rate a real ERC-20 workload actually produces. Don't run anything that doesn't feed that plot.

## 2. Ground rules

1. Baseline and treatment are the **same binary**, cache toggled by a runtime flag, default off.
2. Gas and post-state root **bit-identical** with the flag on. Precondition, not a target.
3. Every speedup number is reported **next to its measured hit rate**.
4. Pinned fork commit and Go toolchain; CPU model and core count recorded once in the results file.
5. In-memory / tmpfs state for all Stage 1–2 work. No disk variance.

---

# STAGE 1 — STORAGE CACHE (primary)

## 3. What is actually being tested

geth already has five layers between an `SLOAD` and the disk: `dirtyStorage` → `pendingStorage` → `originStorage` (all per-stateObject, per-block) → snapshot fastcache (256 MB, global) → trie clean/dirty node caches → disk. So the hypothesis is **not** "cache storage reads" — that exists. It is two narrower claims, and each gets its own measurement:

- **H1 (path cost):** the layers above the snapshot are much cheaper per access than the snapshot layer itself, because a snapshot hit still builds a composite key, allocates, and decodes a blob. If that per-access delta is large, short-circuiting it is worth something.
- **H2 (cross-block gap):** `originStorage` and the state object **die at block commit**. Only the global fastcaches carry across blocks. A bounded, persistent `(address, slot) → value` cache that survives block boundaries closes the one real gap in the hierarchy.

Both are measurable cheaply. H1 decides whether H2 is worth building.

## 4. Experiment S-A — Cost per layer (~2 hours, no cache implementation yet)

Microbenchmark a single storage read resolved at each level, in isolation:

| Level | What it costs |
|---|---|
| `dirtyStorage` / `originStorage` hit | plain Go map lookup — the floor |
| snapshot fastcache hit | composite key construction + fastcache lookup + blob decode |
| snapshot miss → storage trie traversal (nodes in clean cache) | trie path walk |
| trie node cache miss → disk read | worst case |

**Output:** four numbers in nanoseconds, plus allocations per access at each level.

The gap between line 1 and line 2 is the **entire prize for H1**. The gap between line 2 and lines 3–4 is the prize for keeping things resident at all.

**Gate:** if a snapshot hit is already within a few tens of nanoseconds of a map hit, H1 is dead — say so and go straight to H2, or to Stage 2.

## 5. Experiment S-B — Where do ERC-20 reads actually land? (~2 hours)

Add counters at each level (hit at dirty / pending / origin / snapshot / trie / disk) and run a batch of ERC-20 transfers through the EVM against an in-memory state. Record:

- `SLOAD` count per transfer, and the **hit distribution across levels**;
- the same distribution for the **second and later blocks** of a run — this is where the cross-block gap shows up, since `originStorage` starts empty every block while the snapshot cache does not;
- the share of total execution time spent in the state read path, from a CPU profile.

**Output:** the level-hit distribution, and the **state-read share of execution time** — the hard ceiling for Stage 1.

**Free sweep, no code required:** geth already exposes cache-size knobs (`SnapshotLimit`, `TrieCleanLimit`). Shrinking them forces misses down the hierarchy. Sweep them to move the hit distribution without writing anything — this is the cheapest way to see how sensitive execution time is to storage cache hit rate, and it should be run before implementing anything.

**Gate:** write the ceiling down before implementing. If the state-read path is a small share of execution time on this workload, report it and reconsider the whole stage.

## 6. Experiment S-C — Minimal cross-block storage cache (~1 day)

Build the simplest thing that closes the H2 gap.

**Design, kept deliberately narrow:**

- A bounded, process-global map from `(address, slot)` to the **committed value at the start of the current block**.
- It serves the *committed state* read path only — consulted after `pendingStorage` / `originStorage` miss and **before** the snapshot layer. This keeps its semantics simple: it holds exactly what `GetCommittedState` wants.
- Fixed-size, single-digit MB budget. Direct-mapped or simple sharded map; no elaborate eviction at this stage.
- Populated on block commit from the slots the block actually wrote and read.
- Counters for hits, misses, invalidations, and evictions.

**Invalidation — write this down before coding, it is the whole risk:**

- Any slot written during the block must have its entry **updated at commit**, never left stale.
- On a **reorg or any chain-head change, purge the whole cache**. Do not attempt fine-grained reorg handling at this stage — correctness first, refinement only if the numbers justify it.
- Reverted internal calls must not leave poisoned entries. The simplest safe rule: the cache is only ever written at block commit, never mid-execution. Mid-execution reads populate `originStorage` as they do today. This makes reverts a non-issue by construction — take this route unless measurement demands otherwise.
- The cache must never be consulted against a different state root than the one it was populated from. Tag it with the state root or block hash it belongs to and check on read.

**Optional second variant, only if S-A shows a large H1 gap:** a flat in-block `(address, slot)` map short-circuiting the composite-key/decode work above the snapshot. Measure it, but don't build both before seeing S-A.

## 7. Experiment S-D — The hit-rate sweep (the main event, ~half a day)

### D1 — Synthetic sweep (clean signal)

Read storage slots drawn from a pool of size *N*, sweeping *N* against the cache size to hit roughly **0 / 25 / 50 / 75 / ~100%** hit rate. Measures cache mechanics with no EVM noise, and gives the curve shape and empirical break-even point. It should agree with the arithmetic from S-A; if not, the harness is wrong — fix that first.

### D2 — ERC-20 sweep (realistic signal)

Same sweep, hit rate driven by workload instead of a knob: batches of ERC-20 transfers over an address set of **8 / 64 / 1 000 / 100 000**. Small set ⇒ high slot reuse ⇒ high hit rate. Record the *measured* hit rate per cell; never assume it.

Two extra cells, one run each:

- **Cross-block reuse:** a multi-block run with a Zipf-distributed hot address set. This is the cell that isolates exactly what `originStorage` cannot capture today — **the most important cell in Stage 1.**
- **Worst case:** 100 000 unique addresses, no reuse. Zero benefit, full overhead plus invalidation bookkeeping. **Blocking** — a regression beyond ~1% means the design is wrong regardless of best case.

### D3 — Block-level sanity check (3 cells only)

Synthetic blocks of ERC-20 transfers, import time measured for high / realistic / worst-case hit rate. Checks whether the micro-level win survives once state commit and trie root computation are in the picture. A shrink here is a real finding, not a broken benchmark. No full matrix at this stage.

## 8. Correctness (blocking, minimal set)

- **Differential:** every benchmark run asserts identical gas used and identical post-state root, flag off vs. on. Any divergence stops everything.
- Targeted tests: write-then-read within a block; write in block N, read in block N+1; slot cleared to zero; account self-destructed / recreated; reverted internal call that wrote a slot; two contracts with the same slot index (confirms the address is really part of the key).
- Reorg test: build a fork, reorg to it, confirm the cache purges and subsequent reads are correct.
- Race detector on the concurrent path.
- Upstream EVM/state test suites pass with the flag forced on.

---

# STAGE 2 — SHA3 RESULT CACHE (secondary)

Run only after Stage 1 concludes. It reuses the entire harness, so it should be fast.

## 9. Why it is worth keeping on the list

geth has **no** keccak result cache anywhere, and Solidity mapping access is `keccak256(key ‖ slot)` — a fixed 64-byte input that repeats across transactions, across blocks, and across different ERC-20 contracts, because their storage layouts are identical. It is also a **pure function**: no invalidation, no reverts, no reorgs, no gas change. It is by far the cheapest thing here to prove correct — which is exactly why it is second and not first: it is the easy one, not the important one.

## 10. Experiments (mirrors Stage 1)

- **K-A — is there anything to win (~1 hr):** standalone microbenchmark of keccak256 on 64 bytes vs. a cache hit (including full-key comparison) vs. a miss-then-insert. Sweep input size 32 / 64 / 128 / 256. Break-even ≈ miss overhead ÷ (hash cost + miss overhead). If the hit path isn't clearly cheaper than just hashing 64 bytes, stop.
- **K-B — census (~1 hr):** `SHA3` count per ERC-20 transfer, input-size distribution, duplicate-input rate within tx / within block / across blocks, and keccak's share of execution time. That share is the ceiling.
- **K-C — minimal implementation (~half a day):** hook the **`SHA3` opcode only**, never the general keccak used by trie hashing where inputs are near-unique. Cache inputs ≤ 64 bytes. Fixed-size direct-mapped table, overwrite on collision, no LRU — entries can never go stale so no recency bookkeeping is needed. **On an apparent hit, compare the full input bytes before returning.** Never trust a table index or truncated key: one silent collision yields a wrong storage slot and a wrong state root. That comparison stays inside the timed path.
- **K-D — hit-rate sweep:** synthetic pool-size sweep, then the ERC-20 address-set sweep, plus one **cross-contract** cell (same address set across ~10 identical-layout token deployments — this tests the most load-bearing claim in the analysis and costs almost nothing) and the blocking worst-case cell.

---

## 11. Measurement hygiene (both stages)

- ≥ 10 runs per cell, compared with `benchstat` or equivalent; report mean with confidence interval.
- **Noise floor first:** run baseline against baseline and record the apparent "speedup". Never report a delta smaller than it.
- Interleave baseline and treatment runs rather than batching them.
- Report `allocs/op` alongside time — a cache that trades CPU for allocations looks good in a microbenchmark and loses at block level.
- Report peak memory delta next to every speedup. A few percent bought with hundreds of MB is not obviously a good trade on a node already spending 512 MB on trie and snapshot caches.

## 12. Decision, per stage

| Outcome | Action |
|---|---|
| Realistic cell ≥ 3% speedup, clear of noise, worst case < 1% regression | Promote: block-level matrix, then real block replay |
| Win at micro level, gone at block level | Re-target at the read-only `eth_call` / RPC path, where trie-commit cost is absent |
| Worst case regresses, realistic hit rate negligible, or win inside noise | Kill it, write up the numbers, move to the next stage |

Compare the measured speedup against the **ceiling** from S-B / K-B, so the reader can see whether the implementation captured the available headroom.

**Deferred — do not start now:** mainnet block replay, full-node integration, parallel-execution interaction, per-contract-type eviction policies, opcode-sequence fusion for the Solidity overflow-check pattern, `eth_call` result caching, lock-contention tuning.

## 13. Results file

One running file in the repo. Every row: date, commit, flag state, workload cell, N runs, mean ± CI, **measured hit rate**, allocs/op, peak memory, state-root-match yes/no.

Deliverable per stage: that file, the hit-rate/speedup plot, and a one-page verdict against §12.

---

### Appendix

- Pin the solc version and **commit the ERC-20 bytecode** rather than compiling at test time; emitted opcodes are compiler- and optimizer-dependent, and a floating compiler makes results irreproducible.
- Pin fork rules explicitly in the harness rather than relying on defaults. EIP-1153 (`TSTORE`/`TLOAD`) shipped in **Cancun/Dencun (March 2024)**, not Prague as the slide deck lists it.
- The `SHA3` opcode is `KECCAK256` in newer specs and code — same opcode.
- Remember that EIP-2929 warm/cold gas pricing and the storage cache are **independent systems**: the access list resets every transaction, while `originStorage` persists for the whole block. A cross-block cache changes performance only — it must not touch gas.
