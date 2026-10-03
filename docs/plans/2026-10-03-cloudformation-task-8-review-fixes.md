# CloudFormation Task 8 Review Fixes Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix all Task 8 CloudFormation review findings while preserving stale-revision checks, sibling obsolescence, empty-diff behavior, and executor-backed lifecycle execution.

**Architecture:** Keep change-set state in `MemoryStorage` and make each review/execution mutation use the existing persisted stack operation lease. Extend snapshots to include `ChangeSets`, centralize capability/resource preflight through the executor registry, and preserve request-field presence where CloudFormation defaults differ from Go zero values. Extend template summary types/XML conversion from the canonical parsed parameter metadata.

**Tech Stack:** Go 1.26, standard library XML/JSON, existing CloudFormation template evaluator/resource registry/executor, Go tests, race detector.

---

### Task 1: Add failing change-set lifecycle and execution tests

**Files:**
- Modify: `internal/service/cloudformation/change_sets_test.go`
- Modify: `internal/service/cloudformation/executor_test.go`

**Step 1: Write the failing tests**

Add tests that assert UPDATE is rejected on a `REVIEW_IN_PROGRESS` shell while CREATE succeeds, omitted ExecuteChangeSet `DisableRollback` is true and an explicitly supplied false remains false, and a failed execution with the same client token returns the recorded failure rather than rerunning. Add a concurrent/revision test proving CreateChangeSet does not persist a review based on a stack revision changed during evaluation.

**Step 2: Run focused tests**

Run `go test ./internal/service/cloudformation -run 'Test(CreateChangeSet|ExecuteChangeSet)' -count=1` and confirm the new assertions fail for the current behavior.

**Step 3: Implement the minimum handler/request fixes**

Reject UPDATE against review shells, introduce an explicit-presence representation for ExecuteChangeSet `DisableRollback`, and route failed execution results through the existing durable execution-token record so replay returns the original error.

**Step 4: Run focused tests**

Run the same command and confirm the lifecycle tests pass without changing stale-revision, empty-diff, or sibling-obsolescence assertions.

### Task 2: Make CreateChangeSet evaluation revision-safe and preflighted

**Files:**
- Modify: `internal/service/cloudformation/change_sets_test.go`
- Modify: `internal/service/cloudformation/handlers.go`
- Modify: `internal/service/cloudformation/storage.go`
- Modify: `internal/service/cloudformation/executor.go`

**Step 1: Write failing preflight and transform tests**

Add tests for unavailable resource types and insufficient IAM capabilities being stored as failed/unavailable reviews. Add a transform test showing `GetTemplateSummary` reports `CAPABILITY_AUTO_EXPAND`, while CreateChangeSet does not mark unsupported transform execution as available.

**Step 2: Run focused tests**

Run `go test ./internal/service/cloudformation -run 'Test(CreateChangeSet|GetTemplateSummary).*' -count=1` and confirm the tests fail because template parsing rejects Transform before capability reporting and CreateChangeSet bypasses executor preflight.

**Step 3: Implement shared preflight and capability discovery**

Expose a narrow template-summary/parser path that can inspect Transform and parameter metadata without making execution support broader. Add executor preflight access for the production registry and use it while constructing a change set, retaining explicit Transform rejection for execution. Ensure capability discovery checks resource properties and sorts/deduplicates reported capabilities.

**Step 4: Put evaluation and persistence under one lease**

Move stack lookup, template evaluation, revision capture, diff/preflight calculation, and change-set save into one exclusive stack operation. Preserve idempotent token/name handling and reject a stale revision before commit if a separate storage seam remains necessary.

**Step 5: Run focused tests**

Run the focused command again, then `go test ./internal/service/cloudformation -run 'Test(CreateChangeSet|TestGetTemplateSummary)' -count=1` with the full existing change-set and summary files to verify all related behavior.

### Task 3: Make storage snapshots, marking, and leases durable/atomic

**Files:**
- Modify: `internal/service/cloudformation/storage_review_test.go`
- Modify: `internal/service/cloudformation/storage_test.go`
- Modify: `internal/service/cloudformation/storage.go`
- Modify: `internal/service/cloudformation/handlers.go`

**Step 1: Write failing storage tests**

Add a save-failure test that mutates a change set and verifies both stacks and change sets restore from the same pre-mutation snapshot. Add an atomic marking test that forces persistence failure and verifies execution status and sibling obsolescence are unchanged. Add reload/lease tests proving CreateChangeSet and ExecuteChangeSet reject an active persisted lease instead of using only an in-memory semaphore.

**Step 2: Run focused tests**

Run `go test ./internal/service/cloudformation -run 'Test(RunExclusive|MarkChangeSet|CreateChangeSet).*' -count=1` and verify red failures identify missing ChangeSets rollback or lock coverage.

**Step 3: Implement snapshot and lease fixes**

Include cloned `ChangeSets` in `storageSnapshot` and restoration. Resolve and mutate change sets inside the exclusive callback rather than reading a pre-lock clone, and save all terminal state together with rollback on save errors. Ensure CreateChangeSet and ExecuteChangeSet use the persisted `Stack.OperationLock` boundary and preserve lock state through reload.

**Step 4: Run focused tests**

Run the storage-focused command and `go test ./internal/service/cloudformation -run 'Test(CreateChangeSet|ExecuteChangeSet|RunExclusive)' -count=1`.

### Task 4: Expose complete deterministic template summaries

**Files:**
- Modify: `internal/service/cloudformation/template/template.go`
- Modify: `internal/service/cloudformation/types.go`
- Modify: `internal/service/cloudformation/handlers.go`
- Modify: `internal/service/cloudformation/template_summary_test.go`

**Step 1: Write failing summary tests**

Add a template with unsorted parameters and descriptions, AllowedValues, AllowedPattern, Min/MaxLength, and Min/MaxValue. Assert the Go summary and XML response expose all fields in parameter-key order and preserve deterministic constraint ordering. Include Transform capability detection.

**Step 2: Run focused tests**

Run `go test ./internal/service/cloudformation -run 'TestGetTemplateSummary|Test.*Parameter' -count=1` and confirm the new fields are absent or empty.

**Step 3: Implement canonical metadata propagation**

Store parameter description, constraint description, NoEcho, defaults, allowed values/patterns, and numeric/string constraints in the parsed template representation. Populate `TemplateSummaryResult` and XML types from that representation, sort parameters and list fields without mutating shared results, and preserve existing ValidateTemplate output.

**Step 4: Run focused tests**

Run the summary and template package tests and confirm deterministic output.

### Task 5: Format and verify without generation changes

**Files:**
- Verify and format all modified Go files only; do not add Task 9 integration tests.

**Step 1: Format**

Run `gofmt`, `gofumpt`, and `goimports` on modified Go files.

**Step 2: Run requested commands separately with 120-second limits**

Run exactly these as separate commands: `go test -race -count=1 ./internal/service/cloudformation/... ./internal/server/...`, `go build ./...`, `make lint`, `git diff --check`, and `make cli-gen`.

**Step 3: Run post-generation tests/build**

After `make cli-gen`, run the relevant generated-code tests and build, then inspect `git status --short`, `git diff --stat`, and `git diff --check`. Confirm no commit was created and no Task 9 integration tests changed.
