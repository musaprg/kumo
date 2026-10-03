# CloudFormation Task 2 Quality Findings Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix numeric CloudFormation Ref handling, S3 create cleanup, and mutable resource schemas without touching graph, ARN, event, or storage files.

**Architecture:** Keep conversion and validation at the resource adapter boundary. Use a narrow S3 bucket lifecycle interface so failure-path tests can exercise cleanup without replacing the application service registry, and clone schema containers on every `Schema()` call.

**Tech Stack:** Go, standard library JSON/error handling, existing CloudFormation resource adapters and tests.

---

### Task 1: Numeric CloudFormation Ref Values

**Files:**
- Modify: `internal/service/cloudformation/resource/aws_sqs_queue.go`
- Modify: `internal/service/cloudformation/resource/aws_dynamodb_table.go`
- Test: `internal/service/cloudformation/resource/resource_test.go`

**Steps:**
1. Add tests for strict integer strings in SQS properties and nested redrive policy fields, plus DynamoDB table and GSI provisioned throughput fields.
2. Run the focused tests and confirm failure caused by current type decoding.
3. Implement strict integer parsing and normalize only the DynamoDB throughput fields before typed JSON decoding.
4. Add malformed, fractional, whitespace, overflow, and range-invalid cases and verify rejection.
5. Run the focused resource tests.

### Task 2: S3 Failure Cleanup

**Files:**
- Modify: `internal/service/cloudformation/resource/aws_s3_bucket.go`
- Test: `internal/service/cloudformation/resource/resource_test.go`

**Steps:**
1. Add a fake narrow S3 storage test and a failure-path test for website setup and bucket cleanup error context.
2. Run the test and confirm it fails because the lifecycle helper does not exist.
3. Add the lifecycle cleanup boundary and join cleanup errors with the original setup error.
4. Run the focused S3 test and existing resource tests.

### Task 3: Defensive Schemas

**Files:**
- Modify: `internal/service/cloudformation/resource/resource.go`
- Modify: `internal/service/cloudformation/resource/aws_s3_bucket.go`
- Modify: `internal/service/cloudformation/resource/aws_dynamodb_table.go`
- Modify: `internal/service/cloudformation/resource/aws_sqs_queue.go`
- Modify: `internal/service/cloudformation/resource/aws_sns_topic.go`
- Test: `internal/service/cloudformation/resource/resource_test.go`

**Steps:**
1. Add a mutation-isolation test for schema maps and slices.
2. Run it and confirm package-level schemas are currently mutated.
3. Clone schema maps and slices from each adapter’s `Schema()` method.
4. Run the focused test and the full requested verification commands.
