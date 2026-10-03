package cloudformation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sivchari/kumo/internal/service/cloudformation/template"
)

//nolint:wsl // The test intentionally inspects the immediate persistence snapshot.
func TestJournalEntryIsDurableBeforeAppendReturns(t *testing.T) {
	dir := t.TempDir()
	first := NewMemoryStorage(WithDataDir(dir))
	stack, err := first.CreateStack(t.Context(), &CreateStackRequest{StackName: "journal-sync", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.AppendStackJournalEntry(t.Context(), stack.StackID, StackJournalEntry{Operation: "SideEffect", Status: journalBefore}); err != nil {
		t.Fatal(err)
	}

	second := NewMemoryStorage(WithDataDir(dir))
	loaded, err := second.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range loaded.Journal {
		if entry.Operation == "SideEffect" && entry.Status == journalBefore {
			found = true
		}
	}
	if !found {
		t.Fatalf("loaded journal = %#v, want synchronous side-effect entry", loaded.Journal)
	}
}

//nolint:wsl // The test intentionally coordinates two goroutines around one lease.
func TestRunExclusiveStackOperationSerializesFullCallbacksWithoutStorageMutex(t *testing.T) {
	storage := NewMemoryStorage()
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "exclusive", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- storage.RunExclusiveStackOperation(t.Context(), stack.StackID, func(ctx context.Context) error {
			if _, err := storage.GetStack(ctx, stack.StackID); err != nil {
				return err
			}

			close(started)
			<-release

			return nil
		})
	}()
	<-started
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- storage.RunExclusiveStackOperation(t.Context(), stack.StackID, func(context.Context) error { return nil })
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second exclusive operation completed early: %v", err)
	default:
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

//nolint:wsl // The test distinguishes an active persisted lease from a waitable runtime semaphore.
func TestRunExclusiveStackOperationRejectsActiveLeaseImmediately(t *testing.T) {
	storage := NewMemoryStorage()
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "exclusive-active", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	ctx := context.WithValue(t.Context(), executionTokenContextKey{}, "active-token")
	go func() {
		firstDone <- storage.RunExclusiveStackOperation(ctx, stack.StackID, func(context.Context) error {
			close(started)
			<-release

			return nil
		})
	}()
	<-started

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- storage.RunExclusiveStackOperation(ctx, stack.StackID, func(context.Context) error { return nil })
	}()
	select {
	case err := <-secondDone:
		if err == nil {
			t.Fatal("second exclusive operation succeeded while persisted lease was active")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second exclusive operation waited for the active lease")
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

//nolint:wsl // The test preserves the terminal-save snapshot and reloads its durable lease.
func TestRunExclusiveStackOperationRestoresSnapshotAndRejectsReloadedLease(t *testing.T) {
	dir := t.TempDir()
	storage := NewMemoryStorage(WithDataDir(dir))
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "exclusive-save-failure", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	var persistedLease []byte
	err = storage.RunExclusiveStackOperation(t.Context(), stack.StackID, func(context.Context) error {
		persistedLease, err = os.ReadFile(filepath.Join(dir, "cloudformation.json")) //nolint:gosec // The path is a test-owned temporary directory.
		if err != nil {
			return fmt.Errorf("read persisted lease: %w", err)
		}
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			return fmt.Errorf("remove persistence directory: %w", removeErr)
		}

		return os.WriteFile(dir, []byte("not a directory"), 0o600)
	})
	if err == nil {
		t.Fatal("RunExclusiveStackOperation() error = nil, want terminal save failure")
	}
	got, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OperationLock != nil {
		t.Fatalf("in-memory operation lock = %#v, want restored snapshot", got.OperationLock)
	}
	if removeErr := os.Remove(dir); removeErr != nil {
		t.Fatal(removeErr)
	}
	if mkdirErr := os.Mkdir(dir, 0o750); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	if writeErr := os.WriteFile(filepath.Join(dir, "cloudformation.json"), persistedLease, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	reloaded, err := NewMemoryStorageWithError(WithDataDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	loadedStack, err := reloaded.GetStack(t.Context(), stack.StackID)
	if err != nil || loadedStack.OperationLock == nil {
		t.Fatalf("reloaded operation lock = %#v, error = %v, want persisted lease", loadedStack, err)
	}
	if err := reloaded.RunExclusiveStackOperation(t.Context(), stack.StackID, func(context.Context) error { return nil }); err == nil {
		t.Fatal("RunExclusiveStackOperation() succeeded with a reloaded persisted lease")
	}
}

//nolint:wsl // The test verifies stale persisted leases are not silently overwritten after reload.
func TestRunExclusiveStackOperationRejectsStaleReloadedLease(t *testing.T) {
	dir := t.TempDir()
	stackID := generateStackID("exclusive-stale")
	stack := &Stack{
		StackID:       stackID,
		StackName:     "exclusive-stale",
		StackStatus:   StackStatusCreateComplete,
		OperationLock: &StackLock{Token: "stale-token", Operation: "LifecycleOperation", StartedAt: time.Now().Add(-time.Hour)},
	}
	data, err := json.Marshal(persistedStorage{Stacks: map[string]*Stack{stackID: stack}, LiveNameIndex: map[string]string{stack.StackName: stackID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cloudformation.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	storage, err := NewMemoryStorageWithError(WithDataDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.RunExclusiveStackOperation(t.Context(), stackID, func(context.Context) error { return nil }); err == nil {
		t.Fatal("RunExclusiveStackOperation() succeeded with a stale reloaded lease")
	}
}

//nolint:wsl // The test deliberately coordinates mutation and persistence failure.
func TestRunExclusiveStackOperationRestoresChangeSetsOnSaveFailure(t *testing.T) {
	dir := t.TempDir()
	storage := NewMemoryStorage(WithDataDir(dir))
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "change-set-snapshot", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	changeSet, err := storage.CreateChangeSet(t.Context(), &CreateChangeSetRequest{
		StackName:     stack.StackID,
		ChangeSetName: "snapshot",
		ChangeSetType: operationUpdate,
		TemplateBody:  testEmptyResourcesTemplate,
	}, &template.Evaluation{Resources: map[string]template.Resource{
		testBucketLogicalID: {Type: testS3BucketResourceType},
	}})
	if err != nil {
		t.Fatal(err)
	}
	original, err := storage.GetChangeSet(t.Context(), stack.StackID, changeSet.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = storage.RunExclusiveStackOperation(t.Context(), stack.StackID, func(context.Context) error {
		storage.mu.Lock()
		storage.ChangeSets[changeSet.ID].Status = ChangeSetStatusFailed
		storage.mu.Unlock()
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			return fmt.Errorf("remove persistence directory: %w", removeErr)
		}

		return os.WriteFile(dir, []byte("not a directory"), 0o600)
	})
	if err == nil {
		t.Fatal("RunExclusiveStackOperation() error = nil, want save failure")
	}
	restored, err := storage.GetChangeSet(t.Context(), stack.StackID, changeSet.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != original.Status {
		t.Fatalf("restored change set status = %q, want %q", restored.Status, original.Status)
	}
}

//nolint:wsl // The test deliberately coordinates persistence failure and sibling rollback.
func TestMarkChangeSetExecutionRestoresAllChangeSetsOnSaveFailure(t *testing.T) {
	dir := t.TempDir()
	storage := NewMemoryStorage(WithDataDir(dir))
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "change-set-mark-save", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	evaluation := &template.Evaluation{Resources: map[string]template.Resource{testBucketLogicalID: {Type: testS3BucketResourceType}}}
	first, err := storage.CreateChangeSet(t.Context(), &CreateChangeSetRequest{StackName: stack.StackID, ChangeSetName: testFirstChangeSetName, ChangeSetType: operationUpdate, TemplateBody: testEmptyResourcesTemplate}, evaluation)
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.CreateChangeSet(t.Context(), &CreateChangeSetRequest{StackName: stack.StackID, ChangeSetName: testSecondChangeSetName, ChangeSetType: operationUpdate, TemplateBody: testEmptyResourcesTemplate}, evaluation)
	if err != nil {
		t.Fatal(err)
	}
	if removeErr := os.RemoveAll(dir); removeErr != nil {
		t.Fatal(removeErr)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := storage.MarkChangeSetExecution(t.Context(), stack.StackID, first.ID, true, ""); err == nil {
		t.Fatal("MarkChangeSetExecution() error = nil, want save failure")
	}
	first, err = storage.GetChangeSet(t.Context(), stack.StackID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err = storage.GetChangeSet(t.Context(), stack.StackID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ExecutionStatus != ChangeSetExecutionAvailable || second.Status != ChangeSetStatusCreateComplete || second.ExecutionStatus != ChangeSetExecutionAvailable {
		t.Fatalf("change sets after failed mark = %#v/%#v, want both available", first, second)
	}
}

//nolint:wsl // The test intentionally edits a persisted lock before reload.
func TestCreateChangeSetRejectsReloadedPersistedLease(t *testing.T) {
	dir := t.TempDir()
	storage := NewMemoryStorage(WithDataDir(dir))
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "change-set-lease", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	storage.mu.Lock()
	storage.Stacks[stack.StackID].OperationLock = &StackLock{Token: "persisted-token", Operation: "CreateChangeSet", StartedAt: time.Now()}
	if err := storage.saveLocked(); err != nil {
		storage.mu.Unlock()
		t.Fatal(err)
	}
	storage.mu.Unlock()
	reloaded, err := NewMemoryStorageWithError(WithDataDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	_, err = reloaded.CreateChangeSet(t.Context(), &CreateChangeSetRequest{StackName: stack.StackID, ChangeSetName: "blocked", ChangeSetType: operationUpdate, TemplateBody: testEmptyResourcesTemplate}, &template.Evaluation{Resources: map[string]template.Resource{testBucketLogicalID: {Type: testS3BucketResourceType}}})
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("CreateChangeSet() error = %v, want persisted lease rejection", err)
	}
}
