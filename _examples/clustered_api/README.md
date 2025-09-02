Kioshun API Demo using cluster

Overview
- Three API containers run a simple Users service, each embedding a Kioshun cluster node. The nodes form a peer‑to‑peer mesh and cache user data.
- An NGINX container load balances requests across the three API containers.
- A load generator drives mixed GET/UPDATE/CREATE traffic and prints latency and cache/DB hit stats.

Run
1. From repo root: `docker compose -f _examples/clustered_api/docker-compose.yml up --build`
2. Hit the API via NGINX: `curl -i http://localhost:8080/users/u000`
3. Watch logs: cache hits return `X-Source: cache`; misses `X-Source: db`.
4. Loadgen prints a summary at the end (defaults: 60s, 100 goroutines).

Endpoints
- GET `/users/{id}` → returns a user; caches under key `user:{id}` with 2m TTL.
- GET `/users` → returns all users; caches under key `users:all` with 1m TTL.
- POST `/users/create` → create user; updates `user:{id}` and invalidates `users:all`.
- PUT `/users/update/{id}` → update user; updates `user:{id}` and invalidates `users:all`.

Config (env vars per API container)
- `PORT` API HTTP port (8081/8082/8083)
- `CACHE_BIND` cluster bind address (e.g. `:5011`)
- `CACHE_PUBLIC` cluster public URL (e.g. `api1:5011`)
- `CACHE_SEEDS` comma-separated peer list (e.g. `api1:5011,api2:5012,api3:5013`)
- `CACHE_AUTH` optional auth token (must match across peers)
