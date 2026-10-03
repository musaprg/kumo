# CloudFormation Task 6 Protected Delete Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Reject termination-protected deletes before lock acquisition or persistence while preserving normal delete serialization and CloudFormation error mapping.

**Architecture:** The delete entry point performs the existing read-only protection check before entering the generic lifecycle runner. The runner continues to acquire the exclusive stack operation for normal delete execution, token reservation, event persistence, and status/index mutations.

**Tech Stack:** Go 1.26, existing CloudFormation executor/storage implementation, Go tests, race detector, and project lint/build tooling.

---

### Task 1: Add the regression test

**Files:**
- Modify: `internal/service/cloudformation/executor_test.go`

**Steps:**
1. Add a storage wrapper that counts `RunExclusiveStackOperation` calls while delegating to persistent `MemoryStorage`.
2. Create a termination-protected stack, snapshot its stack/index/execution state, then make its persistence path fail before calling `Delete`.
3. Assert the delete returns `ValidationError`, the exclusive operation count remains zero, and all captured state is unchanged.
4. Run the focused test and verify it fails because the current code reaches the lock-save failure first.

### Task 2: Move the preflight boundary

**Files:**
- Modify: `internal/service/cloudformation/executor.go`

**Steps:**
1. Run `deletePreflight` before `runLifecycle` in `Executor.Delete`.
2. Remove the generic preflight callback path from `runLifecycle` and keep normal delete execution inside `RunExclusiveStackOperation`.
3. Preserve the wrapped `ValidationError` for handler mapping.
4. Run the focused test and the CloudFormation/server tests.

### Task 3: Verify without committing

**Files:**
- No additional files.

**Steps:**
1. Run `gofmt`, `gofumpt`, and `goimports` on modified Go files where available.
2. Run each required verification command separately with a 120-second timeout.
3. Check `git diff --check` and confirm no commit was created.
