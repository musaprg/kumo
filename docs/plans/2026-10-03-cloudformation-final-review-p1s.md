# CloudFormation Final Review P1s Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix post-side-effect update state, CREATE change-set import context, and conditional import-edge discovery without changing atomic/idempotent semantics.

**Architecture:** The executor will rebuild template evaluations from the final resource attribute map before committing. Change sets will persist immutable import edges alongside their effective template and evaluation. Template import discovery will evaluate conditions and selected branches before collecting export names.

**Tech Stack:** Go, standard testing, YAML template evaluator, in-memory CloudFormation storage.

---

### Task 1: Post-side-effect update state

**Files:**
- Modify: `internal/service/cloudformation/executor_test.go`
- Modify: `internal/service/cloudformation/executor.go`

Write replacement and in-place failing tests first. Verify they fail because committed evaluated properties/outputs still contain pre-side-effect identifiers or attributes. Rebuild the final template evaluation from the post-operation evaluation input, then verify focused executor tests and existing rollback tests.

### Task 2: Change-set import context

**Files:**
- Modify: `internal/service/cloudformation/types.go`
- Modify: `internal/service/cloudformation/storage.go`
- Modify: `internal/service/cloudformation/handlers.go`
- Modify: `internal/service/cloudformation/change_sets_test.go`

Add a failing cross-stack CREATE change-set execution test. Store cloned import edges in the change set and pass them to CREATE and UPDATE lifecycle requests during execution. Verify idempotent replays and sibling obsolescence remain green.

### Task 3: Conditional import discovery

**Files:**
- Modify: `internal/service/cloudformation/template/template.go`
- Modify: `internal/service/cloudformation/template/template_test.go`

Add a failing test proving inactive resources/outputs and unselected `Fn::If` branches do not produce import edges, while selected imports remain. Implement evaluated traversal and verify template, graph, and CloudFormation tests.

### Task 4: Verification

Run `gofmt`, `gofumpt`, and `goimports` on changed Go files, then run each requested command separately with a 120-second command timeout where specified. Inspect `git diff --check` and report any environment or integration gaps exactly.
