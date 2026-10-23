# Using the 1.17.6 cache build

Build from this tree. The stock `geth` binary does not contain these flags.

```bash
cd /data/github/go-ethereum-1.17.6
go build -o build/bin/geth ./cmd/geth
```

Two different commands. The node loads a datadir. The performance test does not.

Fixture layout for the test and for the delay tool:

```
$BLOCKS_DIR/<blockNumber>/<txHash>/txTest.json
```

## 1. geth, loading a full-node datadir

Stop any geth that already has the datadir open.

Caches are off unless you pass the flags. Off is the normal node: one extra nil check, no retained maps.

```bash
./build/bin/geth --datadir /data-evm-old/geth-fullnode/datadir \
  --http --http.addr 127.0.0.1 --http.port 8546 \
  --http.api eth,net,web3,debug \
  --maxpeers 0
```

Turn both caches on:

```bash
./build/bin/geth --datadir /data-evm-old/geth-fullnode/datadir \
  --cache.evm.storage \
  --cache.evm.keccak \
  --http --http.addr 127.0.0.1 --http.port 8546 \
  --http.api eth,net,web3,debug \
  --maxpeers 0
```

| flag | default | effect |
|---|---|---|
| `--cache.evm.storage` | off | keep each touched account's `originStorage` across blocks |
| `--cache.evm.storage.slots` | 65536 | accepted, ignored (no entry budget) |
| `--cache.evm.keccak` | off | cache SHA3 opcode results of at most 64 bytes |
| `--cache.evm.keccak.slots` | 16384 | table size, rounded up to a power of two |

The storage cache starts empty. It fills only as this process executes blocks. It does not scan chaindata. A reorg or `SetHead` clears it. Path-scheme and hash-scheme datadirs both use the same hooks. `chaindata` and `geth/chaindata` are both accepted.

## 2. Performance test, traces only

This replays `txTest.json` in memory from each fixture's `pre`. It never opens the datadir. `EVMCACHE_WINDOW=0` (the default) skips the test.

```bash
EVMCACHE_BLOCK_DATA=/data-hdd-b/minh/evm-tx-replay/block_result \
EVMCACHE_START=24000000 \
EVMCACHE_WINDOW=5 \
EVMCACHE_REPEATS=3 \
EVMCACHE_FORK=Prague \
go test ./core/evmcache/experiment -run '^TestWindowCacheImpact$' -count=1 -timeout 90m -v
```

| env | default | meaning |
|---|---|---|
| `EVMCACHE_BLOCK_DATA` | `txTest_all` | parent of the numbered block folders |
| `EVMCACHE_START` | 23500000 | first block number |
| `EVMCACHE_WINDOW` | 0 (skip) | how many consecutive blocks |
| `EVMCACHE_REPEATS` | 5 | passes per configuration |
| `EVMCACHE_FORK` | `Prague` | `tests.Forks` name. Use `Osaka` if the range is after Osaka |

It runs four arms: both off, storage only, keccak only, both on. Gas and the post-state root must match. The log line is the mean speedup; positive means faster than both off. Per-block numbers are written to `EVM_CACHE_WINDOW_RESULTS.json` in the module root.

Synthetic cells, no fixtures:

```bash
go test ./core/evmcache/experiment -run 'TestTokenRuntimeTransfer|TestEVMCacheExperiment' -count=1 -timeout 30m
```

## Delay tool (separate from the cache)

`cmd/evmcache-delay` also stops geth and opens the datadir, but only to time backend misses. It does not enable the storage or keccak cache. Details are in `cmd/evmcache-delay/README.md`.

```bash
BLOCKS_DIR=/data-hdd-b/minh/evm-tx-replay/block_result \
JSON_NAME=txTest.json \
FROM=24000000 COUNT=5 \
DATADIR=/data-evm-old/geth-fullnode/datadir \
OUTDIR=./storage-delay-out \
./scripts/measure-storage-delay.sh
```
