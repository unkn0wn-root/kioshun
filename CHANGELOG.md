# Changelog

All notable changes to this project will be documented in this file. See [standard-version](https://github.com/conventional-changelog/standard-version) for commit guidelines.

## [1.7.0](https://github.com/unkn0wn-root/kioshun/compare/v1.6.0...v1.7.0) (2026-11-24)


### Bug Fixes

* bound write queue drains so Set and Delete cannot starve ([defe3b0](https://github.com/unkn0wn-root/kioshun/commit/defe3b0ede99f283ebbfeccb5589de1be4516ea9))
* clear the write batch after each drain ([f0b86af](https://github.com/unkn0wn-root/kioshun/commit/f0b86af1c54d5cfc9343a6fc5f2abc58ed7bc15d))
* deliver staged removals at Close ([9fb69b5](https://github.com/unkn0wn-root/kioshun/commit/9fb69b531b0ed0578010f28e9fc5342038bb3271))
* drain full queues on barriers and stop drains after Close ([a453068](https://github.com/unkn0wn-root/kioshun/commit/a45306818dab5eb314da3f62e2dbb552685feac9))
* order Set and Delete after earlier SetAsync writes ([977f6d5](https://github.com/unkn0wn-root/kioshun/commit/977f6d5daf936caa3522e4270aebca5fdad6f0ae))
* seal write queues at Close ([24bbfde](https://github.com/unkn0wn-root/kioshun/commit/24bbfdeace83a31fb13340178e1f02996def65e3))

## [1.6.0](https://github.com/unkn0wn-root/kioshun/compare/v1.5.2...v1.6.0) (2026-11-19)


### ⚠ BREAKING CHANGES

* **cache:** size small-cache shards, lock-free Exists, timer callbacks, expired negative TTL
* **httpcache:** follow RFC 9111 shared-cache rules

### Bug Fixes

* **cache:** size small-cache shards, lock-free Exists, timer callbacks, expired negative TTL ([9dda814](https://github.com/unkn0wn-root/kioshun/commit/9dda814dab5ccb175bf549e342a902480d49b41a))
* **cache:** stop losing removal listener wake-ups ([ffb8ef5](https://github.com/unkn0wn-root/kioshun/commit/ffb8ef54466d9d0ce561a39ade6dbe533b08d270))
* **cache:** stop losing write queue wake-ups ([25b8c02](https://github.com/unkn0wn-root/kioshun/commit/25b8c02853cea998b138c292a067fe6d6c43a6bc))
* **httpcache:** follow RFC 9111 shared-cache rules ([020ea38](https://github.com/unkn0wn-root/kioshun/commit/020ea385c590a3e45813d39587edb62c12ac0b00))
* **httpcache:** invalidate every stored variant on unsafe requests ([097a605](https://github.com/unkn0wn-root/kioshun/commit/097a6050e0f48323eebf1e507a9460a7fc6d61e0))
* release pending SetWithCallback timers on Close ([5774e7d](https://github.com/unkn0wn-root/kioshun/commit/5774e7d3ad917b45c6877dbf6e3697b0778b7c29))

### [1.5.2](https://github.com/unkn0wn-root/kioshun/compare/v1.5.1...v1.5.2) (2026-06-15)

### [1.5.1](https://github.com/unkn0wn-root/kioshun/compare/v1.4.2...v1.5.1) (2026-06-12)


### Bug Fixes

* keep stripe tokens off the tiny alloc so id cleanups run ([59d25c4](https://github.com/unkn0wn-root/kioshun/commit/59d25c4d6244b89491e4fab44b3ee7b6bc77837f))

### [1.4.2](https://github.com/unkn0wn-root/kioshun/compare/v1.3.2...v1.4.2) (2026-06-11)


### ⚠ BREAKING CHANGES

* GetCache errors on unregistered names

### Features

* GetCache errors on unregistered names ([300e405](https://github.com/unkn0wn-root/kioshun/commit/300e405cdf6fab8cabab832df51a328ae3d2802c))

### [1.3.2](https://github.com/unkn0wn-root/kioshun/compare/v1.2.1...v1.3.2) (2026-06-11)

### [1.2.1](https://github.com/unkn0wn-root/kioshun/compare/v1.0.0...v1.2.1) (2026-06-10)


### Features

* add hints and comments ([#40](https://github.com/unkn0wn-root/kioshun/issues/40)) ([d931cee](https://github.com/unkn0wn-root/kioshun/commit/d931ceeb4de10391a7fc88cb24be2484bc9b6de3))
* disable stats in default config ([125bec6](https://github.com/unkn0wn-root/kioshun/commit/125bec68822cee0c0a5b46c54d4b17a583e9fcff))


### Bug Fixes

* **cache:** make SieveTinyLFU adaptation windowed ([3f07163](https://github.com/unkn0wn-root/kioshun/commit/3f071631518aacda73da87564dbd847a3c7f9ad6))
* remove heap allocation from strict Set path ([#44](https://github.com/unkn0wn-root/kioshun/issues/44)) ([cf11b75](https://github.com/unkn0wn-root/kioshun/commit/cf11b757d07dc79b1da9fc91e2b327c5c74fc2e2))

## [1.0.0](https://github.com/unkn0wn-root/kioshun/compare/v0.3.14...v1.0.0) (2026-06-02)


### ⚠ BREAKING CHANGES

* v1 (#39)

### Features

* v1 ([#39](https://github.com/unkn0wn-root/kioshun/issues/39)) ([4433b2d](https://github.com/unkn0wn-root/kioshun/commit/4433b2d2a3f6a55dab5786956d0ab68c79bec79a))

### [0.3.15](https://github.com/unkn0wn-root/kioshun/compare/v0.3.14...v0.3.15) (2025-10-30)

### [0.3.14](https://github.com/unkn0wn-root/kioshun/compare/v0.3.13...v0.3.14) (2025-10-29)

### [0.3.13](https://github.com/unkn0wn-root/kioshun/compare/v0.3.12...v0.3.13) (2025-10-29)


### Bug Fixes

* expiration race when upgrading shard lock during get ([dcbc8f0](https://github.com/unkn0wn-root/kioshun/commit/dcbc8f08b883f60f6617519db8929991e7d7a42c))

### [0.3.12](https://github.com/unkn0wn-root/kioshun/compare/v0.3.11...v0.3.12) (2025-10-29)

### [0.3.11](https://github.com/unkn0wn-root/kioshun/compare/v0.3.10...v0.3.11) (2025-09-07)


### [0.3.10](https://github.com/unkn0wn-root/kioshun/compare/v0.3.9...v0.3.10) (2025-09-05)

### [0.3.9](https://github.com/unkn0wn-root/kioshun/compare/v0.3.8...v0.3.9) (2025-09-05)


### Features

* **admission:** drop atomics since we hold lock in cache ([1667b84](https://github.com/unkn0wn-root/kioshun/commit/1667b8492a7102ff7d434e2032e9963a3b51dc9e))

### [0.3.8](https://github.com/unkn0wn-root/kioshun/compare/v0.3.7...v0.3.8) (2025-09-05)

### [0.3.7](https://github.com/unkn0wn-root/kioshun/compare/v0.3.6...v0.3.7) (2025-09-05)

### [0.3.6](https://github.com/unkn0wn-root/kioshun/compare/v0.3.5...v0.3.6) (2025-09-05)


### Features

* micro opt. and fixes ([d0322de](https://github.com/unkn0wn-root/kioshun/commit/d0322de4fa3dc35f3844afa16c6474b7afc65f9f))

### [0.3.5](https://github.com/unkn0wn-root/kioshun/compare/v0.3.4...v0.3.5) (2025-09-03)

### [0.3.4](https://github.com/unkn0wn-root/kioshun/compare/v0.3.3...v0.3.4) (2025-09-03)

### [0.3.3](https://github.com/unkn0wn-root/kioshun/compare/v0.3.2...v0.3.3) (2025-09-03)


### Bug Fixes

* make eviction test more robust after admission change ([8d4a759](https://github.com/unkn0wn-root/kioshun/commit/8d4a75903ebe059b1c78a73f87563bf941362c82))

### [0.3.2](https://github.com/unkn0wn-root/kioshun/compare/v0.3.1...v0.3.2) (2025-09-03)


### [0.3.1](https://github.com/unkn0wn-root/kioshun/compare/v0.3.0...v0.3.1) (2025-09-03)

## [0.3.0](https://github.com/unkn0wn-root/kioshun/compare/v0.2.3...v0.3.0) (2025-09-02)


### Features

* add dockerfile(cmd) and docker-compose (_examples) ([5e777a6](https://github.com/unkn0wn-root/kioshun/commit/5e777a62fd56047dd4aa906d2ceb97d8d79598f5))

### [0.2.3](https://github.com/unkn0wn-root/kioshun/compare/v0.2.2...v0.2.3) (2025-08-29)

### [0.2.2](https://github.com/unkn0wn-root/kioshun/compare/v0.2.1...v0.2.2) (2025-08-29)

### [0.2.1](https://github.com/unkn0wn-root/kioshun/compare/v0.2.0...v0.2.1) (2025-08-28)
