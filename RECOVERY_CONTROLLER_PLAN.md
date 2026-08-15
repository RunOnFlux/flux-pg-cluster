# Autonomous Recovery Controller Plan

## Purpose

This document defines the staged redesign of the Flux PostgreSQL cluster agent into an unattended recovery controller. The target behavior is the behavior expected from a careful database administrator:

- preserve data before optimizing availability;
- use the Flux API as the authoritative desired membership;
- continuously collect direct and indirect evidence from every agent;
- remember durable cluster history across container replacement;
- select the safest and most advanced surviving PostgreSQL copy deterministically;
- rebuild etcd and Patroni without manual intervention when the evidence is sufficient;
- fence nodes that Flux has removed from the application;
- expose exact recovery state and reasons instead of silently looping;
- validate the recovered database before declaring success;
- exercise every supported decision in unit, integration, and failure-injection tests.

Persisting `/var/lib/etcd` is now a deployment requirement and is assumed for all new deployments. This substantially reduces the number of events that need disaster recovery. The controller still needs to handle lost, corrupt, or mutually inconsistent etcd state because old deployments and exceptional failures will continue to exist.

## Accepted operating assumptions

### Flux membership is authoritative

The current, stable node list returned by the Flux API defines which placements are allowed to participate in the cluster.

- A node that is absent from a confirmed Flux view is no longer eligible to be PostgreSQL primary, Patroni leader, etcd voter, recovery authority, or proxy target.
- A running node that observes itself absent from the confirmed Flux view must self-fence as quickly as practical.
- Surviving nodes remove a Flux-absent member from etcd as soon as etcd has write quorum.
- A newly appearing Flux member is treated as fresh until its agent proves otherwise. It joins etcd as a learner and cannot become PostgreSQL primary until it has a valid copy and Patroni considers it eligible.
- Flux views must be sampled repeatedly because different nodes may see an API update at different times.

The controller must never make a destructive decision from one Flux response. It must record the response signature, successful observation time, consecutive confirmation count, and the view reported by other agents.

### No external recovery witness

There will be no external consensus service dedicated to recovery. The nodes will build the best available cluster-wide view by exchanging signed observations.

This provides strong protection against asymmetric visibility. For example, if A can see B and C while B can only see A, B learns from A's fresh signed view that C is alive and must not declare C absent.

It does not solve the theoretical complete-partition case. If A, B, and C cannot communicate with one another, all can still reach Flux, Flux continues listing all three, and policy permits a lone node to recover after a timeout, no internal protocol can prove that another partition is not also alive. A quorum or external fencing authority is required for that proof.

The implementation will therefore expose two policies:

- `conservative`: an unreachable Flux-listed data-bearing node blocks promotion indefinitely unless it is removed from Flux;
- `availability-first`: after a configurable and well-evidenced absence interval, unreachable Flux-listed nodes may be excluded and the deterministic authority may recover.

The requested default is `availability-first`, with a ten-minute default absence interval. Logs, history, and status APIs must label when recovery depends on timeout-based absence rather than authoritative Flux removal.

### PostgreSQL data is the durable recovery source

etcd and Patroni are the control plane. PostgreSQL PGDATA is the data plane and must never be deleted merely to make control-plane recovery easier.

Automatic PGDATA replacement is allowed only after a recovered primary is writable, its PostgreSQL system identifier and recovery epoch have been verified, and policy explicitly permits re-cloning stale copies. Until those conditions hold, all PGDATA copies remain untouched.

## Terminology

- **Desired member:** an IP/node currently present in the confirmed Flux view.
- **Removed member:** a previously known node absent from a confirmed Flux view.
- **Direct observation:** this agent successfully probed the target agent itself.
- **Indirect observation:** another directly reached agent reports a recent direct observation of the target.
- **Present:** direct evidence is fresh, or fresh indirect evidence proves another desired member can reach the node.
- **Suspect:** probes are failing but the absence timeout has not elapsed, or peer views disagree.
- **Absent by Flux:** the confirmed Flux view excludes the node.
- **Absent by timeout:** Flux still includes the node, but all direct probes and fresh indirect views have failed for the configured interval.
- **Fenced:** the node is proven unable to serve PostgreSQL writes or participate in recovery. Self-fencing based on Flux exclusion is the primary internal mechanism.
- **Recovery authority:** the only node permitted to create the next etcd/Patroni epoch and promote PostgreSQL.
- **Recovery epoch:** a monotonically increasing identifier recorded in persisted history and exchanged by agents. It coordinates reachable nodes and detects stale state; it is not a substitute for external consensus during a complete partition.

## Desired architecture

### 1. Dedicated agent status and view API

Keep the existing `/cluster-identity` compatibility endpoint, but introduce a versioned read-only API through the existing proxy listener:

- `GET /agent/v1/health`
  - agent process and schema version;
  - node name, current incarnation, and uptime;
  - no sensitive cluster data.
- `GET /agent/v1/status`
  - local PostgreSQL identity, durable role, control state, timeline, WAL position, version, and preflight state;
  - local etcd process, cluster ID, member ID, membership, learner/voter state, and write-quorum result;
  - Patroni role and DCS leader when available;
  - latest confirmed Flux view and observation age;
  - current recovery phase, epoch, authority, and blocking reason.
- `GET /agent/v1/view`
  - the node's latest direct observations of all desired and recently removed members;
  - the Flux view signature and confirmation count used for that observation;
  - timestamps, ages, probe outcomes, and source node;
  - never recursively embeds other nodes' indirect views.
- `GET /agent/v1/history/head`
  - latest persisted history sequence, hash, epoch, and cluster system ID for divergence detection.

All recovery-relevant responses must be authenticated in both directions. The current request-only HMAC over plain HTTP is insufficient because it does not authenticate response content.

The initial compatible implementation will use a request nonce and an HMAC over:

```text
protocol-version || request-nonce || response-body-sha256
```

The response includes the signer node name and signature. Verification uses the existing cluster secret and constant-time comparison. Replayed responses fail because the nonce differs. The subsequent TLS stage will use the existing cluster CA and mTLS on the endpoint, while keeping signed bodies for auditability and mixed-version rollout.

The API remains read-only. Recovery commands are not accepted remotely; every node derives actions from the same evidence and deterministic rules.

### 2. Fast observation loop

Recovery detection must not share the existing 60-second membership-maintenance loop.

Create a dedicated controller loop that:

1. polls Flux;
2. probes every desired agent concurrently;
3. fetches fresh one-hop views from reachable agents;
4. merges direct and indirect evidence;
5. writes the durable observation snapshot;
6. evaluates self-fencing;
7. evaluates normal membership repair;
8. evaluates disaster-recovery eligibility;
9. advances or holds the recovery state machine.

Initial configuration:

| Variable | Default | Purpose |
|---|---:|---|
| `RECOVERY_CONTROLLER_INTERVAL_SECONDS` | `5` | Controller evaluation interval |
| `AGENT_PROBE_TIMEOUT_SECONDS` | `5` | Timeout per direct status/view probe |
| `AGENT_ABSENCE_TIMEOUT_SECONDS` | `600` | Continuous evidence window before a Flux-listed node may be absent by timeout |
| `AGENT_ABSENCE_MIN_PROBES` | `60` | Minimum failed direct-probe samples; prevents a fast loop from shortening the intended window |
| `AGENT_INDIRECT_VIEW_MAX_AGE_SECONDS` | `20` | Maximum age of an indirect positive observation |
| `FLUX_VIEW_CONFIRM_CYCLES` | `3` | Consecutive equal Flux responses before the view is confirmed |
| `FLUX_API_SELF_FENCE_TIMEOUT_SECONDS` | `60` | Maximum time a node may remain writable without a successful Flux membership refresh |
| `REMOVED_NODE_SELF_FENCE_GRACE_SECONDS` | `10` | Grace after confirmed self-exclusion before forced local shutdown |
| `RECOVERY_DECISION_CONFIRM_CYCLES` | `3` | Identical decisions required before execution |
| `RECOVERY_DECISION_CONFIRM_INTERVAL_SECONDS` | `5` | Minimum spacing between confirmations |
| `RECOVERY_ABSENCE_POLICY` | `availability-first` | `conservative` or `availability-first` |

Configuration validation must reject unsafe or nonsensical combinations rather than silently accepting zero or negative values.

### 3. Persisted cluster history

Store recovery metadata under:

```text
/var/lib/postgresql/data/.flux-agent/
```

This directory is created only after PGDATA is established (`global/pg_control` exists). Creating it on a fresh empty volume would poison strict fresh-bootstrap detection.

Files:

- `cluster-history.jsonl`: append-only, hash-chained important events;
- `recovery-state.json`: atomically replaced current controller state;
- `latest-view.json`: atomically replaced merged observation snapshot;
- `recovery-epoch.json`: last proposed, accepted, and completed recovery epoch;
- `schema-version`: persisted-state format version.

Each history record includes:

- schema version and sequence number;
- previous-record hash and current-record hash;
- app name, Patroni scope, PostgreSQL system ID;
- local node name, IP, and process incarnation;
- confirmed Flux membership and its signature;
- direct/indirect observations relevant to the decision;
- etcd cluster/member IDs and membership when readable;
- PostgreSQL role, state, version, timeline, and WAL position;
- Patroni leader/role when readable;
- recovery epoch, phase, authority, reason, and outcome;
- wall-clock timestamp plus monotonic elapsed durations where available.

Writes must use temporary-file, `fsync`, atomic rename, and parent-directory `fsync`. History corruption must be detected and reported; it must not silently become authority evidence.

History is evidence, not proof of current liveness. Its main purposes are:

- remember the last confirmed primary and system ID after total DCS loss;
- remember all known members and their last durable progress;
- distinguish a new replacement from a previously data-bearing member;
- make authority selection deterministic when peers share the same last complete view;
- record exactly why a destructive or availability-changing action occurred;
- survive container replacement with PGDATA;
- allow a re-cloned node to inherit cluster history while generating a new local incarnation.

The controller must tolerate old PGDATA without this directory and bootstrap history from current evidence without weakening recovery guards.

### 4. Flux-authoritative self-fencing and removal

Self-fencing is necessary to make Flux removal meaningful.

When a node observes a confirmed Flux view that excludes itself:

1. mark the node `REMOVED` in persisted state;
2. immediately stop advertising itself through the SQL proxy;
3. refuse recovery authority and etcd join/bootstrap actions;
4. request Patroni/PostgreSQL shutdown;
5. after the short grace, verify PostgreSQL is no longer accepting writes and force a fast local stop if needed;
6. keep only the read-only agent endpoint alive so peers can confirm that it is fenced;
7. continue polling Flux and allow normal startup only after the node is included again with a new confirmed view/incarnation.

When Flux cannot be refreshed successfully for `FLUX_API_SELF_FENCE_TIMEOUT_SECONDS`, a current primary must fence itself. Replicas may remain readable but cannot promote or rebuild DCS until Flux is available again.

Surviving nodes use the confirmed Flux view to remove excluded etcd members. A reachable removed member should already report `fenced=true`; an unreachable removed member is considered absent by Flux without waiting the full agent-absence timeout.

This stage must remove the current manual deletion of Patroni's leader key. Patroni TTL, self-fencing, and verified recovery replace that unsafe shortcut.

### 5. Distributed cluster-view construction

For every desired member, maintain:

- latest successful direct observation;
- consecutive direct failures and first-failure monotonic time;
- latest successful indirect observation from each reachable peer;
- the observer's Flux view signature;
- identity/signature verification outcome;
- state: `present`, `suspect`, `absent_flux`, or `absent_timeout`.

Merge rules:

1. A valid fresh direct response means `present`.
2. A valid fresh indirect report that another directly reached node can see the target means `present` from the recovery decision's perspective.
3. Conflicting fresh views mean `suspect`; they never count as absence.
4. A node excluded by a confirmed Flux view means `absent_flux`.
5. A Flux-listed node becomes `absent_timeout` only when:
   - the Flux view stayed confirmed and unchanged for the entire interval;
   - the minimum number of direct attempts failed;
   - no fresh indirect observation reports it present;
   - all reachable desired agents report an equivalent absence interval;
   - the target did not publish a newer incarnation or history head;
   - the decision remains identical for the configured confirmation cycles.
6. Any new positive evidence resets the absence timer.

Agent views are accepted only from a directly authenticated peer, and only the peer's own direct observations are consumed. This prevents unbounded gossip loops and stale evidence amplification.

### 6. Deterministic recovery decision engine

Implement recovery policy as a pure package with serialized inputs and outputs. It must not perform network, filesystem, supervisor, etcd, Patroni, or PostgreSQL mutations. This makes the complete decision table unit-testable.

Inputs:

- confirmed Flux view;
- merged member observations and absence classifications;
- persisted history heads;
- local PostgreSQL preflight result;
- etcd process/quorum/membership state;
- current recovery epoch/state;
- configured safety policy.

Decision order:

1. If etcd has write quorum, do not force a new etcd cluster. Let Patroni handle ordinary failover.
2. Exclude Flux-removed and confirmed-fenced members.
3. In conservative mode, block if any desired data-bearing member is not present.
4. In availability-first mode, exclude a desired member only after it satisfies `absent_timeout`.
5. Reject app/scope, system-ID, PostgreSQL-major-version, or recovery-epoch conflicts.
6. Prefer a unique valid durable primary when history and current state agree.
7. If the former primary is absent, rank valid replicas by timeline ancestry and replayable durable WAL position.
8. Resolve exact progress ties by stable node identity, not current IP ordering.
9. If exactly one valid PGDATA copy remains, choose it even if its durable role is replica, provided its preflight succeeds.
10. If no valid current copy exists, enter backup/PITR recovery rather than initializing an empty database.
11. Only the selected authority may execute the recovery epoch. Every other node waits or prepares to join as a learner/replica.

The output is one of:

- `NO_ACTION_HEALTHY`;
- `WAIT_FOR_EVIDENCE`;
- `SELF_FENCE`;
- `REMOVE_MEMBER`;
- `RESTART_LOCAL_ETCD`;
- `REJOIN_ETCD_LEARNER`;
- `RECOVER_DCS_AS_AUTHORITY`;
- `RECLONE_FROM_AUTHORITY`;
- `RESTORE_FROM_BACKUP`;
- `BLOCKED_UNSAFE`.

Every decision contains a stable machine-readable reason code, human-readable explanation, evidence digest, authority, epoch, and earliest reevaluation time.

### 7. PostgreSQL candidate preflight

Before authority execution, validate more than system ID and signal files:

- PostgreSQL major version matches the image;
- `pg_controldata` is readable and internally consistent;
- database state is understood;
- timeline and checkpoint/recovery LSN are readable;
- required timeline-history and WAL files are available or fetchable;
- checksums are verified when enabled, with an appropriately bounded verification policy;
- disk has enough free space for crash recovery and WAL;
- PGDATA ownership and permissions are correct;
- the candidate can complete recovery in an isolated, non-listening preflight mode where feasible;
- no evidence says another non-absent node is a later authoritative primary.

Timeline selection must understand ancestry. A higher timeline is eligible only when its history descends from the known cluster history; unrelated branches remain blocked.

If the chosen authority fails preflight, do not simply pick the next node by IP. Re-run the pure decision engine with a signed/persisted `candidate_invalid` result. A fallback candidate is permitted only when its history is compatible and accepting its lower position conforms to the configured RPO policy.

### 8. Idempotent recovery executor

Persist and expose each phase:

```text
IDLE
DETECTING
COLLECTING_VIEWS
WAITING_FOR_ABSENCE
SELECTING_AUTHORITY
PREFLIGHTING_AUTHORITY
FENCING_REMOVED_MEMBERS
PREPARING_DCS
RECOVERING_DCS
RECONCILING_PATRONI_DCS
PROMOTING_POSTGRESQL
VERIFYING_PRIMARY
JOINING_ETCD_LEARNERS
RECLONING_REPLICAS
VERIFYING_CLUSTER
COMPLETE
BLOCKED
```

Every mutation must be safe to repeat after agent or container restart. Before each action, re-read current evidence and verify that the recovery epoch and authority have not changed.

Force-new recovery completion requires all of the following:

- etcd is writable and reports the intended cluster/member identity;
- stale Patroni ephemeral keys were removed successfully;
- `/initialize` equals the selected PostgreSQL system ID;
- dynamic Patroni configuration is present and valid;
- exactly one Patroni leader exists;
- the leader is the selected authority and reports the expected system ID/timeline;
- a SQL read-write transaction succeeds through the local proxy;
- no non-authority agent reports itself writable primary;
- expected learners are joining without changing quorum prematurely;
- the completed recovery epoch is persisted before success is logged.

Do not ignore etcd mutation errors. A failed phase remains pending and is retried idempotently; it is never logged as complete.

### 9. Replica rejoin and repair

After primary verification:

- matching replicas attempt normal Patroni rejoin/rewind;
- replicas with incompatible timelines use `pg_rewind` only when prerequisites are satisfied;
- empty replacements clone from the verified primary;
- mismatched system-ID PGDATA remains quarantined by default;
- automatic deletion/reclone requires the verified recovery epoch and explicit policy;
- never run two base backups into the same PGDATA;
- throttle clones and learner promotion so recovery traffic does not destabilize the new primary;
- retain quarantined metadata/history before any permitted wipe.

### 10. Observability

Expose and log:

- current controller phase and time in phase;
- confirmed Flux view and age;
- each node's direct and indirect reachability;
- absence timer and number of failed probes;
- etcd quorum state and membership;
- candidate rankings and rejection reasons;
- selected authority and recovery epoch;
- whether timeout-based absence was used;
- PostgreSQL preflight results;
- RTO from failure detection to writable primary;
- estimated data-loss distance between selected and last-known primary LSN;
- rejoin/reclone progress;
- terminal verification results.

Logs must use stable reason codes so operators and tests do not depend on prose matching.

## Implementation sequence

Each item is a separate PR. Do not combine stages that change election policy with unrelated refactoring. Every PR must include unit tests, integration tests appropriate to the stage, upgrade compatibility, documentation, and rollback behavior.

### Item 0: Establish release gates and baseline

Work:

- pin Patroni and etcd versions;
- add Go unit and race tests to CI;
- add a non-destructive Docker smoke/failover job to CI;
- correct the normal-failover test so its assertion and timeout both enforce the intended SLA;
- capture current recovery timing and behavior as baseline artifacts;
- validate that `/var/lib/etcd` persistence is present/documented for new deployments;
- correct Patroni's invalid `ttl/loop_wait/retry_timeout` defaults and current setting names without changing PostgreSQL crash-recovery policy.

Exit criteria:

- no image publishes unless unit tests pass;
- normal primary-node loss is demonstrated within 60 seconds;
- ordinary restart with persistent etcd does not enter force-new recovery.

### Item 1: Versioned signed agent API

Work:

- implement `/agent/v1/health`, `/status`, `/view`, and `/history/head`;
- add nonce-bound response signatures;
- reload current credentials/membership for each request;
- retain `/cluster-identity` compatibility;
- add schema negotiation for mixed-image rollout.

Exit criteria:

- forged, replayed, stale, wrong-app, wrong-scope, and wrong-node responses are rejected;
- an old-image peer can block unsafe recovery but cannot authorize new recovery;
- endpoints remain available while etcd, Patroni, and PostgreSQL are stopped.

### Item 2: Durable history and controller state

Work:

- implement atomic persisted snapshots and hash-chained JSONL history;
- migrate safely from deployments with no history;
- ensure fresh PGDATA detection is unaffected;
- generate a new runtime incarnation after replacement/restart;
- expose history head through the API.

Exit criteria:

- history survives container replacement with PGDATA;
- truncated/corrupt state is detected and recovered from the last valid record;
- cloned nodes do not mistake the source node's incarnation for their own;
- no history file can cause an empty database to be initialized accidentally.

### Item 3: Flux membership controller and self-fencing

Work:

- add the fast Flux observation loop and confirmed-view state;
- implement self-fencing when excluded or when Flux freshness expires;
- stop proxy routing before database shutdown;
- remove the direct Patroni leader-key deletion path;
- make removed-node etcd cleanup use the confirmed Flux view.

Exit criteria:

- a running primary removed from Flux stops accepting writes within the configured bound;
- one transient Flux response does not fence a member;
- a primary unable to refresh Flux beyond the safety timeout self-fences;
- no code manually deletes a live Patroni leader lease based only on REST reachability.

### Item 4: Direct and indirect cluster views

Work:

- concurrently probe all agents;
- exchange one-hop direct views;
- implement freshness, conflict, and absence timers;
- persist and expose the merged view;
- keep the existing etcd/Patroni probes as corroborating evidence, not agent identity.

Exit criteria:

- when A sees C and B sees A, B treats C as present from A's fresh signed view;
- stale indirect observations expire;
- cycles and recursive gossip cannot keep a dead node present forever;
- any conflicting current view blocks recovery until resolved or timeout policy explicitly applies.

### Item 5: Pure recovery decision engine

Work:

- move all authority and action selection into a pure package;
- add conservative and availability-first absence policies;
- incorporate Flux removal, timeout absence, persisted history, recovery epoch, system ID, role, timeline, and LSN;
- preserve deterministic outcomes across input ordering.

Exit criteria:

- exhaustive table-driven tests cover every decision and rejection reason;
- all nodes given the same evidence produce byte-identical decisions;
- only the chosen authority can return an execute action;
- lone-survivor outcomes are explicitly tested for Flux-removed peers and timeout-absent peers.

### Item 6: Idempotent recovery executor and verification

Work:

- implement persisted phases and recovery epochs;
- make force-new, DCS reconciliation, Patroni promotion, learner join, and verification resumable;
- verify every etcd mutation;
- require SQL write and single-primary checks before completion;
- replace `/tmp`-only recovery flags/counters with persisted controller state where appropriate.

Exit criteria:

- killing the agent at every phase resumes safely;
- repeated execution does not create a second etcd epoch or wipe PGDATA;
- failed etcd key deletion/set never produces a success state;
- non-authority nodes cannot promote while the authority is recovering.

### Item 7: Candidate preflight, timeline ancestry, and fallback

Work:

- add version, control-data, WAL, disk, permissions, checksum, and recovery preflight;
- parse timeline history and validate ancestry;
- persist candidate-invalid evidence;
- implement policy-controlled safe fallback and backup restore outcome.

Exit criteria:

- a corrupt highest-LSN candidate is rejected before promotion;
- a valid descendant timeline can win over an ancestor;
- divergent branches remain blocked;
- fallback never silently accepts additional data loss beyond configured RPO.

### Item 8: Replica repair automation

Work:

- automate rewind/rejoin/reclone after verified recovery;
- quarantine mismatched PGDATA metadata;
- bound concurrent base backups and learner promotions;
- retain the default no-wipe safety policy unless explicitly changed.

Exit criteria:

- fresh nodes join automatically;
- compatible former primaries rewind and become replicas;
- mismatched copies are never deleted before authority verification;
- a failed clone retries cleanly without partial-PGDATA ambiguity.

### Item 9: RTO tuning and production observability

Work:

- tune fast detection independently of slow Flux membership maintenance;
- target normal failover below 60 seconds;
- define timeout-based disaster-recovery RTO as absence timeout plus less than 60 seconds;
- expose metrics/status and stable reason codes;
- document conservative versus availability-first risk.

Exit criteria:

- measured timing is asserted in integration tests;
- operators can tell exactly why recovery is waiting, blocked, or executing;
- timeout-based absence is prominent in logs and status.

## Required integration and chaos scenarios

All scenarios must assert PostgreSQL system ID, timeline, exactly one writable primary, preserved sentinel data, etcd membership/quorum, Patroni membership, proxy routing, and recovery history—not merely that `SELECT 1` eventually succeeds.

### Normal operation

1. Kill the primary container; two survivors elect within 60 seconds.
2. Kill only PostgreSQL on the primary; local crash recovery follows the configured policy.
3. Restart one node with persistent etcd and PGDATA; no force-new occurs.
4. Delay a new Flux placement; healthy quorum continues and the node later joins as learner/replica.
5. Replace one node with a fresh persistent volume.

### Flux authority and fencing

6. Remove a live replica from Flux; it self-fences and is removed from etcd.
7. Remove a live primary from Flux; it stops proxy writes and PostgreSQL before another primary is accepted.
8. Deliver the new Flux view to nodes at different times; no premature recovery or dual primary occurs.
9. Return one transient incomplete Flux response; no member is fenced.
10. Make Flux unavailable to the primary beyond the configured freshness timeout; it self-fences.
11. Re-add a fenced node with a new incarnation; it rejoins safely.

### Agent view exchange

12. A reaches B and C; B reaches only A; B learns that C is present indirectly.
13. A's view of C becomes stale; B eventually moves C from present to suspect.
14. Peers report conflicting membership views; recovery blocks with the correct reason.
15. Replay an old signed response with a new nonce; verification rejects it.
16. Return a forged body, wrong app/scope, wrong node name, or wrong incarnation.
17. Mix old and new agent images during rollout.

### etcd loss and reconstruction

18. Stop all nodes and restart with persistent etcd; the original etcd cluster resumes.
19. Delete all etcd data while retaining all PGDATA; the unique durable primary reconstructs DCS.
20. Delete all etcd data after the former primary is permanently removed; the most advanced compatible replica wins.
21. Retain etcd only on one survivor with an old three-voter membership; force-new safely reduces it.
22. Two dead members are replaced by fresh nodes; the survivor recovers and replacements join as learners.
23. Inject failure after force-new but before Patroni key cleanup; restart resumes the same epoch.
24. Inject failure during each DCS mutation; completion is withheld until verification passes.
25. Restart/kill the recovery authority during every controller phase.

### Lone-survivor policies

26. One data-bearing node remains and Flux lists only it; it recovers automatically.
27. One data-bearing node plus reachable fresh replacements; all select the data node.
28. One data-bearing node while old members remain Flux-listed and unreachable; conservative mode blocks.
29. Repeat scenario 28 in availability-first mode; recovery occurs only after the shortened test absence interval and minimum probes.
30. An indirect fresh observation reports one old member alive during scenario 29; its absence timer resets and recovery blocks.
31. The deterministic authority is unreachable to the other survivors; non-authorities do not promote.
32. A previously absent authority reappears with a newer incarnation or epoch during confirmation; recovery decision is cancelled and recomputed.

### PostgreSQL candidate selection

33. Unique durable primary with matching replicas.
34. Former primary absent; replicas share timeline and have different WAL positions.
35. Equal WAL positions; stable node identity breaks the tie.
36. Valid higher descendant timeline beats its ancestor.
37. Divergent timeline histories block recovery.
38. Multiple durable primaries block recovery.
39. PostgreSQL system IDs disagree.
40. Highest-LSN candidate is missing required WAL or fails preflight.
41. Candidate PostgreSQL major version differs from the image.
42. PGDATA is partial, unreadable, checksum-corrupt, out of disk, or has wrong ownership.
43. Exactly one valid PGDATA remains but it is durably marked replica; it is promoted after preflight.
44. No valid PGDATA remains; recovery selects backup/PITR and never runs `initdb` over the old epoch.

### Rejoin and data safety

45. Former primary rejoins through `pg_rewind`.
46. Empty replacement clones through `pg_basebackup`.
47. Interrupted base backup restarts cleanly.
48. Mismatched system-ID node remains quarantined when automatic wipe is disabled.
49. Explicitly permitted re-clone happens only after verified authority and epoch.
50. Multiple replacements are throttled and etcd permits only one learner at a time.

### Network and timing faults

51. Full node-to-node partition while all nodes can query Flux; assert documented policy behavior and ensure deterministic authority restrictions.
52. Asymmetric packet loss on agent, etcd client, etcd peer, Patroni REST, and PostgreSQL ports independently.
53. DNS/API delay, HTTP 500s, malformed Flux responses, and stale Flux responses.
54. Clock skew; absence decisions rely on local elapsed duration and observation ages are bounded defensively.
55. Certificate expiry/rotation and HMAC secret mismatch.
56. Recovery completes within the configured RTO under controlled failure timing.

## CI and validation policy

Every PR runs:

1. formatting and static checks;
2. `go test ./...`;
3. `go test -race ./cmd/flux-agent ./internal/...`;
4. pure decision-engine fuzz/property tests;
5. Docker smoke tests for PostgreSQL 14 and 15;
6. the integration scenarios changed by that PR;
7. an image build without publication.

Nightly or manually triggered CI runs the full chaos matrix because some absence and recovery cases are intentionally long. Test-only environment values shorten absence windows while preserving minimum-probe and state-transition semantics.

Images publish only from a commit whose required test workflow passed. Integration failures must retain container logs, agent views, history records, etcd status, Patroni status, and PostgreSQL control data as artifacts.

## Rollout strategy

1. Deploy API and persisted-history readers/writers without changing decisions.
2. Observe views and history in production while the old recovery policy remains authoritative.
3. Enable self-fencing and remove manual leader-key deletion after its integration suite passes.
4. Run the new decision engine in shadow mode and compare decisions with the existing agent.
5. Enable it first for Flux-removed members and reachable unanimous peers.
6. Enable timeout-based absence only after direct/indirect view and partition tests pass.
7. Keep an environment switch to return to conservative absence policy without changing images.
8. Enable automatic replica re-clone last, after recovery authority verification has production evidence.

## Definition of done

The redesign is complete when:

- ordinary primary-node loss produces a verified writable replacement within 60 seconds;
- persisted-etcd restarts never invoke disaster reconstruction unnecessarily;
- all supported total-DCS-loss and lone-survivor scenarios recover without manual commands;
- Flux-removed nodes reliably self-fence;
- asymmetric visibility is resolved through fresh signed peer views;
- timeout-based absence follows the exact configured interval and is explicitly auditable;
- authority selection validates recoverability and timeline ancestry;
- every recovery phase is resumable and idempotent;
- no automatic path deletes PGDATA before a new primary and recovery epoch are fully verified;
- exactly one writable primary is asserted throughout the integration suite;
- all tests are enforced before image publication;
- any scenario that remains blocked reports the missing evidence and required policy, rather than looping without explanation.

