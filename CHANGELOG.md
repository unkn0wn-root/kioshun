# Changelog

All notable changes to this project will be documented in this file. See [standard-version](https://github.com/conventional-changelog/standard-version) for commit guidelines.

### [0.3.15](https://github.com/unkn0wn-root/kioshun/compare/v0.3.14...v0.3.15) (2025-10-30)

### [0.3.15](https://github.com/unkn0wn-root/kioshun/compare/v0.3.14...v0.3.15) (2025-10-30)

### [0.3.14](https://github.com/unkn0wn-root/kioshun/compare/v0.3.13...v0.3.14) (2025-10-29)

### [0.3.13](https://github.com/unkn0wn-root/kioshun/compare/v0.3.12...v0.3.13) (2025-10-29)


### Bug Fixes

* expiration race when upgrading shard lock during get ([dcbc8f0](https://github.com/unkn0wn-root/kioshun/commit/dcbc8f08b883f60f6617519db8929991e7d7a42c))

### [0.3.12](https://github.com/unkn0wn-root/kioshun/compare/v0.3.11...v0.3.12) (2025-10-29)

### [0.3.11](https://github.com/unkn0wn-root/kioshun/compare/v0.3.10...v0.3.11) (2025-09-07)


### Features

* add MaxConcurrentHandshakes and resoect container limits with GOMAXPROCS ([bfbcc78](https://github.com/unkn0wn-root/kioshun/commit/bfbcc7842fc3fd0dcf3c4db654ee6fbadf406f96))
* **cluster:** use ID instead of PublicURL ([e5e9bb3](https://github.com/unkn0wn-root/kioshun/commit/e5e9bb39b201fc434293800b385dc0a5f04bfc2c))


### Bug Fixes

* tests after publicurl to node id change ([d9e7f83](https://github.com/unkn0wn-root/kioshun/commit/d9e7f837ff264a4db6b32c4caed9bcb3e18e2667))

### [0.3.10](https://github.com/unkn0wn-root/kioshun/compare/v0.3.9...v0.3.10) (2025-09-05)


### Bug Fixes

* **node:** make close idempotent ([4c183b5](https://github.com/unkn0wn-root/kioshun/commit/4c183b54db016e836074017f9f8b476594e171d3))

### [0.3.9](https://github.com/unkn0wn-root/kioshun/compare/v0.3.8...v0.3.9) (2025-09-05)


### Features

* **admission:** drop atomics since we hold lock in cache ([1667b84](https://github.com/unkn0wn-root/kioshun/commit/1667b8492a7102ff7d434e2032e9963a3b51dc9e))
* **client:** if local is primary and key exists, serve locally ([6adb9e2](https://github.com/unkn0wn-root/kioshun/commit/6adb9e2f74ddc5de4b2f08056fb2eef8ab2a20ca))

### [0.3.8](https://github.com/unkn0wn-root/kioshun/compare/v0.3.7...v0.3.8) (2025-09-05)


### Features

* add NotInRing to indicate that donor is not ready yet ([0fd11f5](https://github.com/unkn0wn-root/kioshun/commit/0fd11f5fd7db04edb9d15782f8063771dd25f0c3))

### [0.3.7](https://github.com/unkn0wn-root/kioshun/compare/v0.3.6...v0.3.7) (2025-09-05)


### Bug Fixes

* **node:** add itself to ring ([cc96554](https://github.com/unkn0wn-root/kioshun/commit/cc965545264e6be40e99720a037fd283710344f3))

### [0.3.6](https://github.com/unkn0wn-root/kioshun/compare/v0.3.5...v0.3.6) (2025-09-05)


### Features

* micro opt. and fixes ([d0322de](https://github.com/unkn0wn-root/kioshun/commit/d0322de4fa3dc35f3844afa16c6474b7afc65f9f))

### [0.3.5](https://github.com/unkn0wn-root/kioshun/compare/v0.3.4...v0.3.5) (2025-09-03)

### [0.3.4](https://github.com/unkn0wn-root/kioshun/compare/v0.3.3...v0.3.4) (2025-09-03)

### [0.3.3](https://github.com/unkn0wn-root/kioshun/compare/v0.3.2...v0.3.3) (2025-09-03)


### Features

* added new cluster benchmarks - http via wrapper and direct - via inter claster RPC ([287cedd](https://github.com/unkn0wn-root/kioshun/commit/287cedde636154d340a05bfcc6c8c5cec304f854))


### Bug Fixes

* make eviction test more robust after admission change ([8d4a759](https://github.com/unkn0wn-root/kioshun/commit/8d4a75903ebe059b1c78a73f87563bf941362c82))

### [0.3.2](https://github.com/unkn0wn-root/kioshun/compare/v0.3.1...v0.3.2) (2025-09-03)


### Features

* add cluster tests ([dd1ab76](https://github.com/unkn0wn-root/kioshun/commit/dd1ab764ef6e8d4f535e325358a2a96ce0ce8775))


### Bug Fixes

* reset peer only on fatal err; fix example api paths ([409fd08](https://github.com/unkn0wn-root/kioshun/commit/409fd08e3c46da6e526572d70963abc09e86f65f))

### [0.3.1](https://github.com/unkn0wn-root/kioshun/compare/v0.3.0...v0.3.1) (2025-09-03)


### Features

* add clustered api to examples dir ([90047df](https://github.com/unkn0wn-root/kioshun/commit/90047dfe759fa1188da4ba083be2cca9ec7fad4e))

## [0.3.0](https://github.com/unkn0wn-root/kioshun/compare/v0.2.3...v0.3.0) (2025-09-02)


### Features

* add dockerfile(cmd) and docker-compose (_examples) ([5e777a6](https://github.com/unkn0wn-root/kioshun/commit/5e777a62fd56047dd4aa906d2ceb97d8d79598f5))
* add kioshun adapter ([48a79af](https://github.com/unkn0wn-root/kioshun/commit/48a79afad83b1bc04abc650cfecc052311dcc306))
* add kioshun node starter to cmd ([c6df4e9](https://github.com/unkn0wn-root/kioshun/commit/c6df4e9c3db3f29db28fb5687abc3feb0820acc6))
* add snapshot bridge between kioshun and cluster ([a8cd204](https://github.com/unkn0wn-root/kioshun/commit/a8cd20442b85eb505896bc21fe7f7cf000f141f5))
* add timeout-only exponential backoff ([ca3fbc1](https://github.com/unkn0wn-root/kioshun/commit/ca3fbc130643b73b9f7105b764ad1eca28d3047c))
* alias distributed cache as client and accept context ([888f5cb](https://github.com/unkn0wn-root/kioshun/commit/888f5cb4c9381855e6f44848d0edc4dd8c39793a))
* get from other replicas and add panelize node ([7f4803a](https://github.com/unkn0wn-root/kioshun/commit/7f4803ad1f69210773b0695d0ce6485c78d2c807))
* move trie and util to internal and fix adapter type ([7ff9dac](https://github.com/unkn0wn-root/kioshun/commit/7ff9dac93ee5b71c991e6d56708d257197bdc7be))
* reset peer on peer closed ([b17ead0](https://github.com/unkn0wn-root/kioshun/commit/b17ead0f492d7b47e532a0b0e306361e40064b77))


### Bug Fixes

* adapter type ([98d3842](https://github.com/unkn0wn-root/kioshun/commit/98d38429c08b1477e3c176397f2292d3e2cc4146))

### [0.2.3](https://github.com/unkn0wn-root/kioshun/compare/v0.2.2...v0.2.3) (2025-08-29)

### [0.2.2](https://github.com/unkn0wn-root/kioshun/compare/v0.2.1...v0.2.2) (2025-08-29)

### [0.2.1](https://github.com/unkn0wn-root/kioshun/compare/v0.2.0...v0.2.1) (2025-08-28)
