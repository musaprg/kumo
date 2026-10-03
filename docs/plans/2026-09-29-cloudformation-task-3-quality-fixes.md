# CloudFormation Task 3 Quality Fixes Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Close the three Task 3 quality findings while preserving existing resource adapter behavior.

**Architecture:** Make the graph package consume the canonical template evaluator rather than duplicating condition intrinsic semantics. Reject invalid stack names at the storage boundary before generating an ARN, and sort event snapshots at retrieval time by timestamp plus event ID.

**Tech Stack:** Go, `go.yaml.in/yaml/v3`, standard-library sorting, Go tests, race detector, gofmt/gofumpt/goimports, golangci-lint via `make lint`.

---

### Task 1: Canonical Graph Condition Evaluation

**Files:**
- Modify: `internal/service/cloudformation/graph_test.go`
- Modify: `internal/service/cloudformation/template/template.go`
- Modify: `internal/service/cloudformation/graph.go`

**Step 1: Write failing regression tests**

Add graph tests where a condition compares canonical evaluator results involving `Fn::Join`, `Fn::Sub`, and `Fn::ImportValue`, and use `Fn::If` to select a resource dependency. Supply parameters, pseudo parameters, imports, and resource attributes through `template.EvaluationInput`.

**Step 2: Verify red**

Run `go test ./internal/service/cloudformation -run 'TestDependencyGraph.*Condition' -count=1` and confirm the new tests fail because graph condition evaluation does not resolve those intrinsics.

**Step 3: Implement the smallest integration**

Expose a narrow template API that evaluates parsed condition expressions with the same `evaluator` used by `Template.Evaluate`, returning condition values or errors. Replace the graph-local condition-string implementation with that API and retain graph dependency traversal for source expressions.

**Step 4: Verify green**

Run `go test ./internal/service/cloudformation -run 'TestDependencyGraph|TestEvaluate' -count=1` and confirm all targeted tests pass.

### Task 2: Stack Name Validation Before ARN Generation

**Files:**
- Modify: `internal/service/cloudformation/arn_test.go`
- Modify: `internal/service/cloudformation/storage.go`

**Step 1: Write failing tests**

Add valid-name round-trip coverage through generated stack IDs and invalid-name cases through `MemoryStorage.CreateStack`, asserting a validation error and no stored stack. Include names with invalid characters, a leading non-letter, and a length beyond 128 characters.

**Step 2: Verify red**

Run `go test ./internal/service/cloudformation -run 'Test(Stack|Generate|Create)' -count=1` and confirm invalid names currently produce IDs that `ParseStackARN` rejects or are stored unexpectedly.

**Step 3: Implement validation at storage creation**

Call the existing stack-name validation before duplicate checks and before `generateStackID`; return the existing CloudFormation validation error shape. Do not alter resource adapter code or broaden ARN formatting.

**Step 4: Verify green**

Run the same targeted command and confirm valid names round-trip and invalid names are rejected without storage mutation.

### Task 3: Deterministic Event Retrieval Ordering

**Files:**
- Modify: `internal/service/cloudformation/events_test.go`
- Modify: `internal/service/cloudformation/events.go`

**Step 1: Write failing tests**

Append events out of timestamp order and assert `List` returns ascending timestamps. Add equal-timestamp events in non-sorted append order and assert event ID provides stable tie-breaking, including when filtering by request token.

**Step 2: Verify red**

Run `go test ./internal/service/cloudformation -run TestEventLog -count=1` and confirm retrieval follows append order instead of the specified chronological order.

**Step 3: Implement sorting on the copied result**

Sort the result slice by `Timestamp`, then `EventID`, while holding the read lock or after the copy. Preserve deduplication, capacity eviction, and filtering semantics.

**Step 4: Verify green**

Run `go test ./internal/service/cloudformation -run TestEventLog -count=1` and confirm all event tests pass.

### Task 4: Formatting and Full Verification

**Files:**
- Verify only: all modified files above

**Step 1: Format**

Run `gofmt -w` on modified Go files, then `gofumpt -w` and `goimports -w` on the same files.

**Step 2: Run requested verification**

Run `go test -race -count=1 ./internal/service/cloudformation/...`, `go build ./...`, and `make lint`.

**Step 3: Inspect final scope**

Run `git diff --check`, inspect `git status --short`, and confirm no path under `internal/service/cloudformation/resource/` changed and no commit was created.
