package cloudformation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sivchari/kumo/internal/service/cloudformation/template"
)

const (
	testStorageEnvironment = "Environment"
	testStorageTier        = "web"
	testStorageValuesKey   = "Values"
	testStorageEventID     = "event-1"
	testStorageMutated     = "mutated"
	testLegacyStackName    = "legacy"
	testStorageOne         = "one"
	testPersistedStacksKey = "stacks"
	testRestoredStackName  = "restored"
)

//nolint:wsl // The test compares storage indexes and independent getter snapshots.
func TestStorageUsesCanonicalARNsAndReturnsSnapshots(t *testing.T) {
	storage := NewMemoryStorage()
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{
		StackName:    testApplicationStackName,
		TemplateBody: testEmptyResourcesTemplate,
		Parameters:   map[string]string{"Original": "value"},
	})
	if err != nil {
		t.Fatalf("CreateStack() error = %v", err)
	}

	if _, ok := storage.Stacks[stack.StackID]; !ok {
		t.Fatalf("Stacks is not keyed by canonical ARN %q: %#v", stack.StackID, storage.Stacks)
	}
	if got := storage.LiveNameIndex[stack.StackName]; got != stack.StackID {
		t.Fatalf("LiveNameIndex[%q] = %q, want %q", stack.StackName, got, stack.StackID)
	}

	stack.Parameters["Changed"] = "outside"
	stack.Resources = append(stack.Resources, StackResource{LogicalResourceID: "outside"})

	got, err := storage.DescribeStacks(t.Context(), stack.StackName)
	if err != nil {
		t.Fatalf("DescribeStacks() error = %v", err)
	}
	if _, ok := got[0].Parameters["Changed"]; ok || len(got[0].Resources) != 0 {
		t.Fatalf("DescribeStacks() returned mutable storage state: %#v", got[0])
	}
}

//nolint:wsl // The test follows deletion through each compatibility getter.
func TestDeleteRetainsARNAddressabilityAndReleasesLiveName(t *testing.T) {
	storage := NewMemoryStorage()
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: testApplicationStackName, TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatalf("CreateStack() error = %v", err)
	}

	if err := storage.DeleteStack(t.Context(), stack.StackName); err != nil {
		t.Fatalf("DeleteStack() error = %v", err)
	}
	if _, ok := storage.LiveNameIndex[stack.StackName]; ok {
		t.Fatalf("deleted stack retained live name index: %#v", storage.LiveNameIndex)
	}

	byARN, err := storage.DescribeStacks(t.Context(), stack.StackID)
	if err != nil {
		t.Fatalf("DescribeStacks(ARN) error = %v", err)
	}
	if got := byARN[0].StackStatus; got != StackStatusDeleteComplete {
		t.Fatalf("deleted stack status = %q, want %q", got, StackStatusDeleteComplete)
	}

	listed, err := storage.ListStacks(t.Context(), []string{StackStatusDeleteComplete})
	if err != nil || len(listed) != 1 || listed[0].StackID != stack.StackID {
		t.Fatalf("ListStacks() = %#v, %v, want retained deleted stack", listed, err)
	}

	replacement, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: stack.StackName, TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatalf("CreateStack(replacement) error = %v", err)
	}
	if replacement.StackID == stack.StackID {
		t.Fatal("replacement reused deleted stack ARN")
	}
}

//nolint:goconst,wsl // The test deliberately arranges the two concurrent operations in phases.
func TestRunStackOperationSerializesCallbacksWithoutHoldingStorageMutex(t *testing.T) {
	storage := NewMemoryStorage()
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: testApplicationStackName, TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatalf("CreateStack() error = %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var orderMu sync.Mutex
	var order []string
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- storage.RunStackOperation(t.Context(), stack.StackID, func(_ context.Context, working *Stack) error {
			orderMu.Lock()
			order = append(order, "first")
			orderMu.Unlock()
			close(started)
			<-release
			working.StackStatusReason = "first"

			return nil
		})
	}()
	<-started

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- storage.RunStackOperation(t.Context(), stack.StackID, func(_ context.Context, working *Stack) error {
			orderMu.Lock()
			order = append(order, "second")
			orderMu.Unlock()
			working.StackStatusReason = "second"

			return nil
		})
	}()

	select {
	case err := <-secondDone:
		t.Fatalf("second operation completed before first release: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first operation error = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second operation error = %v", err)
	}

	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("operation order = %v, want [first second]", order)
	}
}

//nolint:wsl // The callback intentionally calls a getter and writes both history collections.
func TestRunStackOperationCommitsCallbackHistoryAndAllowsGetters(t *testing.T) {
	storage := NewMemoryStorage()
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: testApplicationStackName, TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatalf("CreateStack() error = %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- storage.RunStackOperation(t.Context(), stack.StackID, func(ctx context.Context, working *Stack) error {
			got, err := storage.GetStack(ctx, working.StackID)
			if err != nil {
				return err
			}
			if got.StackID != working.StackID {
				return errors.New("getter returned a different stack")
			}
			working.Journal = append(working.Journal, StackJournalEntry{Revision: working.Revision, Operation: "Callback", Status: "COMMITTED"})
			working.Events = append(working.Events, StackEvent{EventID: "callback-event", StackID: working.StackID, StackName: working.StackName, ResourceStatus: ResourceStatusCreateComplete, Timestamp: time.Unix(2, 0)})

			return nil
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunStackOperation() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunStackOperation() deadlocked while callback called GetStack()")
	}

	got, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatalf("GetStack() error = %v", err)
	}
	if !containsJournalOperation(got.Journal, "Callback") {
		t.Fatalf("committed journal = %#v, want callback entry", got.Journal)
	}
	if !containsEventID(got.Events, "callback-event") {
		t.Fatalf("committed events = %#v, want callback event", got.Events)
	}
}

//nolint:goconst,wsl // The test intentionally performs a complete persistence round trip.
func TestStoragePersistsMetadataAndEvents(t *testing.T) {
	dir := t.TempDir()
	first := NewMemoryStorage(WithDataDir(dir))
	stack, err := first.CreateStack(t.Context(), &CreateStackRequest{StackName: testApplicationStackName, TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatalf("CreateStack() error = %v", err)
	}

	evaluation := &template.Evaluation{
		Parameters: map[string]string{testStorageEnvironment: "test"},
		Conditions: map[string]bool{"Enabled": true},
		Resources: map[string]template.Resource{
			"App": {
				Type: "Example::App",
				Properties: map[string]any{
					"Labels":  map[string]string{"Tier": testStorageTier},
					"Regions": []string{"us-east-1", "us-west-2"},
					"Nested":  []any{map[string]any{testStorageValuesKey: []string{testStorageOne, "two"}}},
				},
				DependsOn: []string{"Network"},
			},
		},
		Outputs: map[string]template.Output{
			"URL": {Value: map[string]string{"Scheme": "https"}},
		},
	}
	if err := first.SetEvaluatedState(t.Context(), stack.StackID, evaluation); err != nil {
		t.Fatalf("SetEvaluatedState() error = %v", err)
	}
	if err := first.AppendStackEvent(t.Context(), stack.StackID, StackEvent{
		EventID:            testStorageEventID,
		StackID:            stack.StackID,
		ClientRequestToken: testRequestToken,
		Timestamp:          time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("AppendStackEvent() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	second := NewMemoryStorage(WithDataDir(dir))
	loaded, err := second.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatalf("GetStack() error = %v", err)
	}
	expectedEvaluation, err := json.Marshal(evaluation)
	if err != nil {
		t.Fatalf("json.Marshal(evaluation) error = %v", err)
	}
	actualEvaluation, err := json.Marshal(loaded.EvaluatedState)
	if err != nil {
		t.Fatalf("json.Marshal(loaded.EvaluatedState) error = %v", err)
	}
	if loaded.Revision == 0 || len(loaded.Journal) == 0 || len(loaded.Events) < 2 || !bytes.Equal(actualEvaluation, expectedEvaluation) {
		t.Fatalf("loaded metadata = %#v", loaded)
	}
	if loaded.Events[len(loaded.Events)-1].EventID != testStorageEventID {
		t.Fatalf("loaded events = %#v, want persisted event details", loaded.Events)
	}
}

//nolint:funlen,wsl // The test verifies every mutable evaluated-state container is isolated at both boundaries.
func TestEvaluatedStateDeepCloneIsolatesNestedMapsAndSlices(t *testing.T) {
	storage := NewMemoryStorage()
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: testApplicationStackName, TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatalf("CreateStack() error = %v", err)
	}
	evaluation := &template.Evaluation{
		Parameters: map[string]string{testStorageEnvironment: "test"},
		Resources: map[string]template.Resource{
			"App": {
				Properties: map[string]any{
					"Labels": map[string]string{"Tier": testStorageTier},
					"List":   []string{"one", "two"},
					"Nested": []any{map[string]any{testStorageValuesKey: []string{"deep"}}},
				},
				DependsOn: []string{"Network"},
			},
		},
		Outputs: map[string]template.Output{
			"URL": {Value: map[string]any{"Tags": map[string]string{"Owner": "team"}, "Values": []string{"a"}}},
		},
	}
	if err := storage.SetEvaluatedState(t.Context(), stack.StackID, evaluation); err != nil {
		t.Fatalf("SetEvaluatedState() error = %v", err)
	}

	evaluation.Parameters[testStorageEnvironment] = testStorageMutated
	labels := mustEvaluatedValue[map[string]string](t, evaluation.Resources["App"].Properties["Labels"])
	labels["Tier"] = testStorageMutated
	list := mustEvaluatedValue[[]string](t, evaluation.Resources["App"].Properties["List"])
	list[0] = testStorageMutated
	nested := mustEvaluatedValue[[]any](t, evaluation.Resources["App"].Properties["Nested"])
	nestedEntry := mustEvaluatedValue[map[string]any](t, nested[0])
	nestedValues := mustEvaluatedValue[[]string](t, nestedEntry[testStorageValuesKey])
	nestedValues[0] = testStorageMutated
	evaluation.Resources["App"] = template.Resource{Properties: map[string]any{"Replaced": true}}

	got, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatalf("GetStack() error = %v", err)
	}
	gotLabels := mustEvaluatedValue[map[string]string](t, got.EvaluatedState.Resources["App"].Properties["Labels"])
	gotLabels["Tier"] = "getter-mutated"
	gotOutput := mustEvaluatedValue[map[string]any](t, got.EvaluatedState.Outputs["URL"].Value)
	gotTags := mustEvaluatedValue[map[string]string](t, gotOutput["Tags"])
	gotTags["Owner"] = "getter-mutated"

	isolated, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatalf("GetStack(second) error = %v", err)
	}
	resource := isolated.EvaluatedState.Resources["App"]
	isolatedLabels := mustEvaluatedValue[map[string]string](t, resource.Properties["Labels"])
	isolatedList := mustEvaluatedValue[[]string](t, resource.Properties["List"])
	isolatedNested := mustEvaluatedValue[[]any](t, resource.Properties["Nested"])
	isolatedNestedEntry := mustEvaluatedValue[map[string]any](t, isolatedNested[0])
	isolatedNestedValues := mustEvaluatedValue[[]string](t, isolatedNestedEntry[testStorageValuesKey])
	if isolatedLabels["Tier"] != testStorageTier || isolatedList[0] != testStorageOne || isolatedNestedValues[0] != "deep" {
		t.Fatalf("stored evaluated resource was not isolated: %#v", resource)
	}
	isolatedOutput := mustEvaluatedValue[map[string]any](t, isolated.EvaluatedState.Outputs["URL"].Value)
	isolatedTags := mustEvaluatedValue[map[string]string](t, isolatedOutput["Tags"])
	if isolatedTags["Owner"] != "team" {
		t.Fatalf("stored evaluated output was not isolated: %#v", isolated.EvaluatedState.Outputs["URL"])
	}
}

//nolint:funlen,wsl // The test runs deletion and repeated same-name creation concurrently.
func TestConcurrentDeleteAndNameReuse(t *testing.T) {
	storage := NewMemoryStorage()
	initial, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "reusable", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatalf("CreateStack(initial) error = %v", err)
	}

	start := make(chan struct{})
	done := make(chan struct{})
	var deleteErr atomic.Value
	var winner atomic.Value
	var once sync.Once
	var waitGroup sync.WaitGroup
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		<-start
		if err := storage.DeleteStack(t.Context(), initial.StackID); err != nil {
			deleteErr.Store(err)
		}
	}()

	for index := 0; index < 16; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			deadline := time.After(time.Second)
			for {
				stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "reusable", TemplateBody: testEmptyResourcesTemplate})
				if err == nil {
					winner.Store(stack)
					once.Do(func() { close(done) })

					return
				}
				var cloudFormationErr *Error
				if !errors.As(err, &cloudFormationErr) || cloudFormationErr.Code != errAlreadyExists {
					t.Errorf("CreateStack(reuse) error = %v", err)

					return
				}
				select {
				case <-done:
					return
				case <-deadline:
					t.Error("timed out waiting for reusable name")

					return
				default:
				}
			}
		}()
	}
	close(start)
	waitGroup.Wait()

	if value := deleteErr.Load(); value != nil {
		t.Fatalf("DeleteStack() error = %v", value)
	}
	if value := winner.Load(); value == nil {
		t.Fatal("no concurrent creator reused the deleted name")
	}
}

//nolint:wsl // The test restores the pre-index name-keyed JSON snapshot shape.
func TestUnmarshalSupportsLegacyNameKeyedSnapshot(t *testing.T) {
	legacyARN := generateStackID("legacy")
	data, err := json.Marshal(map[string]any{
		testPersistedStacksKey: map[string]*Stack{
			testLegacyStackName: {StackID: legacyARN, StackName: testLegacyStackName, TemplateBody: testEmptyResourcesTemplate, StackStatus: StackStatusCreateComplete},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	storage := NewMemoryStorage()
	if err := json.Unmarshal(data, storage); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	got, err := storage.GetStack(t.Context(), testLegacyStackName)
	if err != nil {
		t.Fatalf("GetStack(legacy) error = %v", err)
	}
	if got.StackID != legacyARN || got.StackName != testLegacyStackName {
		t.Fatalf("legacy stack = %#v", got)
	}
}

//nolint:wsl // The test deliberately holds and releases a serialized operation.
func TestRunStackOperationHonorsContextCancellationWhileWaiting(t *testing.T) {
	storage := NewMemoryStorage()
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: testApplicationStackName, TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatalf("CreateStack() error = %v", err)
	}
	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_ = storage.RunStackOperation(t.Context(), stack.StackID, func(_ context.Context, _ *Stack) error {
			close(started)
			<-release

			return nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = storage.RunStackOperation(ctx, stack.StackID, func(_ context.Context, _ *Stack) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunStackOperation(cancelled) error = %v, want context canceled", err)
	}
	close(release)
}

//nolint:wsl // The callback makes persistence fail only at the terminal save boundary.
func TestRunStackOperationRestoresLiveStateWhenTerminalSaveFails(t *testing.T) {
	dir := t.TempDir()
	storage := NewMemoryStorage(WithDataDir(dir))
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "restore-on-save-error", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.RunStackOperation(t.Context(), stack.StackID, func(_ context.Context, working *Stack) error {
		working.StackStatusReason = "unsaved mutation"
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove persistence directory: %w", err)
		}

		return os.WriteFile(dir, []byte("not a directory"), 0o600)
	}); err == nil {
		t.Fatal("RunStackOperation() error = nil, want terminal save failure")
	}
	got, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StackStatusReason != "" || got.OperationLock != nil {
		t.Fatalf("restored live state = %#v, want original reason and no lock", got)
	}
}

//nolint:wsl // The test forces the terminal save to fail after finalization.
func TestRunExclusiveStackOperationRestoresStackAndLiveNameIndexAfterFinalizationSaveFails(t *testing.T) {
	dir := t.TempDir()
	storage := NewMemoryStorage(WithDataDir(dir))
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "restore-finalization", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}

	err = storage.RunExclusiveStackOperation(t.Context(), stack.StackID, func(ctx context.Context) error {
		if err := storage.FinalizeDeletedStack(ctx, stack.StackID, nil, nil, nil); err != nil {
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove persistence directory: %w", err)
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
	if got.StackStatus != StackStatusCreateInProgress || got.OperationLock != nil {
		t.Fatalf("restored stack = %#v, want original status and operation metadata", got)
	}
	if gotARN := storage.LiveNameIndex[stack.StackName]; gotARN != stack.StackID {
		t.Fatalf("restored LiveNameIndex[%q] = %q, want %q", stack.StackName, gotARN, stack.StackID)
	}
}

//nolint:wsl // The test distinguishes malformed records from stale indexes.
func TestUnmarshalRejectsInvalidARNAndRebuildsStaleIndexesWithoutDroppingStacks(t *testing.T) {
	validARN := generateStackID(testRestoredStackName)
	deletedARN := generateStackID("deleted")
	invalid, err := json.Marshal(map[string]any{
		testPersistedStacksKey: map[string]*Stack{
			testRestoredStackName: {StackID: "not-an-arn", StackName: testRestoredStackName, StackStatus: StackStatusCreateComplete},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(invalid, NewMemoryStorage()); err == nil {
		t.Fatal("json.Unmarshal(invalid ARN) error = nil, want persisted record error")
	}

	data, err := json.Marshal(map[string]any{
		testPersistedStacksKey: map[string]*Stack{
			"stale-key":   {StackID: validARN, StackName: testRestoredStackName, StackStatus: StackStatusCreateComplete},
			"deleted-key": {StackID: deletedARN, StackName: "deleted", StackStatus: StackStatusDeleteComplete},
		},
		"liveNameIndex": map[string]string{
			testRestoredStackName: "missing-arn",
			"stale":               validARN,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	storage := NewMemoryStorage()
	if err := json.Unmarshal(data, storage); err != nil {
		t.Fatalf("json.Unmarshal(stale index) error = %v", err)
	}
	got, err := storage.GetStack(t.Context(), testRestoredStackName)
	if err != nil || got.StackID != validARN {
		t.Fatalf("restored stack = %#v, error = %v", got, err)
	}
	if len(storage.LiveNameIndex) != 1 || storage.LiveNameIndex[testRestoredStackName] != validARN {
		t.Fatalf("rebuilt live index = %#v, want only restored entry", storage.LiveNameIndex)
	}
	if _, ok := storage.Stacks[deletedARN]; !ok {
		t.Fatal("stale-index repair dropped deleted stack tombstone")
	}
}

func containsJournalOperation(entries []StackJournalEntry, operation string) bool {
	for index := range entries {
		if entries[index].Operation == operation {
			return true
		}
	}

	return false
}

func containsEventID(events []StackEvent, eventID string) bool {
	for index := range events {
		if events[index].EventID == eventID {
			return true
		}
	}

	return false
}

//nolint:wsl // Type assertion errors are reported with the requested test context.
func mustEvaluatedValue[T any](t *testing.T, value any) T {
	t.Helper()
	result, ok := value.(T)
	if !ok {
		t.Fatalf("evaluated value has type %T, want %T", value, result)
	}

	return result
}
