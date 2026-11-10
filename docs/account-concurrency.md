# Account concurrency in auth-only mode

`auth_only: true` keeps the host as the executor. Account admission does not enable the plugin Executor, ModelRouter, or ResponseInterceptor, copy request/response bodies, or create financial reservations. Key concurrency remains a separate limit.

## Lifecycle

The before-auth hook registers the host-generated lifecycle request ID and forwards it to scheduling through a private header. Incoming client values are overwritten. Scheduling only accepts registered IDs, and checks availability, selects, and acquires an account slot under the same mutex. Existing permission, priority, weight, affinity, and warmup rules still apply. After-auth verifies the selected credential and removes the private header before upstream execution. Final request completion releases the slot idempotently, including failures and cancellation. Usage statistics are not used as a release signal.

A retry replaces the previous logical attempt's slot at the next scheduler call. A model-pool retry or token refresh without another pick retains the same slot. Streams retain their slot until logical execution completion, not merely until the first output token. Accounts that are full are excluded before priority selection; a model-compatible API credential can therefore be chosen when higher-priority accounts are full. If no eligible credential remains, scheduling fails closed rather than delegating to the host's builtin selector.

## Memory and recovery boundaries

Admission state contains only scalar identifiers/state plus a last-touch timestamp, never retained request headers, metadata maps, contexts, bodies, or stream buffers. State is bounded; capacity exhaustion rejects new admissions. Normal completion deletes state, and state is preserved across same-store plugin reconfiguration.

Long streams are not discarded by an arbitrary short TTL: the map is refreshed (`lastTouch`) at every lifecycle observation (register, pick, after-auth). A zombie entry whose completion callback was truly lost is reclaimed by a dedicated sweep once it has been untouched for `authAdmissionTTL` (30 minutes). That bound is safely above the host's ~300s per-request upstream cap, so a live stream is never mistaken for a zombie; the legacy `authPending`/session-affinity sweepers never touch strict admissions.

The in-memory admission map is process-local, so a cold restart always starts at zero — in-flight counts cannot leak across a restart. On graceful hot reload the host calls `plugin.quiesce`: the plugin stops background loops, drains in-flight admissions for up to 5s, then shuts down so the replacement can open the database. A completion callback lost past the drain window is covered by the TTL sweep or, at the latest, the next restart.

The host delivers completion callbacks asynchronously without durable acknowledgements, so a lost callback can leave a conservative stale slot until the TTL sweep or a restart; the plugin cannot safely infer that an arbitrary old stream has ended. Disabling or fusing the plugin also disables its enforcement. This is logical request/attempt admission, **not** a guarantee that every discarded upstream TCP stream has physically closed before a retry starts: the host may asynchronously drain discarded streams.

## Display

The authentication quota cards read the same account counter used for admission. While the quota tab is visible, the frontend polls its existing cached GET endpoint every five seconds. Polling updates badges only, does not trigger upstream quota probes, and does not overwrite edited limit inputs. Stale/missing counts are marked rather than silently rendered as zero. Requests shorter than the polling interval may not be visible in a sampled badge.

## Operational verification

Use lifecycle regression tests and isolated ABI tests before rollout. Keep `auth_only: true`, body-omission capabilities enabled, and exactly one version-pinned plugin artifact installed. Back up the plugin, configuration, peppers, and a consistent database before restart. Do not restore an old database during binary rollback. Verify loaded-artifact hash, version pin, business health, and normal traffic; do not use invalid management credentials or production fault injection as a test.
