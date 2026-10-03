# CloudFormation Task 5 Lifecycle Execution Engine Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a synchronous, durable lifecycle executor for evaluated CloudFormation resources without wiring service handlers.

**Architecture:** `Executor` accepts an execution storage seam, adapter registry, evaluated state, capabilities, and failure policies. It builds deterministic dependency plans from evaluated resources, records intent and completion around every adapter side effect, invokes adapters with `context.WithoutCancel`, and commits state/events/exports through storage operations that hold locks only during state work. Existing `resource.Adapter` remains create/delete-only; update behavior is derived from schema property-update metadata.

**Tech Stack:** Go 1.26, existing CloudFormation template/resource/graph/storage/event/export types, standard-library contexts/errors/reflection, Go tests and race detector.

---

### Task 1: Define executor test seam and create lifecycle behavior

**Files:**
- Create: `internal/service/cloudformation/executor_test.go`
- Create: `internal/service/cloudformation/executor.go`
- Modify: `internal/service/cloudformation/storage.go` only if a durable mutation hook is required

**Steps:**
1. Add fake adapters and storage hooks that record calls, contexts, journals, events, and committed snapshots.
2. Add create tests for dependency-first adapter calls, evaluated-state persistence, capability enforcement, unique token-associated events, and journal-before/after ordering.
3. Run the focused create tests and verify they fail because the executor API does not exist.
4. Add the minimal exported executor/request/result types and create implementation.
5. Run the focused create tests and verify they pass.

### Task 2: Add update diff planning and failure semantics

**Files:**
- Modify: `internal/service/cloudformation/executor_test.go`
- Modify: `internal/service/cloudformation/executor.go`

**Steps:**
1. Add tests for no-op evaluated diffs, no-interruption updates, replacement create-before-delete, fixed custom-name replacement rejection, and update rollback restoration.
2. Add a failure test proving a deleted old resource can be recreated from journaled properties during rollback.
3. Run the focused update tests and verify the new cases fail for missing planning/rollback behavior.
4. Implement deterministic evaluated-state diffing, schema classification, replacement orchestration, and journal-backed rollback.
5. Run the focused update tests and verify they pass.

### Task 3: Add delete policies and graph-safe cleanup

**Files:**
- Modify: `internal/service/cloudformation/executor_test.go`
- Modify: `internal/service/cloudformation/executor.go`

**Steps:**
1. Add tests for reverse dependency deletion, Retain, rejected Snapshot, and failed-create rollback options.
2. Add tests proving adapter calls use a non-cancelled context and do not run while a storage lock is held.
3. Run the focused delete/failure tests and verify they fail before implementation.
4. Implement reverse dependency cleanup, Retain skipping, Snapshot validation, and create/update failure policy handling while preserving journaled state.
5. Run the focused lifecycle tests and verify they pass.

### Task 4: Integrate export staging and finish verification

**Files:**
- Modify: `internal/service/cloudformation/executor_test.go`
- Modify: `internal/service/cloudformation/executor.go`
- Modify: `internal/service/cloudformation/storage.go` only when needed for an executor state hook

**Steps:**
1. Add tests proving output exports are staged before commit and are not visible after a failed execution.
2. Implement export staging/commit at the final state boundary and ensure no-op operations still produce the expected completion metadata.
3. Run `gofmt`, `gofumpt`, and `goimports` on modified Go files.
4. Run `go test -race -count=1 ./internal/service/cloudformation/...`.
5. Run `go build ./...` and `make lint`.
6. Inspect `git diff --check` and `git status --short`; confirm no commit was created and prior files remain untouched except explicitly required storage support.
