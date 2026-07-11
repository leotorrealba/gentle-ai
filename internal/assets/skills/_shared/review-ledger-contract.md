# Bounded Review Execution and Ledger Contract

This is the canonical reusable contract for orchestrators, 4R lens reviewers, refuters, scoped validators, and Judgment Day. Generated adapter prompts expand this file; role-specific prompts add only their lens or tool boundary.

## Operation Boundary

Review is explicit `review/start(target)`. The operation receives one complete immutable snapshot, is detached, read-only, and terminal after one result. A reviewer never edits code, launches a correction, starts another reviewer, or owns lifecycle routing. Return one result and terminate.

The parent orchestrator selects zero, one, or four initial lenses from deterministic risk classification. Each selected lens runs exactly one exhaustive sweep. Full 4R means four initial lens sweeps, not extra sweeps or three refuter tasks.

## Orchestration Claims and Native Frozen Ledger

Findings freeze after the initial selected-lens review. Each lens first emits an in-memory orchestration claim with neutral evidence rather than persuasive narrative:

| Field | Values |
|---|---|
| `id` | `{LENS}-{NNN}` |
| `lens` | `risk | readability | reliability | resilience | judgment-day | scoped-fix-validator` |
| `location` | `path/to/file.ext:line` or range |
| `severity` | `BLOCKER | CRITICAL | WARNING | SUGGESTION` |
| `claim` | Neutral statement of observable incorrect behavior |
| `evidence_class` | `deterministic | inferential | insufficient` |
| `proof_refs` | Concrete command, output hash, or `file:line` references |
| `status` | `open | corroborated | refuted | inconclusive | fixed | verified | info` |

The orchestration claim is not the native freeze schema. Before `freeze-findings`, the parent projects each claim to the exact native `Finding` fields `id`, `lens`, `location`, `severity`, `claim`, and `proof_refs`. It MUST NOT serialize `evidence_class` or `status` into the strict native ledger: `evidence_class` is supplied later to `classify-evidence`, while status is derived from authoritative classifications and outcomes. Unknown native finding fields remain rejected.

The native canonical envelope is exact compact JSON with schema `gentle-ai.review-ledger/v1` and an explicit `findings` array containing only those native fields. Finding IDs must already be whitespace-canonical. The canonical empty ledger bytes are exactly `{"schema":"gentle-ai.review-ledger/v1","findings":[]}` with no trailing newline or alternate whitespace. Native `freeze-findings --ledger <path>` reads and validates those bytes before append, derives their SHA-256 identity, rejects a supplied `ledger_hash` mismatch, and requires the envelope findings to exactly match the transaction findings input. Missing or wrong schema, missing findings, malformed or non-canonical JSON, unknown fields, whitespace-padded IDs, and semantic findings mismatch fail before mutation.

Persist an explicit empty ledger when no findings exist. Empty and non-empty ledgers follow the same native lifecycle. WARNING/SUGGESTION rows are `info`; they never drive correction or block approval.

## Evidence Routing

- Deterministic severe findings become `corroborated` with proof and never invoke a refuter.
- Inferential severe findings from every selected lens are merged into exactly ONE detached refuter operation for the transaction. The refuter receives the immutable target plus all neutral claims/proof references and returns one `corroborated | refuted | inconclusive` result per finding.
- Insufficient findings become `inconclusive` and are never auto-fixed.
- Missing, malformed, or incomplete refuter output is `inconclusive`, never implied corroboration.
- Judgment Day's two-judge agreement is its corroboration mechanism; it never launches `review-refuter`. Native state records two distinct blind execution hashes, two distinct result hashes, a confirmation/agreement hash, and exactly two judge executions before findings can freeze.

The refuter is read-only, cannot add findings or change scope, returns one complete result, and terminates. One candidate or twenty candidates consume the same single refuter-batch budget.

## Correction and Scoped Validation

Only the parent orchestrator may launch a correction actor or scoped validator, and only within native transaction counters.

Ordinary review permits at most one correction transaction composed of atomic work units. Each work unit maps exactly to frozen accepted/blocking IDs, changes only the immutable genesis path set, and records focused-test evidence, runtime evidence or justified `N/A`, and an independent rollback boundary. Work-unit count never creates another correction budget.

If correction occurred, ordinary review runs exactly one scoped fix-delta validator. It is detached and read-only, receives only the frozen ledger plus immutable fix delta, verifies original acceptance criteria/tests and correction regression evidence, and can return only `approve` or `escalate`. Later observations are non-blocking follow-ups: they do not alter findings, outcomes, scope, frozen IDs, counters, or correction. A failed original criterion escalates; it never reopens the original diff, launches another correction, or iterates.

Judgment Day replaces ordinary 4R for an explicitly selected target. It alone may run at most two fix rounds and two scoped re-judgments; it does not inherit or extend an ordinary budget.

## Independent Final Verification

Final verification is independent requirements/runtime verification. It checks actual requirements/scenarios, task completion, current test/build evidence, frozen-ledger resolution, snapshot identity, and counter coherence. A contradiction or newly failing deterministic check escalates; it cannot start another 4R, refuter, correction, or scoped-validation loop.

Only `approved | escalated` are terminal transaction states. `scope-changed | invalidated` are lifecycle validation outcomes requiring explicit action.

## Persistence and Lifecycle Gates

OpenSpec mode persists exact machine-readable artifacts:

- `openspec/changes/{change-name}/reviews/transaction.json`
- `openspec/changes/{change-name}/reviews/policy.md`
- `openspec/changes/{change-name}/reviews/ledger.json`
- `openspec/changes/{change-name}/reviews/receipt.json`
- `openspec/changes/{change-name}/reviews/chain-bundle.json`
- `openspec/changes/{change-name}/reviews/gate-context.json`

The sole authoritative append-only CAS state is `<git-common-dir>/gentle-ai/review-transactions/v1/{lineage-id}/`, derived from the canonical validated repository root and a canonical lowercase kebab-case lineage ID. Every Git subprocess uses explicit `git -C <canonical-repo>` and strips inherited repository/worktree/common-dir/index/object/alternate/namespace/shallow/graft/replacement/discovery overrides while preserving ordinary credentials and safe configuration. Linked worktrees retain shared common-dir behavior.

`transaction.json`, chain bundles, and every OpenSpec or Engram artifact are explicitly non-authoritative mirrors. Dispatchers and lifecycle gates load exact HEAD and validate every content-addressed predecessor to one legal `review/start` genesis plus semantic state invariants. Frozen severe findings cannot disappear or lose classification/outcome; pending refuter IDs require one consumed complete batch; corroborated IDs equal correction IDs; corrected candidates require scoped validation; ready/final/approved states have coherent mode counters and no pending severe work. WARNING/SUGGESTION rows remain non-blocking `info`. Missing, cyclic, reordered, regressive, hash-mismatched, semantically incomplete, or standalone terminal chains fail closed.

Writers use a non-blocking cross-platform OS lock with token/PID/host/timestamp observability and crash release. Exact retries repair a linked event or return an already-committed exact HEAD without changing budget; stale predecessors or different content remain rejected. Use `review-resume` after machine-output failure rather than rerunning review work.

<!-- authority-first-terminal-procedure:start -->
### Authority-First Terminal Procedure

Hashes cannot reconstruct policy, ledger, or verification-evidence bytes. Preserve the exact canonical policy and ledger preimages before review starts, and retain them with the exact independent verification-evidence preimage until native validation finishes. Materializing an ephemeral private native input is not mirror persistence.

#### Authoritative Review

| Order | Operation | Required authoritative result | Terminal mirrors |
|---|---|---|---|
| 01 | `preserve-preimages` | Hold exact policy and canonical native ledger bytes; hold selected-lens results in memory. | blocked |
| 02 | `review-start` | Append `review/start`, then run `review-resume` and verify the start revision, lineage, target identity, genesis, and ordered chain identity. | blocked |
| 03 | `record-lens-result` | Execute each selected lens exactly once and append/read back every required canonical result in selected order. | blocked |
| 04 | `freeze-findings` | Materialize ledger bytes only as an ephemeral private input; append/read back the validated canonical ledger and its computed identity. | blocked |
| 05 | `classify-evidence` | Append/read back exact evidence classes and authoritative outcomes for the frozen findings. | blocked |

#### Independent Final Verification

| Order | Operation | Required authoritative result | Terminal mirrors |
|---|---|---|---|
| 06 | `review-resume-preterminal` | Verify lineage, target identity, ordered chain identity, findings, evidence classes, counters, and bound canonical ledger identity. | blocked |
| 07 | `begin-final-verification` | Append/read back final-verification start without requiring a receipt, bundle, gate context, or other terminal-only artifact. | blocked |
| 08 | `independent-final-verification` | Verify requirements and runtime using only the preterminal transaction plus preserved policy and ledger; retain the exact canonical evidence bytes. | blocked |
| 09 | `complete-final-verification` | Hash the retained evidence preimage, append completion, and preserve the same bytes for the gate. | blocked |
| 10 | `review-resume-terminal` | Reload CAS and verify terminal state, revision, genesis, ordered chain identity, counters, and policy/ledger/evidence bindings. | blocked |

#### Terminal Materialization and Gate

| Order | Operation | Required authoritative result | Terminal mirrors |
|---|---|---|---|
| 11 | `review-bundle-export` | Export the validated terminal chain and capture its emitted store revision, genesis revision, chain identity, and bundle digest. The bundle remains non-authoritative transport. | blocked |
| 12 | `extract-terminal-receipt` | Structurally extract and materialize the bundle's natively produced `terminal_receipt`; extraction is allowed but does not authorize a gate. | blocked |
| 13 | `construct-gate-request` | Build `GateRequest` from emitted identities, target identity, and preserved policy/ledger/evidence artifacts or content; hash-check every preimage against terminal bindings. | blocked |
| 14 | `review-validate` | Validate the materialized receipt and request. Native validation derives the repository store and reloads authoritative CAS; it never trusts the bundle or mirrors as authority. | blocked |
| 15 | `reconcile-terminal-mirrors` | Only after `review-validate` returns allow, finalize or reconcile OpenSpec/Engram transaction, ledger, evidence, receipt, bundle, and gate-context mirrors from validated native state. | allowed |

No mirror mutation may precede its confirmed authority boundary; no terminal mirror may finalize before terminal readback and successful native validation. A failed append leaves mirrors at the last confirmed authoritative state. If machine output or post-append readback fails after an append committed, recover with `review-resume`, never `review-start` or a new budget. Empty and non-empty ledgers use the same procedure. Approved or escalated lineages are immutable, and an existing malformed lineage remains invalid and requires an explicit replacement lineage rather than history rewriting.
<!-- authority-first-terminal-procedure:end -->

Engram mode upserts the equivalent exact topics:

- `sdd/{change-name}/review/transaction`
- `sdd/{change-name}/review/policy`
- `sdd/{change-name}/review/ledger`
- `sdd/{change-name}/review/receipt`
- `sdd/{change-name}/review/chain-bundle`
- `sdd/{change-name}/review/gate-context`

Ad-hoc review uses `review/{target-slug}/{transaction|policy|ledger|receipt|chain-bundle|gate-context}`. If no artifact store exists, keep all artifacts inline for the current session and never claim durable receipt reuse.

Post-implementation/post-apply starts ordinary review only when no valid receipt exists. Pre-commit, pre-push, pre-PR, and SDD archive run one native command for the same content-bound receipt; they never hand-author request JSON, create a budget, or silently launch Judgment Day:

```bash
gentle-ai review-validate --cwd <repo> --lineage <id> --gate <post-apply|pre-commit|pre-push|pre-pr|release> --receipt <receipt.json> --bundle <chain-bundle.json> --policy <policy> --ledger <ledger.json> --evidence <verification> [gate-specific artifact flags]
```

Post-apply and pre-commit may add repeatable `--intended-untracked` inputs. Corrected lineages add `--fix-delta <snapshot-json>` whose strict `fix-diff` snapshot bytes reproduce the terminal correction identity; no-fix lineages omit it. Pre-PR derives the publication base from the push remote's advertised default-branch HEAD; `--base-ref` is only an optional expectation and cannot select the boundary. When that base advanced, add `--pre-pr-ci-attestation <signed-json>`. Release adds `--release-configuration`, `--release-generated`, `--release-provenance`, `--release-publication-boundary`, and `--release-evidence-freshness` artifacts. `--request-out` may persist the canonically derived request for audit. Explicit `--request` remains a mutually exclusive compatibility mode, not the prescribed lifecycle path.

Gate context binds expected HEAD, genesis, ordered chain identity, and bundle digest. The validator derives the authoritative store root, verifies the named bundle is the exact authoritative chain, reads every persisted artifact preimage once, derives the current repository target, and binds policy, frozen ledger, fix delta, verify evidence, and release artifacts from those retained bytes; caller-authored store paths, transactions, trees, digests, path sets, favorable states, or chain assertions are never authoritative. For corrected lineages, `--request-out` retains the canonical fix-diff snapshot bytes so explicit replay remains byte-equivalent without changing legacy request semantics. Immediately before any allow the validator re-derives the repository target and configured publication refs. Evaluated `scope-changed`, `invalidated`, and `escalated` results emit machine-readable denial JSON and return non-success; setup and parsing failures remain ordinary CLI errors.

At PRE-PR, a configured push remote's advertised default-branch HEAD is the publication boundary; remote selection follows Git push precedence (`branch.<name>.pushRemote`, `remote.pushDefault`, `branch.<name>.remote`, then `origin`). A stale or unrelated caller-selected local ref is never sufficient proof. Existing explicit requests without a configured publication remote retain offline unchanged-base validation, but cannot authorize an advanced base. An advanced base can reuse the immutable receipt only when native Git derivation proves the original reviewed merge-base tree, unchanged delivered patch and path identities, disjoint old-base-to-new-base paths, and a conflict-free merged result. Moving refs are resolved once to immutable commits and rechecked before authorization. The exact merged-result tree requires successful CI attested by Ed25519: the issuer and public key come from the already receipt-bound policy preimage, either as JSON `pre_pr_ci_trust` fields or `pre_pr_ci_issuer` and `pre_pr_ci_ed25519_public_key` Markdown declarations. `--pre-pr-ci-attestation` only names an artifact containing schema `gentle-ai.pre-pr-ci-attestation/v1`, issuer, merged tree, `success` status, and signature. The signature preimage is `schema NUL issuer NUL merged_tree NUL status NUL`. A success boolean, unsigned artifact, caller-selected key, missing trust root, overlap, conflict, ref movement, changed delivery, or any unprovable condition falls back to the existing fail-closed gate result. Successful reuse emits `base_advanced_compatible` context and never rewrites the receipt.

For clean-clone/workstation recovery, persist the full ordered content-addressed chain bundle. Explicit `review-bundle-import` MUST validate bundle digest, every event hash/predecessor/semantic transition, lineage/generation/mode, initial/final snapshot identities, terminal fix-diff semantics, and expected gate chain identity before CAS installation into the repository-derived destination. Recovery is delivered-content equivalence, not snapshot-structure equivalence: the current derived candidate and delivered path digest MUST match `final_candidate_tree` and the receipt/lineage path scope; the authoritative intended-untracked proof MUST reproduce from that candidate; and policy, ledger, evidence, fix delta, receipt, and chain bindings MUST match exactly. The terminal fix-diff kind, base, ledger IDs, and identity remain preserved for audit, but the recovery snapshot need not share its kind, base, or identity. `review-validate` never auto-imports. Tampered content, wrong path scope/artifacts, truncated or tampered chains, different lineages, arbitrary alternate stores, or unbound bundles remain untrusted and fail closed.

Release from protected `main` has a narrow fast path and does not require a reusable review receipt when all of these are proven: the tag target is the current immutable `origin/main` SHA, required CI for that exact SHA is successful, the remote has not advanced before tag push, and no new vulnerability, policy, provenance, signing, generated-artifact, or release evidence requires escalation. Local branch position and worktree dirtiness are irrelevant because they are not publication inputs. Tag the verified remote SHA explicitly; never infer it from local `HEAD`. Any failed or unprovable condition falls back to native receipt validation and fails closed on missing, changed, invalidated, or escalated state. Major releases and releases following an operational or security incident always require explicit extraordinary review even when the fast-path checks pass.

Outside that protected-`main` fast path, release validation additionally requires an immutable release tree plus content hashes for configuration, generated artifacts, provenance/signing, publication boundary, and evidence freshness. Publication boundary state must be sealed and evidence freshness state must be current; a generic base-relationship context cannot authorize publication.

New CI, vulnerability, base, policy, provenance, or release evidence may invalidate or escalate without reopening unchanged code review. Operational incident separation remains valid only while code, configuration, generated-artifact, and provenance targets are immutable.

## User-Owned Runtime Selection

Model, provider, profile, and effort selection remain optional user choices. This review contract never enforces or silently changes those settings.
