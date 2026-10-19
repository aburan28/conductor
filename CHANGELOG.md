# Changelog

## [0.1.6](https://github.com/aburan28/conductor/compare/v0.1.5...v0.1.6) (2026-10-18)


### Features

* **delphi:** launch project batches from dashboard with scoped WIF ([#98](https://github.com/aburan28/conductor/issues/98)) ([e6d99fb](https://github.com/aburan28/conductor/commit/e6d99fb5afb23fc873c63ec63bb6ac6b7e4f690f))


### Bug Fixes

* **test:** give the throwaway PostgreSQL a locale so PostgreSQL 18 starts on macOS ([#94](https://github.com/aburan28/conductor/issues/94)) ([d99a2bb](https://github.com/aburan28/conductor/commit/d99a2bbb23d5e59862d7614886e414c95c3dfe13))

## [0.1.5](https://github.com/aburan28/conductor/compare/v0.1.4...v0.1.5) (2026-10-17)


### Features

* add advisory quality.lint for public task envelopes ([#87](https://github.com/aburan28/conductor/issues/87)) ([0d829c8](https://github.com/aburan28/conductor/commit/0d829c82b89b45a46534e567bf9017f9b837a7f8))


### Bug Fixes

* **setup:** scaffold the four-tier topology; Opus is core, Fable is frontier ([d4a71dd](https://github.com/aburan28/conductor/commit/d4a71dd71d966f562a5687c0f0422c290be9c045))
* **setup:** scaffold the four-tier topology; Opus is core, Fable is frontier ([b04cb86](https://github.com/aburan28/conductor/commit/b04cb86bad3f09ac253b42f8d0f279ef6af4613a))

## [0.1.4](https://github.com/aburan28/conductor/compare/v0.1.3...v0.1.4) (2026-10-17)


### Bug Fixes

* **release:** dispatch the release build so release-please releases get assets ([#84](https://github.com/aburan28/conductor/issues/84)) ([a0f2217](https://github.com/aburan28/conductor/commit/a0f2217413cae53a2d0d3fa028a2929b24564b06))

## [0.1.3](https://github.com/aburan28/conductor/compare/v0.1.2...v0.1.3) (2026-10-17)


### Features

* **network:** add private WireGuard plans and offline discovery ([#78](https://github.com/aburan28/conductor/issues/78)) ([8b9e99a](https://github.com/aburan28/conductor/commit/8b9e99a68229aa4d5b3d42b45237dcfed72869de))

## [0.1.2](https://github.com/aburan28/conductor/compare/v0.1.1...v0.1.2) (2026-10-17)


### Bug Fixes

* stop unsigned Sparkle modal and stale mesh peer errors ([#76](https://github.com/aburan28/conductor/issues/76)) ([dcce520](https://github.com/aburan28/conductor/commit/dcce5208ec971b72c8995c660bec909054bf0fda))

## [0.1.1](https://github.com/aburan28/conductor/compare/v0.1.0...v0.1.1) (2026-10-17)


### Features

* **admin:** organization policy, branding, group mapping and SCIM parsing ([9a51a6e](https://github.com/aburan28/conductor/commit/9a51a6e4a93fa6c1e6f12912540628c1beac17aa))
* **api:** admin area, require-SSO, SCIM 2.0 and audit export ([75f1574](https://github.com/aburan28/conductor/commit/75f15741442173f0c3b9ce90f6a40cb3bdcbb9ae))
* **api:** GitHub Issues sync ([f0dbf34](https://github.com/aburan28/conductor/commit/f0dbf344d268daebdef71ba8089d1c167993d86b))
* **api:** manage notification channels; conductord runs the relay ([869356d](https://github.com/aburan28/conductor/commit/869356df063030c5b42cbdeeb3a805dd9877cd68))
* **api:** record usage-limit snapshots with owner-only visibility ([4ecadf2](https://github.com/aburan28/conductor/commit/4ecadf25128f467184016207610c3bfe10b8522a))
* **api:** request IDs, access logs, metrics, readiness, body deadlines, shared event feed ([5d852b6](https://github.com/aburan28/conductor/commit/5d852b6309a13ff3a161ad795bb2c83009815e89))
* **api:** single sign-on that issues ordinary bearer tokens ([cf2097c](https://github.com/aburan28/conductor/commit/cf2097c47395953704d7583a6fb19bd76ce980b4))
* **checkpoint:** portable session checkpoints across machines, accounts, and harnesses ([7742d11](https://github.com/aburan28/conductor/commit/7742d11f2bf30598989e83a1dfb9c0b5814b2d0e))
* **checkpoint:** portable session checkpoints across machines, accounts, and harnesses ([b6ce645](https://github.com/aburan28/conductor/commit/b6ce645d8a14f48504fd1954c50a8afa2d9d8a99))
* **cli,dashboard:** conductor login --sso, conductor sso, sign-in buttons ([2dc9605](https://github.com/aburan28/conductor/commit/2dc96052d13304b29a14457b106acdaf2a61702f))
* **cli,web,docs:** github issues commands, issue links, docs ([8b364fe](https://github.com/aburan28/conductor/commit/8b364fefd12a653e019df0c8a6e48c75efae0d86))
* **cli:** conductor notify add|list|remove|test|events ([8111a76](https://github.com/aburan28/conductor/commit/8111a76ff819763c6b2c507674d9b35a5eb7700a))
* **cli:** conductor publish runs the Tailscale serve/funnel recipe ([119579e](https://github.com/aburan28/conductor/commit/119579e89b90be52752e5c0e0a64ec2d6f8cd299))
* **cli:** conductor quota, wrap-sidecar warnings, coord_quota, dashboard card ([558e1b2](https://github.com/aburan28/conductor/commit/558e1b262d022157d40c6c72eb00e123e124efe8))
* **cli:** consistent help, completion, a fuller doctor, and first-run hints ([c7a8c59](https://github.com/aburan28/conductor/commit/c7a8c5913f35f74126934ef85dec901087a8f268))
* **cli:** task done/reopen, claim -&gt; wrap adoption, auto-reserving pre-edit hook, observed paths ([93cb6f5](https://github.com/aburan28/conductor/commit/93cb6f54eb97eb2d883e07c0a4db6280393bd0b9))
* **conductord:** --config file, config check, bootstrap makes an org_admin ([3d5d91d](https://github.com/aburan28/conductor/commit/3d5d91dcf5f4008d879dae18f506a9d4920ec8ba))
* dashboard resume-all button with account switching on usage exhaustion ([7a332be](https://github.com/aburan28/conductor/commit/7a332be265f336b776f15c438579bf89fda51601))
* **dashboard:** a Notifications card in Settings ([7d2554e](https://github.com/aburan28/conductor/commit/7d2554ee4c38b0f048564cd35a44ff1d3e574fd7))
* **dashboard:** budget sharing and mesh follow their feature flags ([fd0c6d8](https://github.com/aburan28/conductor/commit/fd0c6d8a314170256ffd9acc5c1399dcb48a398a))
* **dashboard:** simpler navigation and an admin area ([a1d94e7](https://github.com/aburan28/conductor/commit/a1d94e733137211b641bf8d280e481b1b8698896))
* **db:** external identities and in-flight SSO sign-ins (migration 0012) ([55c0253](https://github.com/aburan28/conductor/commit/55c02537263fe9784021902cc4e7ed50471cb70e))
* **db:** org policy, logos, SCIM provisioning, deactivation (migration 0013) ([f8d9477](https://github.com/aburan28/conductor/commit/f8d9477e69e5429184e4a444d8e746420c99f71d))
* **db:** tracker sync storage (migration 0011) ([ce64a08](https://github.com/aburan28/conductor/commit/ce64a082f096365bbc487d4e52dcf1ea17545e36))
* **dns:** conductor-mesh-dns serves the tailnet service directory ([d11ec34](https://github.com/aburan28/conductor/commit/d11ec340bc2b37275edeecaef22b3102791cb71a))
* **githubapp:** issues permission, issues client, rate-limit backoff ([3aa4808](https://github.com/aburan28/conductor/commit/3aa4808dfd17c33548a15c6e92ca6f041da8715e))
* inference endpoints with auto-discovered models ([24b383a](https://github.com/aburan28/conductor/commit/24b383a3609a1b0ee37f713c801cd0627a2845cd))
* **macos:** back and forward toolbar buttons for the dashboard history ([a60ec75](https://github.com/aburan28/conductor/commit/a60ec75dc16823f4ceff49d7e98607a99b479f41))
* **macos:** Remote Access sheet for Tailscale publishing ([127c4d5](https://github.com/aburan28/conductor/commit/127c4d597b67f57164d7c75016d1e54148c54756))
* migrate Claude chats to persistent Codex threads with account identity hints ([a41e715](https://github.com/aburan28/conductor/commit/a41e715b0f903b97de5e1ef1fa5953c82f2d7205))
* **notify:** --notify-proxy, with the destination still checked ([22eacc1](https://github.com/aburan28/conductor/commit/22eacc11934a4f44f5388a1bc9ab8bc07f6bf8b8))
* **notify:** conflict.blocked, conflict.suggest_join and conflict.detected events ([c22dad7](https://github.com/aburan28/conductor/commit/c22dad70b12a489bc32ce21b6d168ffd1b8e1a67))
* **notify:** notification channels and a replica-safe outbox relay ([18a4f40](https://github.com/aburan28/conductor/commit/18a4f40c35719d9d410efcc220f66ede09f1e764))
* **peer:** discover mesh peers via DNS SRV, no seed node list required ([661ee4a](https://github.com/aburan28/conductor/commit/661ee4a8713c616fd9891207c25e3b1dd1eb3be0))
* **quota:** research usage-limit sources and add local collectors ([4f14e08](https://github.com/aburan28/conductor/commit/4f14e0850ae84e470d65b8bb5b8233ba71cc4605))
* **sso:** Entra ID tenant trust, groups claim, GitHub orgs and teams ([98ccccf](https://github.com/aburan28/conductor/commit/98ccccfe26038d6e7e3406565c27156270f062f2))
* **sso:** OIDC and GitHub sign-in protocol with stdlib ID token verification ([d0c2fec](https://github.com/aburan28/conductor/commit/d0c2fec8a3cd60403a410726c654d3f5f8f1323f))
* **systemd:** user units for postgres + conductord (+ vLLM) with image pre-pulls ([931c4ec](https://github.com/aburan28/conductor/commit/931c4ec0b672bd165e528708568401ea2396088d))
* **systemd:** user units for postgres + conductord (+ vLLM) with image pre-pulls ([9245042](https://github.com/aburan28/conductor/commit/92450422f030b840cc1abc769c05cb91b1ec151f))
* token reset, peer-dns-server, marketing site, codex harness ([11ad2e0](https://github.com/aburan28/conductor/commit/11ad2e0058cf33459065bb3c31dce6bb520d77c3))
* token-free local sign-in, enhanced security mode, GitHub App, shareable invites ([1532c0a](https://github.com/aburan28/conductor/commit/1532c0a0b23f2cd27ecfa53d23d527ac8463e5e1))
* token-free local sign-in, enhanced security mode, GitHub App, shareable invites ([5b347be](https://github.com/aburan28/conductor/commit/5b347be84efc8706acef328b803166177183bb37))
* **tracker:** tracker-neutral issue sync rules ([9111cc2](https://github.com/aburan28/conductor/commit/9111cc2130951df5e9bbf729e6f01c308b2694f2))
* **version:** report the build in every binary and in /v1/health ([58843b1](https://github.com/aburan28/conductor/commit/58843b16ffa7bb36d97b152d57df76b359208539))
* **web:** tell a waiting person when their territory frees up, and announce merges ([31dbdee](https://github.com/aburan28/conductor/commit/31dbdeec6e8cb88fd876554602683137f19e20f0))


### Bug Fixes

* build and publish the universal macOS DMG ([#74](https://github.com/aburan28/conductor/issues/74)) ([6863013](https://github.com/aburan28/conductor/commit/686301305076f4c7e52883445f1b9b3b5776e316))
* canonicalize Claude config root for macOS checkpoints ([4d91888](https://github.com/aburan28/conductor/commit/4d91888f12468d374920f9d048240805d4f3bebb))
* **cli:** drop backticks from flag help, which flag treats as the value name ([eb7b5cc](https://github.com/aburan28/conductor/commit/eb7b5cc27c9075976b27342fdeac53ac0d9b2dce))
* **conductord:** a flag that does not parse exits 2 and is reported once ([5ff5fe6](https://github.com/aburan28/conductor/commit/5ff5fe6e104108a1e00fe44a3579c51b5a667c26))
* **conductord:** graceful shutdown, operations flags, accurate replica notes ([e7685cc](https://github.com/aburan28/conductor/commit/e7685cc9c5b7f1365a81a827ad006a67b5798d4c))
* **core:** keep claims alive with their session, hold territory until merge, finish on merge ([04cb246](https://github.com/aburan28/conductor/commit/04cb246b0b6ed3849e295dc2cd2a5625fd3b546e))
* **github:** seal the app's private key and secrets at rest ([906ebcd](https://github.com/aburan28/conductor/commit/906ebcdb9f38fef9fa3a29920013c58be1a438ef))
* **github:** share the app, setups and check-run dedupe across replicas ([f9bd793](https://github.com/aburan28/conductor/commit/f9bd79347f9292e7db5db1040df40c578b51ebee))
* **github:** the poller sweeps recently closed pull requests, and recording a PR's end is idempotent ([0d6e71a](https://github.com/aburan28/conductor/commit/0d6e71ac95c8ed573925817b372269a1ca3e629a))
* **harness:** a dedicated Codex `exec --json` adapter; name a Codex model id ([7a1db74](https://github.com/aburan28/conductor/commit/7a1db744e99bba685e651f33f0f160c316c26274))
* include larger Claude agent transcripts in checkpoints ([c4e31f3](https://github.com/aburan28/conductor/commit/c4e31f3e43f3c7db216e98ad537f64042dfa0488))
* **mcp:** keep the claimed lease alive in the background, bind it to the session, publish evidence ([b17bb00](https://github.com/aburan28/conductor/commit/b17bb00c7ff0725646de2079d55a9dd12cb2f80c))
* **mesh-dns:** keep _conductor-mesh on the localhost target ([c6ecf61](https://github.com/aburan28/conductor/commit/c6ecf613b5830a5e215cd1da06b07c5a84ad5427))
* **metrics:** count written events; drop an unused test constant ([b7bf7e6](https://github.com/aburan28/conductor/commit/b7bf7e676150c1e58ed2d31f68105c6dd8388986))
* mount the existing Postgres volume when recreating conductor-db ([49cd138](https://github.com/aburan28/conductor/commit/49cd138f99c077bef5fcad3708d8c41556459c3a))
* mount the existing Postgres volume when recreating conductor-db ([947c770](https://github.com/aburan28/conductor/commit/947c770921dd36d3c8f2e8690f029e70f49a6e72))
* recreate conductor-db when Docker says the container is missing ([2bd6859](https://github.com/aburan28/conductor/commit/2bd68597c648b6c25b7609d29ebcab9f6718e1a3))
* recreate conductor-db when Docker says the container is missing ([897b646](https://github.com/aburan28/conductor/commit/897b6466aa3048e0c38c7b3dbbd08361191694e7))
* restore complete workspace before Codex migration handoff ([7209492](https://github.com/aburan28/conductor/commit/72094923e71f6e21f1d7f45ce5b1e0a568ca1e95))
* **scheduler,db:** survive outages, dedupe budget alerts, bound DB calls, add retention ([1f2d3a9](https://github.com/aburan28/conductor/commit/1f2d3a9032a7d8ba731ff093322959fc05ae876d))
* **scheduler:** persist stall-alert dedupe so restarts and replicas do not re-announce ([b9ee082](https://github.com/aburan28/conductor/commit/b9ee08283514468e209dd26750f5b971682abf2b))
* **security:** bind the local database to loopback with a generated password ([caab743](https://github.com/aburan28/conductor/commit/caab7436ec53062759d2ce3ba6aec73ab6e6a22d))
* **security:** close invite takeover, lease and privacy holes, scope runner credentials ([23bb09a](https://github.com/aburan28/conductor/commit/23bb09afce2877b33e548356a3087611fb33dacb))
* **security:** gate task edits, hide private attempt paths, confine scoped tokens ([026caad](https://github.com/aburan28/conductor/commit/026caad820bae8b66d36ed4d0126aa8f4b007aae))
* **security:** harden checkpoint restore and off-host session backups ([f8839b6](https://github.com/aburan28/conductor/commit/f8839b66e93fba746a5bd86b4535d07fb6a706f2))
* **security:** harden local sign-in and the GitHub App against cross-tenant and proxy abuse ([1aae9da](https://github.com/aburan28/conductor/commit/1aae9da27fca5fedd9812d9c9c978e86ff330073))
* **web:** send `status` on task transitions; add Mark done / Reopen and the pull request link ([f1b8d64](https://github.com/aburan28/conductor/commit/f1b8d64e88cedd4955e0763abfbdf10f1cdd16e0))


### Performance

* **notify:** claim per project with a channel, never scanning the rest ([d2102ac](https://github.com/aburan28/conductor/commit/d2102ac2e20006db4e359fc4c1e6eb139c0e2299))
