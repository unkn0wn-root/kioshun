Mesh Bench: 3‑Node Kioshun Cluster Load Test

Overview
- Spins up a 3‑node Kioshun mesh cluster (no DB) and a separate runner that issues massive concurrent GET/SET operations.
- Measures p50/p95/p99 latencies for GET and SET, logs HIT/MISS (local vs remote), and performs integrity checks to ensure the cluster never returns the wrong object for a key.

Quick Start
- docker compose -f _benchmarks/cluster/docker-compose.yml up --build

Services
- node1/node2/node3: Minimal HTTP wrappers around a Kioshun cluster node.
  - Endpoints:
    - GET /get?k=KEY → 200 with value on hit, 404 on miss. Headers: X-Cache=HIT_LOCAL|HIT_REMOTE|MISS
    - POST /set JSON {"k":"KEY","v":"VALUE","ttl_ms":0} → 200
    - GET /stats → local shard stats
- runner: Generates high concurrency load and prints percentile latencies and hit ratios.
- direct-runner: Starts 3 kioshun nodes in-process and drives load via the Node API (no HTTP); useful to compare protocol-only performance vs Redis.

Runner Env Vars
- TARGETS: Comma-separated list of node URLs (default: http://node1:8081,http://node2:8082,http://node3:8083)
- DURATION: Test duration (default: 60s)
- CONCURRENCY: Number of goroutines (default: 512)
- KEYS: Key space size (default: 50000)
- SET_RATIO: Percentage of SET ops (0..100, default: 10)
- LOG_EVERY: Log every N ops per worker (default: 0 = disable)
- STATS_EVERY: Print aggregated node /stats every interval (e.g., 10s). Empty disables.
- SET_TTL_MS: TTL used for all SETs. Use -1 for no expiration across all replicas; positive ms for fixed TTL; avoid 0 if you want consistent TTL across owners.

Failure Injection (optional)
- KILL_MODE: none | random | target (default: none)
- KILL_AFTER: when to kill (duration, e.g., 45s)
- KILL_TARGET: base URL of node to kill when mode=target (e.g., http://node2:8082)
- KILL_TOKEN: shared token passed to /kill to authorize

Node Env Vars
- ALLOW_KILL: enable /kill endpoint (default: false)
- KILL_TOKEN: required token to authorize /kill (optional, recommended)
  
Read/Write/Failure Tuning (per node)
- REPLICATION_FACTOR: owners per key (default 3)
- WRITE_CONCERN: acks required (default 2)
- READ_MAX_FANOUT: max parallel read legs (default 2)
- READ_PER_TRY_MS: per-leg timeout (ms)
- READ_HEDGE_DELAY_MS: delay before spinning hedges (ms)
- READ_HEDGE_INTERVAL_MS: spacing between hedges (ms)
- WRITE_TIMEOUT_MS: write timeout (ms)
- READ_TIMEOUT_MS: read timeout (ms)
- SUSPICION_AFTER_MS: suspect peer after (ms)
- WEIGHT_UPDATE_MS: ring weight refresh interval (ms)
- GOSSIP_INTERVAL_MS: gossip interval (ms)

Output
- Prints total ops, hit/miss counts (local/remote), and p50/p95/p99 for GET/SET.
- Performs periodic consistency checks across all nodes and flags mismatches.
- Optionally prints aggregated node stats during the run and at the end.

Stopping
- The runner traps SIGINT/SIGTERM and will always print the summary on exit. Use Ctrl+C safely.

Direct Runner
- Build/run: docker compose -f _benchmarks/cluster/docker-compose.yml up --build direct
- Env knobs (same as node tuning + workload): DURATION, CONCURRENCY, KEYS, SET_RATIO, SET_TTL_MS, KILL_AFTER, CACHE_AUTH, READ_* and *_MS vars.
