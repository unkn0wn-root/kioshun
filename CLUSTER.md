# Kioshun Cluster

## Status

> **Experimental:** the cluster implementation is under active development.
  Backward compatibility is not guaranteed across minor releases. Review release notes before upgrading.
  Wire messages are stable at present, but new fields/messages may be added as features evolve.

This document describes the distributed cache cluster components and protocols used by `kioshun/cluster`. It focuses on the replication model, failure handling, and the wire protocol.

## Usage Model (peer-to-peer)

- Peer-to-peer mesh: each service instance runs a full cluster peer. Nodes discover each other via `Seeds`, gossip membership/weights, form a weighted rendezvous ring, and replicate directly. There is no coordinator or proxy.
- In-process node: embed a `cluster.Node` in your service. Start it with a unique `PublicURL` and `BindAddr`, configure `Seeds` with known peers, then wrap it with `NewDistributedCache` to call `Set/Get`.

### Kioshun Mesh-style vs. Redis-style

- Redis Cluster: clients stay clients; they compute slot→node and connect over a client protocol.
- Kioshun Cluster: your app becomes a node in the mesh. It gossips, can be chosen as an owner, stores a shard locally, and routes requests to primary owners when needed.

### Quickstart

Run the same service on three servers. Each instance is a peer in the mesh.

On each server, env:

```
CACHE_BIND=:4443
CACHE_PUBLIC=srv-a:4443   # use srv-b:4443 and srv-c:4443 on other servers
CACHE_SEEDS=srv-a:4443,srv-b:4443,srv-c:4443
CACHE_AUTH=supersecret
```

In code:

```
local := cache.NewWithDefaults[string, []byte]()

cfg := cluster.Default()
cfg.BindAddr = os.Getenv("CACHE_BIND")
cfg.PublicURL = os.Getenv("CACHE_PUBLIC")
cfg.Seeds = strings.Split(os.Getenv("CACHE_SEEDS"), ",")
cfg.ReplicationFactor = 3
cfg.WriteConcern = 2
cfg.Sec.AuthToken = os.Getenv("CACHE_AUTH")

node := cluster.NewNode[string, []byte](cfg, cluster.StringKeyCodec[string]{}, local, cluster.BytesCodec{})
if err := node.Start(); err != nil { panic(err) }
dc := cluster.NewDistributedCache[string, []byte](node)
_ = dc.Set("k", []byte("v"), time.Minute)
v, ok := dc.Get("k")
_ = v; _ = ok
```

Key points:

- Mesh like: every instance is a peer; it gossips, can be selected as an owner, and stores a shard locally.
- Reachability: `CACHE_PUBLIC` must be routable between peers. Open the port in firewalls/security groups.
- Durability: tune `ReplicationFactor` and `WriteConcern` (e.g., RF=3, WC=2).
- Security: set the same `CACHE_AUTH` on all peers. Enable TLS in config if required.
- Adapter scope: `Clear/Size/Stats` act on the local shard only.

## Architecture

```
Clients ──▶ Node (API) ──▶ Owners (RF replicas) over TCP/TLS (CBOR frames)
                      │                │
                      ├── Gossip/Weights (peer discovery + load-based ring)
                      └── Hinted Handoff (per‑peer queues)
```

Core components:
- Rendezvous ring with dynamic weights chooses RF owners per key.
- LWW replication with HLC versions guarantees monotonic conflict resolution.
- Hinted handoff replays missed writes to down/unreachable owners.
- Digest‑based backfill periodically repairs divergence and on join.

## Data Flow

Write path (Set/Delete):
1. Compute owners via rendezvous (RF replicas, ordered).
2. If local is primary, apply locally and record HLC version.
3. Replicate to remaining owners in parallel and wait for WC acknowledgements.
4. On peer error, enqueue a hinted‑handoff record (per‑peer queue).

Read path (Get):
1. If local is primary and key exists, serve locally.
2. Otherwise route to primary owner; decompress/deserialize on success.
3. Clients may hedge reads across owners to reduce tail latency.

## Failure Handling

Hinted handoff:
- Per‑peer queues store newest version per key with absolute expiry.
- Replay loop drains queues with exponential backoff and global RPS limit.
- Auto‑pause stops enqueues under high backlog; replay continues to drain.

Backfill/repair:
- Donor builds bucket digests (count, XOR(hash^version)) for a prefix depth.
- Joiner compares to its local digests - on mismatch, pages keys by bucket.
- Keys are imported with versions and absolute expiries; LWW prunes stale.

Ring membership:
- Gossip exchanges peer addresses, seen timestamps, and load.
- Weight updates rebuild a weighted rendezvous ring for owner selection.

## Consistency Model

- Eventual consistency for reads; WC controls write durability/freshness.
- LWW (HLC) ensures monotonic versions across nodes.
- Rebalancer migrates keys to new primaries (preserves TTL; HLC versions).

## Wire Protocol

Transport:
- Length‑prefixed frames over TCP (optional TLS); each frame carries a CBOR message.
- Initial Hello (with token) authenticates peers when auth is enabled.

Messages:
- Get/Set/Delete (+Bulk) carry keys, values, compression flag, expiry, and version.
- LeaseLoad supports coordinated loader on primary with single‑flight leases.
- Gossip exchanges peer list, load metrics, and hot key samples.
- BackfillDigest/BackfillKeys implement incremental repair.

```
┌────────┬───────────────┬───────────────────────────┐
│ Frame  │ 4B length N   │ N bytes: CBOR(Message)    │
└────────┴───────────────┴───────────────────────────┘

Message Base: { t: MsgType, id: uint64 }
Key/Value: []byte (value may be gzip-compressed; Cp=true)
Version: uint64 (HLC)
```

## Hinted Handoff

```
enqueue(write→peer) ──▶ per‑peer queue (max items/bytes, TTL, DropPolicy)
                                 │
replay loop (RPS) ───────────────┘─▶ send → ok: drop; fail: backoff + requeue
```

- Coalesces by key: keeps newest version; older hints replaced in place.
- Drops expired values (SET with already expired E) and aged hints (TTL).
- Auto‑resume when backlog drains below hysteresis threshold.

## Backfill

```
Joiner → Donor: BackfillDigest(depth)
Donor  → Joiner: Buckets [{prefix, count, hash}]
Joiner compares; for mismatched buckets:
Joiner → Donor: BackfillKeys(prefix, cursor, limit)
Donor  → Joiner: Items [{K, V, E, Ver, Cp}] + next cursor
```

- Depth controls bucket granularity (default 2 bytes = 65,536 buckets).
- Cursor is last key‑hash in bucket for stable pagination.

## Rebalance

- Periodically samples local keys- if primary changed, pushes key to new primary using current HLC and remaining TTL → absolute expiry; deletes local key on success.

## Tuning

- RF ≥ 3, WC ≥ 2 for balanced durability and freshness.
- Handoff: set per‑peer caps and RPS to sustainable values. TTL high enough to cover expected downtimes.
- Backfill: adjust depth for dataset size. Rune page size to donor capacity.
- Backfill: adjust depth for dataset size. Tune page size to donor capacity.
- Timeouts: read/write/idle tuned to network characteristics; inflight caps per peer.
