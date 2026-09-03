# Go-Ethereum v1.14.13 - Comprehensive Caching System Analysis

## Executive Summary

This document provides a comprehensive analysis of all caching mechanisms in the go-ethereum (geth) v1.14.13 repository. The caching system is hierarchical and distributed across multiple layers, optimizing blockchain operations, state management, P2P networking, and transaction processing.

---

## 1. Core LRU Cache Infrastructure

### 1.1 Base LRU Implementation (`common/lru/`)

**Location:** `common/lru/basiclru.go`, `common/lru/lru.go`, `common/lru/blob_lru.go`

#### BasicLRU
- **Type:** Generic LRU cache (`BasicLRU[K comparable, V any]`)
- **Thread Safety:** Not thread-safe (for internal use)
- **Implementation:** Doubly-linked list with hash map
- **Eviction Policy:** Least Recently Used (LRU) - evicts oldest item when capacity reached
- **Operations:**
  - `Add()` - Inserts/updates item, moves to front, evicts if full
  - `Get()` - Retrieves and marks as recently used
  - `Peek()` - Retrieves without updating recency
  - `Remove()` - Explicit removal
  - `Purge()` - Clears all entries

#### Thread-Safe LRU Cache
- **Type:** `Cache[K comparable, V any]`
- **Thread Safety:** Yes (mutex-protected)
- **Wrapper:** Around BasicLRU with sync.Mutex

#### SizeConstrainedCache (Blob LRU)
- **Type:** `SizeConstrainedCache[K comparable, V blobType]` where `blobType` = `[]byte | string`
- **Constraint:** Size-based (bytes) instead of item count
- **Thread Safety:** Yes (mutex-protected)
- **Eviction Policy:** LRU based on total size constraint
- **Use Case:** Variable-sized data like contract code
- **Special Feature:** Content-addressed assumption (same key = same value)

---

## 2. Blockchain Core Caching

### 2.1 BlockChain Cache (`core/blockchain.go`)

**Cache Configuration:**
```go
const (
    bodyCacheLimit     = 256    // Block bodies
    blockCacheLimit    = 256    // Full blocks
    receiptsCacheLimit = 32     // Transaction receipts
    txLookupCacheLimit = 1024   // Transaction lookup entries
)
```

**Cached Data:**

| Cache | Data Type | Capacity | Purpose |
|-------|-----------|----------|---------|
| `bodyCache` | `*types.Body` | 256 | Block transaction lists and uncles |
| `bodyRLPCache` | `rlp.RawValue` | 256 | RLP-encoded block bodies |
| `receiptsCache` | `[]*types.Receipt` | 32 | Transaction receipts |
| `blockCache` | `*types.Block` | 256 | Complete blocks |
| `txLookupCache` | `txLookup` | 1024 | Transaction hash → block mapping |

**Eviction Policy:** LRU for all caches

**Logical Meaning:**
- **Block Bodies:** Contains transaction lists and uncle blocks
- **Receipts:** Transaction execution results (status, gas used, logs)
- **Full Blocks:** Headers + bodies combined
- **Tx Lookup:** Maps transaction hash to its location (block hash + index)

### 2.2 HeaderChain Cache (`core/headerchain.go`)

**Cache Configuration:**
```go
const (
    headerCacheLimit = 512     // Block headers
    tdCacheLimit     = 1024    // Total difficulty
    numberCacheLimit = 2048    // Block number lookups
)
```

**Cached Data:**

| Cache | Data Type | Capacity | Purpose |
|-------|-----------|----------|---------|
| `headerCache` | `*types.Header` | 512 | Block headers |
| `tdCache` | `*big.Int` | 1024 | Cumulative difficulty per block |
| `numberCache` | `uint64` | 2048 | Block hash → block number mapping |

**Eviction Policy:** LRU for all caches

**Logical Meaning:**
- **Headers:** Metadata of blocks (parent hash, timestamp, difficulty, etc.)
- **Total Difficulty:** Cumulative proof-of-work from genesis to this block
- **Number Cache:** Fast block hash to number resolution for chain navigation

### 2.3 Cache Configuration System

**Configuration Structure:** `CacheConfig` in `core/blockchain.go`

```go
type CacheConfig struct {
    TrieCleanLimit      int           // MB for clean trie node cache
    TrieCleanNoPrefetch bool          // Disable prefetching
    TrieDirtyLimit      int           // MB for dirty trie nodes
    TrieDirtyDisabled   bool          // Disable trie caching (archive node)
    TrieTimeLimit       time.Duration // Flush interval
    SnapshotLimit       int           // MB for snapshot cache
    Preimages           bool          // Store preimages
    StateHistory        uint64        // Blocks of state history
    StateScheme         string        // "hash" or "path"
}
```

**Default Values:**
- TrieCleanLimit: 256 MB
- TrieDirtyLimit: 256 MB
- TrieTimeLimit: 5 minutes
- SnapshotLimit: 256 MB

---

## 3. State Database Caching

### 3.1 State CachingDB (`core/state/database.go`)

**Cache Configuration:**
```go
const (
    codeSizeCacheSize = 100000              // Number of code size entries
    codeCacheSize     = 64 * 1024 * 1024   // 64 MB for contract code
    pointCacheSize    = 4096                // Verkle point cache
)
```

**Cached Data:**

| Cache | Data Type | Size | Purpose |
|-------|-----------|------|---------|
| `codeCache` | `[]byte` | 64 MB | Smart contract bytecode |
| `codeSizeCache` | `int` | 100k entries | Size of contract code |
| `pointCache` | `*utils.PointCache` | 4096 | Verkle tree curve points |

**Eviction Policy:**
- `codeCache`: Size-constrained LRU (evicts by total byte size)
- `codeSizeCache`: Count-based LRU
- `pointCache`: Specialized for Verkle trees

**Logical Meaning:**
- **Code Cache:** Stores deployed smart contract bytecode to avoid disk reads
- **Code Size Cache:** Quick lookup for contract size (used in gas calculations)
- **Point Cache:** Cryptographic curve points for Verkle tree verification

### 3.2 Storage Key/Value Access Caching (`core/state/state_object.go`)

**Multi-Level Storage Slot Caching System**

Each `stateObject` implements a sophisticated multi-tier caching system for storage slot access, ensuring that repeated reads of the same key within a transaction or block are served from memory without disk I/O.

#### Level 1: State Object In-Memory Caches (Primary Cache)

**Cache Structure:**
```go
type stateObject struct {
    originStorage      Storage  // Original values loaded from DB
    pendingStorage     Storage  // Pending values from previous block
    dirtyStorage       Storage  // Modified values in current execution
    uncommittedStorage Storage  // Uncommitted changes for tracking
}
```

**Access Pattern for `GetCommittedState(key)`:**

1. **Check `pendingStorage` first** (line 173-175 in state_object.go):
   - Contains finalized but not-yet-committed values from previous block
   - **Cache hit = instant return** (no lower-level access)

2. **Check `originStorage` second** (line 176-178):
   - Contains original values loaded from disk/trie during execution
   - **This is the primary key/value cache for repeated reads**
   - **Cache hit = instant return** (no disk/trie access)

3. **On cache miss** (line 192-206):
   - Load from database through reader interface
   - **Immediately cache in `originStorage`** (line 205)
   - All subsequent reads of same key return instantly from cache

**Current State Access (`getState`):**
- Checks `dirtyStorage` first for modified values (line 162-164)
- Falls back to `GetCommittedState` for committed values

**Cache Lifetime:**
- `originStorage`: Survives entire transaction/block execution
- `dirtyStorage`: Active modifications in current transaction
- `pendingStorage`: Carries over from previous block's finalized state
- `uncommittedStorage`: Tracks changes awaiting commit

#### Level 2: Snapshot Disk Layer Cache (Secondary Cache)

**Location:** `core/state/snapshot/disklayer.go`

```go
func (dl *diskLayer) Storage(accountHash, storageHash common.Hash) ([]byte, error) {
    key := append(accountHash[:], storageHash[:]...)
    
    // Check fastcache (256 MB by default)
    if blob, found := dl.cache.HasGet(nil, key); found {
        return blob, nil  // CACHE HIT
    }
    
    // Load from disk and cache
    blob := rawdb.ReadStorageSnapshot(dl.diskdb, accountHash, storageHash)
    dl.cache.Set(key, blob)  // Cache for future reads
    
    return blob, nil
}
```

**Characteristics:**
- **Type:** Fastcache (256 MB default)
- **Scope:** Global, persists across state objects
- **Purpose:** Flat state storage without trie traversal
- **Eviction:** Fastcache automatic algorithm

#### Level 3: Storage Trie Cache (Tertiary Cache)

**Location:** `core/state/reader.go` - `trieReader`

```go
type trieReader struct {
    subRoots map[common.Address]common.Hash  // Cached storage roots
    subTries map[common.Address]Trie         // Cached opened tries
}
```

**Caching Behavior:**
- Once a storage trie is opened, it's **cached for the entire block** (line 219)
- Avoids repeatedly opening the same storage trie
- Individual trie nodes cached in trie database layer

#### Level 4: Trie Database Node Cache (Quaternary Cache)

Trie nodes themselves are cached in:
- **Clean cache:** Fastcache (256 MB) for immutable nodes
- **Dirty cache:** Map for modified nodes pending commit
- See section 4 for full details

#### Level 5: Prefetcher Deduplication

**Location:** `core/state/trie_prefetcher.go`

```go
type subfetcher struct {
    seenReadSlot  map[common.Hash]struct{}  // Tracks prefetched read slots
    seenWriteSlot map[common.Hash]struct{}  // Tracks prefetched write slots
}
```

**Deduplication Logic (lines 414-433):**
- Prevents duplicate prefetch of same storage slot
- Tracks separate read/write operations
- Counts and reports duplicate attempts (metrics: `dupsRead`, `dupsWrite`, `dupsCross`)

#### Complete Cache Hierarchy Table

| Level | Cache Location | Type | Scope | Typical Hit Rate | Purpose |
|-------|---------------|------|-------|------------------|---------|
| **1a** | `dirtyStorage` | `map[common.Hash]common.Hash` | Per stateObject | Very High | Modified values in current tx |
| **1b** | `pendingStorage` | `map[common.Hash]common.Hash` | Per stateObject | High | Finalized values from prev block |
| **1c** | `originStorage` | `map[common.Hash]common.Hash` | Per stateObject | **Very High** | **Primary repeated-read cache** |
| **2** | Snapshot cache | Fastcache (256 MB) | Global | High | Flat state across blocks |
| **3** | `subTries` map | Opened Trie objects | Per trieReader | Medium | Storage trie structures |
| **4** | Trie node cache | Fastcache (256 MB) | Global | High | Individual Merkle nodes |
| **5** | Prefetcher tracking | `map[common.Hash]struct{}` | Per subfetcher | N/A | Deduplication only |

#### Performance Example: Repeated Storage Reads

```solidity
// Smart contract execution
uint256 value = storageSlot;  // 1st read - may load from snapshot/disk
uint256 copy = storageSlot;   // 2nd read - CACHED in originStorage
uint256 again = storageSlot;  // 3rd read - CACHED (instant in-memory)
for (i = 0; i < 100; i++) {
    x = storageSlot;          // Reads 4-103 - ALL CACHED (instant)
}
```

**Performance Characteristics:**
- **1st read:** May traverse snapshot → trie → disk (cold)
- **2nd+ reads:** Served from `originStorage` (hot, ~10-50ns map lookup)
- **No additional disk I/O** for repeated access
- **No additional gas cost** for EVM (SLOAD already charged once)

#### Cache Invalidation

**Within Transaction:**
- `SetState()` updates `dirtyStorage`
- Dirty values shadow cached `originStorage` values
- Clean/dirty separation maintains read correctness

**Between Transactions:**
- `finalise()` moves dirty → uncommitted
- `uncommittedStorage` prepared for commit
- `originStorage` remains valid for reads

**Between Blocks:**
- State object lifecycle ends
- New stateObject created with fresh caches
- Previous block's state available via snapshot cache

#### Memory Impact

**Per State Object:**
- Each map entry: ~32 bytes (key) + ~32 bytes (value) + overhead
- 100 storage slots cached: ~8 KB per contract
- Cleaned up when state object is released

**Total Memory:**
- Bounded by number of active state objects
- Typically hundreds to low thousands per block
- Total storage caches: Usually < 100 MB per block execution

#### Key Insight: Warm/Cold Access and Transaction Boundaries

**EIP-2929 Implementation:**

The distinction between "warm" (256 gas) and "cold" (2100 gas) storage access is implemented through **TWO separate systems**:

1. **Access List** (`accessList` in StateDB) - Tracks EIP-2929 warm/cold for **gas pricing**
2. **Storage Cache** (`originStorage` in stateObject) - Provides **performance optimization**

**These are independent but complementary systems!**

##### Within a Single Transaction:

```go
// Transaction execution
SLOAD slot1  // 1st access: Cold (2100 gas) + load into originStorage
SLOAD slot1  // 2nd access: Warm (256 gas) + served from originStorage cache
SLOAD slot1  // 3rd access: Warm (256 gas) + served from originStorage cache
```

**What Happens:**
1. **Access List:** `Prepare()` creates new `accessList` for transaction (line 1331 in statedb.go)
2. **First SLOAD:** 
   - `SlotInAccessList()` returns false → charge 2100 gas (cold)
   - `AddSlotToAccessList()` marks slot as accessed (line 105 in operations_acl.go)
   - Load from DB → cache in `originStorage` (line 205 in state_object.go)
3. **Subsequent SLOADs:**
   - `SlotInAccessList()` returns true → charge 256 gas (warm)
   - Read from `originStorage` cache (instant, line 176 in state_object.go)

##### Between Transactions (Critical Behavior):

```go
// Block execution with multiple transactions
Transaction 1:
  SLOAD slot1  // Cold (2100 gas)
  
// Transaction boundary: Finalise() called

Transaction 2:
  SLOAD slot1  // COLD AGAIN (2100 gas)! Access list reset
              // But originStorage cache PERSISTS!
```

**What Happens at Transaction Boundary:**

**1. Access List is RESET** (line 1330-1332 in statedb.go):
```go
func (s *StateDB) Prepare(...) {
    if rules.IsEIP2929 {
        // Clear out any leftover from previous executions
        al := newAccessList()  // <-- CREATES NEW ACCESS LIST
        s.accessList = al
    }
}
```
- **Every transaction starts with a fresh access list**
- All slots become "cold" again for gas pricing
- This is **required by EIP-2929 specification**

**2. Storage Caches PERSIST** (line 237-273 in state_object.go):
```go
func (s *stateObject) finalise() {
    // Move dirtyStorage → pendingStorage
    for key, value := range s.dirtyStorage {
        s.uncommittedStorage[key] = s.GetCommittedState(key)
        s.pendingStorage[key] = value  // <-- PERSISTS
    }
    s.dirtyStorage = make(Storage)  // Clear dirty
    // originStorage REMAINS UNTOUCHED - still cached!
}
```
- `originStorage` is **NOT cleared** - retains all loaded slots
- `pendingStorage` gets updated with finalized values
- `dirtyStorage` is reset for next transaction
- **State object lifecycle continues through the block**

**3. State Object Lifecycle:**
- State objects live for the **entire block**, not just one transaction
- `originStorage` and `pendingStorage` survive transaction boundaries
- Only cleared when block is committed or state object is released

**Performance vs Gas Semantics:**

| System | Scope | Reset Point | Purpose |
|--------|-------|-------------|---------|
| **Access List** | Per-transaction | Every tx start (`Prepare`) | EIP-2929 gas pricing (warm/cold) |
| **originStorage** | Per-block | Block commit | Performance (avoid disk I/O) |
| **pendingStorage** | Per-block | Block commit | Track uncommitted changes |
| **dirtyStorage** | Per-transaction | `finalise()` call | Track current tx modifications |

**Real-World Example:**

```solidity
// Block with 2 transactions

// TX 1: First access to storage[5]
contract.method1() {
    x = storage[5];  // Gas: 2100 (cold, access list empty)
                     // Cached in: originStorage
}

// TX boundary: Finalise called
// - Access list RESET (slot[5] no longer "warm")
// - originStorage NOT reset (slot[5] still cached)

// TX 2: Second access to storage[5]
contract.method2() {
    y = storage[5];  // Gas: 2100 (cold again, new access list)
                     // Performance: INSTANT (from originStorage cache!)
                     // NO disk I/O despite cold gas cost
}
```

**Why This Design?**

1. **EIP-2929 Compliance:** Access list reset ensures gas pricing is transaction-isolated (prevents cross-tx gas optimization exploits)

2. **Performance Optimization:** Cache persistence means:
   - Second transaction doesn't hit disk (even though charged cold gas)
   - Block processing is faster overall
   - Memory efficiency (don't reload same data multiple times in a block)

3. **Correctness:** `pendingStorage` ensures changes from TX1 are visible to TX2 even before commit

This is why the storage cache is more sophisticated than just "warm/cold" tracking - it serves **both** gas semantics (via access list) and performance optimization (via multi-level caching), with different lifetimes for each system.

### 3.3 State Prefetcher (`core/state/trie_prefetcher.go`)

**Type:** Parallel trie node pre-loading system

**Mechanism:**
- Spawns goroutines to pre-fetch trie nodes before they're needed
- Maintains multiple subfetchers (one per storage trie)
- Tracks read/write operations to avoid duplicate fetches

**Caching Strategy:**
- Prefetches based on predicted access patterns
- Loads nodes into trie cache before state execution needs them
- Reduces latency during transaction execution

**Tracked Metrics:**
- Duplicate reads/writes
- Cross-operation duplicates
- Waste (prefetched but unused)

---

## 4. Trie Database Caching

### 4.1 Hash-Based Trie DB (`triedb/hashdb/database.go`)

**Two-Tier Cache System:**

#### Clean Cache (Immutable)
- **Type:** `*fastcache.Cache` (external library: VictoriaMetrics/fastcache)
- **Size:** Configurable (default from `TrieCleanLimit`)
- **Content:** Trie nodes from disk (immutable, committed)
- **Eviction:** Internal fastcache algorithm (approximates LRU)
- **Thread-Safe:** Yes

#### Dirty Cache (Mutable)
- **Type:** `map[common.Hash]*cachedNode`
- **Size:** Tracked as `dirtiesSize`
- **Content:** Recently modified trie nodes (not yet committed to disk)
- **Eviction:** Reference counting + flush on commit
- **Structure:** Doubly-linked list for flush ordering

**Node Lookup Order:**
1. Check clean cache (fastcache)
2. Check dirty cache (in-memory map)
3. Read from disk (LevelDB/Pebble)
4. Store in clean cache for future use

**Eviction Policy:**
- Clean cache: Automatic by fastcache
- Dirty cache: Reference-counted, evicted on commit or when children are no longer referenced

### 4.2 Path-Based Trie DB (`triedb/pathdb/`)

**Cache System:**

#### Clean Node Cache
- **Location:** `disklayer.nodes` - `*fastcache.Cache`
- **Content:** Clean trie nodes from disk
- **Key:** `nodeCacheKey(owner, path)`
- **Eviction:** Fastcache internal algorithm

#### Dirty Buffer Cache
- **Location:** `disklayer.buffer` - in-memory node buffer
- **Content:** Uncommitted trie nodes
- **Structure:** Layered diff system
- **Eviction:** Committed to disk layer when stable

**Node Lookup Order:**
1. Check dirty buffer (pending writes)
2. Check clean cache (fastcache)
3. Read from disk (path-based storage)

**Diff Layer System:**
- Multiple `diffLayer` objects stacked
- Each represents state changes from one block
- Merged down to `diskLayer` on commit
- Allows fast state rollback

---

## 5. Snapshot System (`core/state/snapshot/`)

### 5.1 Disk Layer (`snapshot/disklayer.go`)

**Cache:**
- **Type:** `*fastcache.Cache`
- **Size:** From `SnapshotLimit` config (default 256 MB)
- **Content:** Account and storage snapshots
- **Purpose:** Fast flat-database access without traversing tries

**Cached Data:**
- Account snapshots (balance, nonce, code hash, storage root)
- Storage slot values

**Eviction Policy:** Fastcache automatic eviction

### 5.2 Diff Layer (`snapshot/difflayer.go`)

**Bloom Filter Cache:**
- **Type:** Bloom filter for changed accounts/storage
- **Purpose:** Quick negative lookups
- **Size:** Memory-efficient probabilistic filter
- **Eviction:** When diff layer is merged/flattened

**Lookup Strategy:**
1. Check bloom filter (fast negative check)
2. If bloom hit, traverse diff layers
3. Fall back to disk layer

---

## 6. Transaction Pool Caching

### 6.1 Legacy Pool (`core/txpool/legacypool/`)

**In-Memory Storage:**
- **Structure:** `lookup` table with separate `locals` and `remotes` maps
- **Indexing:** Transaction hash → `*types.Transaction`
- **Sorted Lists:** Per-account sorted by nonce (`sortedMap`)

**Cache Characteristics:**
- No explicit size limit (bounded by config)
- Eviction based on gas price, nonce gaps, or pool fullness
- Separate handling of local vs. remote transactions

### 6.2 Blob Pool (`core/txpool/blobpool/`)

**Persistent Cache:**
- **Type:** Billy database (persistent k/v store)
- **Location:** `datadir/blobpool/pending/`
- **Content:** Blob transactions waiting for inclusion
- **Eviction:** Price-based heap (`evictHeap`)

**Limbo Storage:**
- **Purpose:** Track non-finalized blob transactions
- **Type:** Persistent storage
- **Eviction:** After finalization

**Eviction Policy:**
- Heap-based on execution tip, execution cap, and blob fee
- Maintains per-account minimums
- Evicts cheapest transactions when full

---

## 7. Mining and Block Building

### 7.1 Pending Block Cache (`miner/pending.go`)

**Cache:**
- **Type:** Single-entry cache with TTL
- **TTL:** 2 seconds (`pendingTTL`)
- **Content:** Pre-built pending block

**Eviction Policy:**
- Time-based (expires after 2 seconds)
- Parent hash mismatch
- Explicit update on new block

**Purpose:** Serve RPC requests (`eth_getBlockByNumber("pending")`) without rebuilding

---

## 8. P2P Networking Caches

### 8.1 Discovery v5 Session Cache (`p2p/discover/v5wire/session.go`)

**Cache:**
- **Type:** `BasicLRU[sessionID, *session]`
- **Content:** Encrypted session keys for peer connections
- **Eviction:** LRU

**Logical Data:**
- Read/write encryption keys
- Nonce counter
- Per-peer, per-address sessions

### 8.2 Node Database (`p2p/enode/nodedb.go`)

**Cache:**
- **Type:** LevelDB with expiration
- **Content:** Known peer nodes and metadata
- **Expiration:** 24 hours (`dbNodeExpiration`)
- **Cleanup Cycle:** Hourly (`dbCleanupCycle`)

**Eviction Policy:** Time-based expiration with periodic cleanup

### 8.3 DNS Discovery Cache (`p2p/dnsdisc/client.go`)

**Cache:**
- **Type:** `lru.Cache[string, entry]`
- **Size:** Configurable (default 1000)
- **Content:** DNS-based ENR entries
- **Eviction:** LRU

**Purpose:** Avoid repeated DNS lookups for node discovery

---

## 9. Transaction Fetcher (`eth/fetcher/tx_fetcher.go`)

**Underpriced Transaction Cache:**
- **Type:** `lru.Cache[common.Hash, time.Time]`
- **Size:** `maxTxUnderpricedSetSize` (default likely 10k-100k)
- **Content:** Hashes of rejected underpriced transactions
- **Purpose:** Avoid re-fetching transactions already rejected for low gas price
- **Eviction:** LRU

---

## 10. Downloader Caches

### 10.1 Queue Result Cache (`eth/downloader/queue.go`)

**Cache:**
- **Type:** `resultStore`
- **Size:** `blockCacheMaxItems` = 8192 blocks
- **Memory Limit:** 256 MB (`blockCacheMemory`)
- **Content:** Downloaded blocks/receipts pending insertion

**Eviction Policy:**
- Size-based (both item count and memory)
- Throttling when threshold reached
- FIFO-style processing

### 10.2 Skeleton Syncer (`eth/downloader/skeleton.go`)

**In-Memory Cache:**
- Header ranges during snap sync
- Checkpoint skeletons
- Backfill progress

---

## 11. Gas Price Oracle (`eth/gasprice/gasprice.go`)

**History Cache:**
- **Type:** `lru.Cache[cacheKey, processedFees]`
- **Size:** 2048 entries
- **Content:** Processed gas fee data per block
- **Eviction:** Purged on chain reorg, LRU otherwise

**Logical Data:**
- Historical gas prices for fee estimation
- Percentile calculations
- Block-level fee statistics

**Cache Invalidation:** On chain reorganization (parent hash mismatch)

---

## 12. Filter System (`eth/filters/`)

### 12.1 Log Cache (`eth/filters/filter_system.go`)

**Cache:**
- **Type:** `lru.Cache[common.Hash, *logCacheElem]`
- **Size:** Configurable (default 32 blocks)
- **Content:** Block logs and bodies
- **Eviction:** LRU

**Cached Structure:**
```go
type logCacheElem struct {
    logs []*types.Log
    body atomic.Pointer[types.Body]
}
```

**Purpose:** Speed up log queries for recent blocks (eth_getLogs RPC)

---

## 13. Tracers (`eth/tracers/`)

### 13.1 State Tracker

**Type:** Reference-counted state retention system

**Mechanism:**
- Tracks up to `maximumPendingTraceStates` concurrent trace operations
- Reference counting prevents premature state release
- Blocking wait if too many pending traces

**Not a traditional cache but manages state lifecycle for tracing**

---

## 14. RLP Encoding Cache (`rlp/typecache.go`)

**Type Cache:**
- **Type:** `map[typekey]*typeinfo` with atomic swap
- **Content:** Encoder/decoder functions for Go types
- **Eviction:** Never (grows indefinitely but typically small)
- **Thread-Safe:** Copy-on-write with atomic updates

**Buffer Pool:**
- **Type:** `sync.Pool` for `*encBuffer`
- **Purpose:** Reuse encoding buffers
- **Eviction:** Automatic by Go runtime

**Logical Meaning:** Compile-once, use-many encoding/decoding optimizations

---

## 15. Account Keystore (`accounts/keystore/`)

### 15.1 File Cache (`file_cache.go`)

**Cache:**
- **Type:** `mapset.Set[string]` (file paths)
- **Content:** Scanned keystore file metadata
- **Update:** On filesystem watch events or periodic scan
- **Eviction:** Replaced on full rescan

**Purpose:** Track file modifications without repeated directory scans

### 15.2 Account Cache (`account_cache.go`)

**Cache:**
- **Type:** In-memory account index
- **Structure:**
  - `all`: Slice of all accounts
  - `byAddr`: Map of address → accounts
- **Update:** Filesystem watcher + debounced reload (500ms)
- **Eviction:** Updated on file create/delete/modify

**Purpose:** Fast account lookups without parsing key files

---

## 16. Bloom Filter Caching

### 16.1 State Pruner Bloom (`core/state/pruner/`)

**Cache:**
- **Type:** Bloom filter file on disk
- **Size:** ~2048 MB for mainnet
- **Content:** All state keys in current state
- **Purpose:** Identify unreferenced state for pruning

**Eviction:** Entire bloom replaced on new pruning operation

### 16.2 Snapshot Diff Bloom (`core/state/snapshot/difflayer.go`)

**Cache:**
- **Type:** In-memory bloom filter per diff layer
- **Content:** Account/storage hashes modified in this layer
- **Purpose:** Fast negative lookups before traversing layers
- **Eviction:** When diff layer is merged

---

## 17. Database-Level Caching

### 17.1 LevelDB (`ethdb/leveldb/`)

**Block Cache:**
- **Size:** Configurable (part of `DatabaseCache`)
- **Content:** Compressed LevelDB blocks
- **Eviction:** LevelDB internal LRU

**Open Files Cache:**
- **Size:** From `DatabaseHandles` config
- **Content:** File descriptors
- **Eviction:** LRU by LevelDB

### 17.2 Pebble (`ethdb/pebble/`)

**Block Cache:**
- **Size:** Configurable
- **Type:** Pebble's internal cache
- **Content:** Compressed blocks and index blocks
- **Eviction:** Clock-based algorithm

---

## 18. Consensus-Specific Caches

### 18.1 Clique (PoA)

**Snapshot Cache:**
- **Type:** In-memory snapshots
- **Content:** Signer state at checkpoint blocks
- **Eviction:** LRU with typical size 128

### 18.2 Ethash (PoW)

**Dataset Cache:**
- **Type:** Mmap-ed or memory-resident DAG
- **Size:** ~1+ GB (epoch-dependent)
- **Eviction:** On epoch change

---

## 19. Sender Cacher (`core/sender_cacher.go`)

**Type:** Background worker pool

**Mechanism:**
- Recovers transaction senders (ECDSA signature → address)
- Caches result in transaction object itself
- Worker pool sized to CPU count

**Not a traditional cache:** Results stored in transaction structures

---

## Summary Table: All Cache Sizes

| Component | Cache Name | Type | Size | Eviction Policy |
|-----------|-----------|------|------|----------------|
| **Blockchain** | Block bodies | LRU | 256 | LRU |
| | Block bodies (RLP) | LRU | 256 | LRU |
| | Full blocks | LRU | 256 | LRU |
| | Receipts | LRU | 32 | LRU |
| | Tx lookup | LRU | 1024 | LRU |
| **HeaderChain** | Headers | LRU | 512 | LRU |
| | Total difficulty | LRU | 1024 | LRU |
| | Block numbers | LRU | 2048 | LRU |
| **State DB** | Contract code | Size-LRU | 64 MB | Size-based LRU |
| | Code sizes | LRU | 100,000 | LRU |
| | Verkle points | Custom | 4096 | Verkle-specific |
| **Storage Slots** | originStorage (per object) | Map | Dynamic | Never (per-object lifetime) |
| | pendingStorage (per object) | Map | Dynamic | Block transition |
| | dirtyStorage (per object) | Map | Dynamic | Transaction finalize |
| | Snapshot storage | Fastcache | 256 MB | Fastcache algo |
| **Trie DB (Hash)** | Clean nodes | Fastcache | 256 MB | Fastcache algo |
| | Dirty nodes | Map | Dynamic | Ref-counting |
| **Trie DB (Path)** | Clean nodes | Fastcache | 256 MB | Fastcache algo |
| | Dirty buffer | Layers | Dynamic | Commit-based |
| **Snapshots** | Disk layer | Fastcache | 256 MB | Fastcache algo |
| | Diff bloom | Bloom | Variable | Layer merge |
| **Downloader** | Block results | Custom | 8192 items or 256 MB | Throttle+FIFO |
| **Gas Oracle** | Fee history | LRU | 2048 | LRU + reorg purge |
| **Filters** | Logs | LRU | 32 blocks | LRU |
| **P2P** | Sessions | LRU | Configurable | LRU |
| | DNS entries | LRU | 1000 | LRU |
| | Node DB | LevelDB | Unlimited | 24h expiry |
| **Tx Fetcher** | Underpriced | LRU | ~10k+ | LRU |
| **RLP** | Type info | Map | Unbounded | Never |
| | Encode buffers | Pool | Dynamic | GC-managed |

---

## Key Eviction Policies Used

1. **LRU (Least Recently Used):** Most common - evicts least recently accessed item
2. **Size-based LRU:** Evicts based on total byte size, not item count
3. **TTL (Time-To-Live):** Time-based expiration
4. **Reference Counting:** Evicts when no references remain
5. **Fastcache Internal:** Approximates LRU with lower overhead
6. **Bloom Filter Negative Checks:** Not eviction, but filtering
7. **Price-based Heap:** Transaction pools evict cheapest
8. **Explicit Purge:** Manual clearing (e.g., on reorg)
9. **Copy-on-Write:** RLP type cache grows but rarely evicts

---

## Cache Hierarchy and Dependencies

```
┌─────────────────────────────────────────────────────────────┐
│                    Application Layer                         │
│  (RPC, JSON-RPC APIs, GraphQL, WebSocket subscriptions)     │
└────────────────────────┬────────────────────────────────────┘
                         │
┌────────────────────────▼────────────────────────────────────┐
│               Blockchain Core Layer                          │
│  - BlockChain (block/body/receipt caches)                   │
│  - HeaderChain (header/TD/number caches)                    │
│  - TxPool (pending transaction caches)                      │
└────────────────────────┬────────────────────────────────────┘
                         │
┌────────────────────────▼────────────────────────────────────┐
│              State Management Layer                          │
│  - StateDB (code cache, code size cache)                    │
│  - Trie Prefetcher (predictive loading)                     │
│  - Snapshot System (flat state cache)                       │
└────────────────────────┬────────────────────────────────────┘
                         │
┌────────────────────────▼────────────────────────────────────┐
│              Trie Database Layer                             │
│  - HashDB (clean/dirty trie node caches)                    │
│  - PathDB (clean cache + diff layers)                       │
└────────────────────────┬────────────────────────────────────┘
                         │
┌────────────────────────▼────────────────────────────────────┐
│          Persistent Database Layer                           │
│  - LevelDB (block cache, file handle cache)                 │
│  - Pebble (block cache, index cache)                        │
└─────────────────────────────────────────────────────────────┘
```

---

## Performance Implications

### Cache Hit Benefits:
- **Block/Header Caches:** Avoid disk I/O for recent blocks
- **Trie Node Caches:** Prevent expensive Merkle tree traversals
- **State Caches:** Skip disk reads for smart contract code
- **Snapshot Caches:** Bypass trie entirely for state reads
- **Tx Lookup Caches:** Fast transaction location
- **Prefetcher:** Hides latency of trie node loads

### Cache Miss Costs:
- Disk I/O (LevelDB/Pebble read)
- Merkle proof verification
- RLP decoding
- Cryptographic hashing

### Memory Trade-offs:
- Default total: ~1 GB+ for full node
- Archive node: Disables dirty cache, increases clean cache
- Light client: Minimal caching

---

## Configuration Recommendations

### Full Node (Default):
- TrieCleanCache: 256 MB
- TrieDirtyCache: 256 MB
- SnapshotCache: 256 MB
- Total: ~1 GB + overhead

### Archive Node:
- TrieCleanCache: 1024 MB
- TrieDirtyCache: 0 (disabled)
- SnapshotCache: 0
- Preimages: true

### Light Node:
- TrieCleanCache: 64 MB
- SnapshotCache: 0
- Minimal block caches

### High-Performance RPC Node:
- TrieCleanCache: 2048 MB
- SnapshotCache: 1024 MB
- FilterLogCacheSize: 128 blocks
- Increase DB cache

---

## 20. Cryptographic Hash Caching (Keccak256)

### 20.1 Current State: NO Dedicated Keccak256 Result Cache

**Finding:** Go-Ethereum does **NOT** implement a general-purpose keccak256 hash result cache. Every call to `crypto.Keccak256()` or `crypto.Keccak256Hash()` recomputes the hash from scratch.

**Evidence:**
```go
// crypto/crypto.go lines 84-93
func Keccak256(data ...[]byte) []byte {
    b := make([]byte, 32)
    d := NewKeccakState()        // Creates NEW hasher every time
    for _, b := range data {
        d.Write(b)
    }
    d.Read(b)
    return b                     // Returns fresh computation
}
```

**Frequency of Keccak256 Usage:**
- **~50+ files** across the codebase call `crypto.Keccak256Hash()`
- Used for: Account hashing, storage key hashing, trie node hashing, transaction hashing, block hashing, signature verification, merkle proofs, etc.

### 20.2 Existing Hash-Related Optimizations

Despite no dedicated keccak256 cache, the codebase implements **three types** of hash-related optimizations:

#### A. Structure-Level Hash Caching (Lazy Computation)

**1. Block Hash Caching** (`core/types/block.go`):

```go
type Block struct {
    header       *Header
    transactions Transactions
    // caches
    hash atomic.Pointer[common.Hash]  // <-- Cached hash
    size atomic.Uint64
}

// Line 519-528: Hash is computed ONCE on first call
func (b *Block) Hash() common.Hash {
    if hash := b.hash.Load(); hash != nil {
        return *hash              // CACHED - instant return
    }
    h := b.header.Hash()          // Compute only on first call
    b.hash.Store(&h)              // Cache for future calls
    return h
}
```

**Performance:** After first `.Hash()` call, subsequent calls are instant (atomic.Load ≈ 1-2ns)

**2. Transaction Hash Caching** (`core/types/transaction.go`):

```go
type Transaction struct {
    inner TxData
    hash atomic.Pointer[common.Hash]  // <-- Cached hash
}

// Line 487-501: Same pattern as Block
func (tx *Transaction) Hash() common.Hash {
    if hash := tx.hash.Load(); hash != nil {
        return *hash              // CACHED
    }
    // Compute based on transaction type
    var h common.Hash
    if tx.Type() == LegacyTxType {
        h = rlpHash(tx.inner)
    } else {
        h = prefixedRlpHash(tx.Type(), tx.inner)
    }
    tx.hash.Store(&h)             // Cache
    return h
}
```

**3. Trie Node Hash Caching** (`trie/node.go`, `trie/hasher.go`):

```go
type nodeFlag struct {
    hash  hashNode  // Cached hash of the node (may be nil)
    dirty bool      // Whether the node has changes
}

// trie/hasher.go line 58-62: Check cache before computing
func (h *hasher) hash(n node, force bool) (hashed node, cached node) {
    // Return the cached hash if it's available
    if hash, _ := n.cache(); hash != nil {
        return hash, n            // CACHED
    }
    // ... compute hash if not cached
}
```

**Scope:** Block, Transaction, and Trie node structures cache their **own** hash after first computation. This is **NOT** a global hash result cache.

#### B. Hasher Object Pooling (Reduce Allocations)

Instead of caching hash **results**, go-ethereum pools the **hasher objects** to reduce allocation overhead:

**1. Trie Hasher Pool** (`trie/hasher.go` lines 35-44):

```go
type hasher struct {
    sha      crypto.KeccakState  // Reusable keccak state
    tmp      []byte              // Reusable buffer
    encbuf   rlp.EncoderBuffer   // Reusable encoder
}

var hasherPool = sync.Pool{
    New: func() interface{} {
        return &hasher{
            tmp:    make([]byte, 0, 550),
            sha:    crypto.NewKeccakState(),  // Expensive allocation
            encbuf: rlp.NewEncoderBuffer(nil),
        }
    },
}
```

**Usage:**
```go
h := newHasher(parallel)     // Get from pool
defer returnHasherToPool(h)  // Return to pool after use
```

**2. RLP Hash Pool** (`core/types/hashing.go` lines 31-34):

```go
var hasherPool = sync.Pool{
    New: func() interface{} { return sha3.NewLegacyKeccak256() },
}
```

**3. PathDB Hasher Pool** (`triedb/pathdb/disklayer.go` lines 287-304):

```go
type hasher struct{ sha crypto.KeccakState }

var hasherPool = sync.Pool{
    New: func() interface{} { return &hasher{sha: crypto.NewKeccakState()} },
}

func newHasher() *hasher {
    return hasherPool.Get().(*hasher)  // Reuse from pool
}

func (h *hasher) hash(data []byte) common.Hash {
    return crypto.HashData(h.sha, data)
}

func (h *hasher) release() {
    hasherPool.Put(h)  // Return to pool
}
```

**Performance Impact:**
- **Without pooling:** Each hash computation allocates ~500 bytes for hasher state
- **With pooling:** Amortized allocation cost ≈ 0 for steady-state operations
- **Savings:** Reduces GC pressure during high-throughput operations (block processing, trie hashing)

#### C. Preimage Store (Hash → Preimage Reverse Lookup)

**Location:** `triedb/preimages.go`

**Purpose:** Store the **reverse mapping** (hash → original value) for debugging and certain query operations. This is **NOT** a performance cache.

```go
type preimageStore struct {
    lock          sync.RWMutex
    preimages     map[common.Hash][]byte  // Hash → original data
    preimagesSize common.StorageSize
}
```

**Usage:**
- **Enabled:** Only when `--cache.preimages` flag is set (or archive mode)
- **Storage:** Persisted to disk at `secure-key-` prefix in database
- **Purpose:** Retrieve original storage key from its keccak256 hash (used for debugging, state inspection)
- **NOT for performance:** This is a *reverse lookup* cache, not a *forward hash computation* cache

**Example Use Case:**
```go
// You have: keccak256(storageKey) = 0xabc123...
// You want: original storageKey value
originalKey := db.Preimage(hash)  // Returns the preimage
```

**Eviction Policy:**
- **Memory threshold:** Flushes to disk when in-memory size > 4 MB
- **Disk persistence:** Never evicted (grows indefinitely)

### 20.3 Why No Keccak256 Result Cache?

**Potential Reasons:**

1. **Hash Collision Concerns:** A global cache using `hash(input) → result` would need the **input** as the key. For variable-length inputs (contract code, RLP-encoded data), this could create massive memory overhead.

2. **Low Recomputation Frequency:** Most hash-intensive operations already cache at structure level:
   - Block hashes cached in Block objects
   - Transaction hashes cached in Transaction objects
   - Trie node hashes cached in node structures
   - Therefore, **actual repeated hash computation of identical data is rare**

3. **Hasher Pooling Sufficient:** The dominant cost in keccak256 is often the **allocation** of the hasher state (~500 bytes), not the computation itself. Object pooling addresses this.

4. **Memory vs. CPU Trade-off:** Keccak256 is relatively fast (~2-10 microseconds for typical inputs). Caching would require:
   - Key storage (input data or hash of input)
   - Value storage (32-byte hash result)
   - Cache management overhead (LRU bookkeeping, eviction)
   - May not provide net benefit for non-repeated computations

### 20.4 Where to Implement a Keccak256 Result Cache (If Needed)

If profiling shows that **repeated keccak256 computation of identical inputs** is a bottleneck, here's where to add it:

#### Recommended Location: `crypto/crypto.go`

**Implementation Approach:**

```go
// crypto/crypto.go - Add after existing functions

import (
    "github.com/ethereum/go-ethereum/common/lru"
    "sync"
)

// Keccak256 result cache (optional, disabled by default)
var (
    keccakCacheEnabled atomic.Bool
    keccakCache        *lru.SizeConstrainedCache[string, common.Hash]
    keccakCacheOnce    sync.Once
)

const (
    keccakCacheSize = 16 * 1024 * 1024  // 16 MB cache
)

// EnableKeccakCache enables caching of keccak256 results (experimental)
func EnableKeccakCache() {
    keccakCacheOnce.Do(func() {
        keccakCache = lru.NewSizeConstrainedCache[string, common.Hash](keccakCacheSize)
        keccakCacheEnabled.Store(true)
    })
}

// Keccak256Hash with optional caching
func Keccak256Hash(data ...[]byte) (h common.Hash) {
    // Fast path: caching disabled (default)
    if !keccakCacheEnabled.Load() {
        d := NewKeccakState()
        for _, b := range data {
            d.Write(b)
        }
        d.Read(h[:])
        return h
    }
    
    // Caching enabled: use input as key
    // WARNING: This creates a copy of input data as map key
    var key string
    if len(data) == 1 {
        key = string(data[0])  // Common case: single input
    } else {
        // Concatenate multiple inputs
        var buf []byte
        for _, b := range data {
            buf = append(buf, b...)
        }
        key = string(buf)
    }
    
    // Check cache
    if cached, ok := keccakCache.Get(key); ok {
        return cached  // CACHE HIT
    }
    
    // Compute and cache
    d := NewKeccakState()
    for _, b := range data {
        d.Write(b)
    }
    d.Read(h[:])
    keccakCache.Add(key, h)
    return h
}
```

**Configuration:**

| Parameter | Recommended Value | Rationale |
|-----------|------------------|-----------|
| **Cache Size** | 16-32 MB | Typical RLP-encoded data is 100-1000 bytes; 16 MB ≈ 16k-160k cached hashes |
| **Cache Type** | `SizeConstrainedCache[string, Hash]` | Variable-length inputs require size-based eviction |
| **Eviction** | Size-based LRU | Evict least-recently-used when size limit reached |
| **Thread Safety** | Mutex-protected | Keccak256 called from multiple goroutines |
| **Default** | **Disabled** | Enable via flag or config only after benchmarking |

**Eviction Policy:** Size-based LRU
- Tracks total byte size of keys + values
- Evicts oldest entry when adding new entry would exceed 16 MB
- Key size = len(input data), Value size = 32 bytes

**Expected Hit Rate:** Highly workload-dependent
- **High hit rate:** Repeated trie node hashing during state root computation
- **Low hit rate:** One-time transaction/block hashing
- **Requires profiling** to determine if worthwhile

**Alternative: Input Hash as Key** (Reduces memory overhead)

```go
// Use hash of input as cache key to reduce memory
func Keccak256Hash(data ...[]byte) (h common.Hash) {
    if !keccakCacheEnabled.Load() {
        // ... normal path
    }
    
    // Hash the input to create a fixed-size cache key
    var inputHash [32]byte
    hasher := sha256.New()
    for _, b := range data {
        hasher.Write(b)
    }
    copy(inputHash[:], hasher.Sum(nil))
    
    // Check cache using input hash as key
    if cached, ok := keccakCache.Get(inputHash); ok {
        return cached
    }
    
    // ... compute and cache
}
```

**Trade-off:** 
- **Pro:** Fixed 32-byte key size (better memory efficiency)
- **Con:** Extra hash computation (SHA256) before checking cache
- **Con:** Potential (extremely rare) hash collisions on cache key

### 20.5 Benchmarking Considerations

Before implementing a keccak256 result cache, profile these scenarios:

1. **Block Import:** Measure if identical data is hashed multiple times during block processing
2. **State Root Computation:** Check if trie node hashing benefits from caching (likely already cached at trie node level)
3. **Transaction Pool:** Assess if transaction hash recomputation is significant
4. **RPC Queries:** Determine if repeated state queries trigger redundant hashing

**Profiling Tools:**
```bash
go tool pprof -http=:8080 cpu.prof
# Look for crypto.Keccak256Hash in flame graph
```

**Expected Outcome:** Most hash-intensive operations already benefit from structure-level caching, so a general-purpose cache may show minimal improvement.

---

## Summary Table Update: Hash Caching

| Component | Cache Name | Type | Size | Eviction Policy | Status |
|-----------|-----------|------|------|----------------|--------|
| **Block** | Hash field | `atomic.Pointer[Hash]` | 32 bytes | Never (per-block lifetime) | ✅ Implemented |
| **Transaction** | Hash field | `atomic.Pointer[Hash]` | 32 bytes | Never (per-tx lifetime) | ✅ Implemented |
| **Trie Node** | nodeFlag.hash | `hashNode` | 32 bytes | On node eviction | ✅ Implemented |
| **Hasher Objects** | hasherPool | `sync.Pool` | Dynamic | GC-managed | ✅ Implemented (3 pools) |
| **Preimages** | preimageStore | `map[Hash][]byte` | 4 MB mem, unlimited disk | 4 MB flush threshold | ✅ Implemented (opt-in) |
| **Keccak256 Results** | ❌ None | N/A | N/A | N/A | ❌ Not Implemented |

**Key Takeaway:** Go-Ethereum caches hash results at the **structure level** (Block, Transaction, Trie nodes) but does **NOT** implement a general-purpose keccak256 result cache. If profiling shows repeated computation of identical inputs is a bottleneck, implement an opt-in size-based LRU cache in `crypto/crypto.go` with ~16-32 MB capacity.

---

## Conclusion

Go-Ethereum employs a sophisticated multi-layered caching architecture optimized for blockchain operations:

1. **In-Memory Speed:** Multiple LRU caches for hot data
2. **Size Awareness:** Both count-based and byte-based limits
3. **Write Optimization:** Dirty caches batch disk writes
4. **Read Optimization:** Clean caches + prefetching
5. **Storage Slot Caching:** Multi-tier system ensures repeated reads are served from memory (critical for EVM gas model)
6. **Flat State:** Snapshots bypass trie traversal
7. **Network Efficiency:** P2P session caching
8. **Query Performance:** Log and filter caches for RPCs
9. **Persistence:** Some caches survive restarts (node DB, snapshots)
10. **Hash Computation:** Structure-level caching (Block/Transaction/Trie nodes) + hasher object pooling, but NO general-purpose keccak256 result cache

The eviction policies are predominantly **LRU-based** with specialized strategies for size-constrained, time-based, and reference-counted scenarios. The overall design balances memory usage, disk I/O, and CPU costs to achieve optimal blockchain synchronization and query performance.

**Special Note on Storage Caching:** The multi-level storage slot caching system (originStorage → snapshot → trie nodes) is fundamental to Ethereum's execution model. It directly enables the EIP-2929 "warm" vs "cold" storage access gas pricing, where the first access to a storage slot costs 2100 gas (cold) and subsequent accesses cost only 256 gas (warm). This 8x gas reduction for repeated reads is only possible because of the comprehensive caching infrastructure that keeps accessed slots in memory throughout transaction execution.
