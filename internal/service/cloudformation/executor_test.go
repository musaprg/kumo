//nolint:goconst,wsl // Lifecycle fixtures intentionally repeat CloudFormation field names.
package cloudformation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sivchari/kumo/internal/service/cloudformation/resource"
	"github.com/sivchari/kumo/internal/service/cloudformation/template"
)

const (
	executorStackName       = "executor-stack"
	executorResourceType    = "Example::Resource"
	executorSecondType      = "Example::Dependent"
	executorCreateToken     = "create-token"
	executorUpdateToken     = "update-token"
	executorFailureToken    = "failure-token"
	executorInitialPhysical = "old-physical"
	executorSameName        = "same"
	rollbackSecondNewName   = "second-new"
	rollbackRestoredID      = "restored-physical"
	fingerprintLogicalID    = "App"
	deleteByNameRequestID   = "delete-by-name-token"
	executorOldDeleteCall   = "delete:old"
)

//nolint:goconst,wsl // Repeated fixture names make dependency assertions readable.
func TestExecutorCreateUsesRegisteredAdaptersInDependencyOrder(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType, order: &storage.adapterOrder, orderLabel: "Base"}
	dependent := &executorAdapter{typeName: executorSecondType, order: &storage.adapterOrder, orderLabel: "Child"}
	registry.Register(adapter)
	registry.Register(dependent)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	evaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"Base":  {Type: executorResourceType, Properties: map[string]any{"Name": "base"}},
		"Child": {Type: executorSecondType, Properties: map[string]any{"Name": "child"}, DependsOn: []string{"Base"}},
	}}

	if err := executor.Create(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: evaluation,
		Options: ExecutionOptions{
			ClientRequestToken: executorCreateToken,
		},
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if got := append([]string(nil), adapter.calls...); !reflect.DeepEqual(got, []string{"create:base"}) {
		t.Fatalf("base adapter calls = %v, want one create", got)
	}
	if got := append([]string(nil), dependent.calls...); !reflect.DeepEqual(got, []string{"create:child"}) {
		t.Fatalf("dependent adapter calls = %v, want one create", got)
	}
	if got := append([]string(nil), storage.adapterOrder...); !reflect.DeepEqual(got, []string{"Base", "Child"}) {
		t.Fatalf("adapter order = %v, want [Base Child]", got)
	}

	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatalf("GetStack() error = %v", err)
	}
	if stored.EvaluatedState == nil || len(stored.EvaluatedState.Resources) != 2 {
		t.Fatalf("stored evaluated state = %#v, want two resources", stored.EvaluatedState)
	}
	if stored.StackStatus != StackStatusCreateComplete {
		t.Fatalf("stack status = %q, want %q", stored.StackStatus, StackStatusCreateComplete)
	}
}

func TestExecutorTemplateCreateRollsBackAfterLatePlanningFailures(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "unknown resource",
			body: `{"Resources":{"Base":{"Type":"Example::Resource","Properties":{"Name":"base"}},"Unknown":{"Type":"Example::Unknown","Properties":{"Name":"unknown"}}}}`,
		},
		{
			name: "unresolved dependency",
			body: `{"Resources":{"Base":{"Type":"Example::Resource","Properties":{"Name":"base"}},"Broken":{"Type":"Example::Resource","DependsOn":"Missing","Properties":{"Name":"broken"}}}}`,
		},
		{
			name: "evaluation failure",
			body: `{"Resources":{"Base":{"Type":"Example::Resource","Properties":{"Name":"base"}},"Broken":{"Type":"Example::Resource","Properties":{"Name":{"Ref":"Missing"}}}}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storage := newExecutorStorage(t)
			adapter := &executorAdapter{typeName: executorResourceType}
			registry := resource.NewRegistry()
			registry.Register(adapter)
			executor := NewExecutor(storage, registry)
			stack := executorCreateStack(t, storage)
			parsed, err := template.Parse(test.body)
			if err != nil {
				t.Fatal(err)
			}

			err = executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Template: parsed})
			if err == nil {
				t.Fatal("Create() error = nil, want late template failure")
			}
			if !reflect.DeepEqual(adapter.calls, []string{"create:base", "delete:base"}) {
				t.Fatalf("adapter calls = %v, want create followed by rollback delete", adapter.calls)
			}
			stored, getErr := storage.GetStack(t.Context(), stack.StackID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if len(stored.Resources) != 0 {
				t.Fatalf("failed create resources = %#v, want no leaked resources", stored.Resources)
			}
		})
	}
}

//nolint:wsl // The test keeps adapter/storage lock assertions adjacent.
func TestExecutorCreateUsesWholeStackOperationAndLeavesStorageReadableDuringAdapterCall(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	storage.operationActive.Store(false)
	adapter.onCreate = func(ctx context.Context) {
		if _, err := storage.GetStack(ctx, stack.StackID); err != nil {
			t.Errorf("GetStack() from adapter error = %v", err)
		}
		if !storage.operationActive.Load() {
			t.Errorf("adapter call did not run inside the exclusive stack operation")
		}
	}

	if err := executor.Create(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"}),
		Options:    ExecutionOptions{ClientRequestToken: executorCreateToken},
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got := storage.exclusiveCalls.Load(); got != 1 {
		t.Fatalf("exclusive operation calls = %d, want 1", got)
	}
}

//nolint:goconst,wsl // Concurrent lifecycle setup intentionally repeats fixture values.
func TestExecutorSerializesConcurrentCreateOperationsPerStack(t *testing.T) {
	storage := NewMemoryStorage()
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "serialized-executor", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	adapter.onCreate = func(context.Context) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-release

			return
		}
		close(secondStarted)
	}
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: executorEvaluation("First", executorResourceType, map[string]any{"Name": "first"})})
	}()
	<-firstStarted
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: executorEvaluation("Second", executorResourceType, map[string]any{"Name": "second"})})
	}()
	select {
	case <-secondStarted:
		t.Fatal("second adapter call started before first operation completed")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

//nolint:goconst,wsl // Intrinsic dependency fixture names make the order assertion readable.
func TestExecutorCreateOrdersImplicitRefGetAttAndSubDependencies(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	base := &executorAdapter{typeName: executorResourceType, order: &storage.adapterOrder, orderLabel: "ZBase"}
	child := &executorAdapter{typeName: executorSecondType, order: &storage.adapterOrder, orderLabel: "AChild"}
	registry.Register(base)
	registry.Register(child)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	evaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"AChild": {Type: executorSecondType, Properties: map[string]any{
			"RefValue":  map[string]any{"Ref": "ZBase"},
			"Attribute": map[string]any{"Fn::GetAtt": "ZBase.Arn"},
			"SubValue":  map[string]any{"Fn::Sub": "${ZBase}"},
		}},
		"ZBase": {Type: executorResourceType, Properties: map[string]any{"Name": "base"}},
	}}

	if err := executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: evaluation}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got := storage.adapterOrder; !reflect.DeepEqual(got, []string{"ZBase", "AChild"}) {
		t.Fatalf("adapter order = %v, want implicit dependency order", got)
	}
}

//nolint:wsl // Policy rejection setup and assertions are intentionally adjacent.
func TestExecutorRejectsUnknownFailurePolicyAndUsesCloudFormationDoNothingValue(t *testing.T) {
	if FailurePolicyDoNothing != FailurePolicy("DO_NOTHING") {
		t.Fatalf("FailurePolicyDoNothing = %q, want DO_NOTHING", FailurePolicyDoNothing)
	}
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)

	err := executor.Create(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"}),
		Options:    ExecutionOptions{OnFailure: FailurePolicy("UNKNOWN")},
	})
	if err == nil || !strings.Contains(err.Error(), "failure policy") {
		t.Fatalf("Create() error = %v, want unknown policy rejection", err)
	}
	if len(adapter.calls) != 0 {
		t.Fatalf("adapter calls = %v, want no calls after policy rejection", adapter.calls)
	}
	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.StackStatus != StackStatusCreateInProgress || len(stored.Events) != 1 {
		t.Fatalf("invalid policy state = %#v, want unchanged CREATE_IN_PROGRESS state", stored)
	}
}

//nolint:wsl // The test keeps failure state assertions beside option validation.
func TestExecutorRejectsConflictingOnFailureAndDisableRollback(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	registry.Register(&executorAdapter{typeName: executorResourceType})
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)

	err := executor.Create(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"}),
		Options: ExecutionOptions{
			OnFailure:       FailurePolicyRollback,
			DisableRollback: true,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "DisableRollback") {
		t.Fatalf("Create() error = %v, want conflicting option rejection", err)
	}
	stored, getErr := storage.GetStack(t.Context(), stack.StackID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.StackStatus != StackStatusCreateInProgress || len(stored.Events) != 1 {
		t.Fatalf("conflicting option stack = %#v, want unchanged CREATE_IN_PROGRESS state", stored)
	}
}

//nolint:wsl // The test keeps unchanged-state assertions beside the option error.
func TestExecutorValidatesOptionsBeforeExecutionReservation(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	registry.Register(&executorAdapter{typeName: executorResourceType})
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)

	err := executor.Create(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"}),
		Options:    ExecutionOptions{ClientRequestToken: "invalid-before-reservation", OnFailure: FailurePolicy("INVALID")},
	})
	if err == nil || !strings.Contains(err.Error(), "failure policy") {
		t.Fatalf("Create() error = %v, want invalid option error", err)
	}
	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.StackStatus != StackStatusCreateInProgress {
		t.Fatalf("stack status = %q, want unchanged reservation status", stored.StackStatus)
	}
	if _, ok := stored.Executions["invalid-before-reservation"]; ok {
		t.Fatalf("execution records = %#v, want no reservation", stored.Executions)
	}
}

//nolint:wsl // The test keeps tombstone assertions beside the planning failure.
func TestExecutorOnFailureDeleteTombstonesPlanningFailure(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	registry.Register(&executorAdapter{typeName: executorResourceType})
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	evaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"A": {Type: executorResourceType, DependsOn: []string{"B"}},
		"B": {Type: executorResourceType, DependsOn: []string{"A"}},
	}}

	err := executor.Create(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: evaluation,
		Options:    ExecutionOptions{ClientRequestToken: "planning-delete", OnFailure: FailurePolicyDelete},
	})
	if err == nil {
		t.Fatal("Create() error = nil, want dependency planning failure")
	}
	deleted, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.StackStatus != StackStatusDeleteComplete || !containsEventStatus(deleted.Events, StackStatusCreateFailed) || !containsEventStatus(deleted.Events, StackStatusDeleteComplete) {
		t.Fatalf("planning failure stack = %#v, want CREATE_FAILED and DELETE_COMPLETE events", deleted)
	}
}

//nolint:gosec,wsl // The token is a deterministic test identifier, not a credential.
func TestExecutorOnFailureDeleteTombstonesFailedCreate(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	registry.Register(&executorAdapter{typeName: executorResourceType, createErr: errors.New("create failed")})
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)

	err := executor.Create(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"}),
		Options:    ExecutionOptions{OnFailure: FailurePolicyDelete, ClientRequestToken: "delete-on-failure"},
	})
	if err == nil {
		t.Fatal("Create() error = nil, want resource failure")
	}
	deleted, getErr := storage.GetStack(t.Context(), stack.StackID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if deleted.StackStatus != StackStatusDeleteComplete || !containsEventStatus(deleted.Events, StackStatusCreateFailed) {
		t.Fatalf("failed create tombstone = %#v, want DELETE_COMPLETE and CREATE_FAILED event", deleted)
	}
	if _, getErr := storage.DescribeStacks(t.Context(), stack.StackName); getErr == nil {
		t.Fatal("failed create remained addressable by live name")
	}
}

//nolint:goconst,wsl // Policy fixture repeats the values needed for both update cases.
func TestExecutorRejectsSnapshotOnlyForActualReplacement(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType, schema: resource.Schema{TypeName: executorResourceType, PropertyUpdate: map[string]resource.PropertyUpdate{
		"Label":       resource.PropertyUpdate("NoInterruption"),
		"Replacement": resource.PropertyUpdate("Replacement"),
	}}}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	old := executorEvaluation("Base", executorResourceType, map[string]any{"Label": "old"})
	old.Resources["Base"] = template.Resource{Type: executorResourceType, Properties: map[string]any{"Label": "old"}}
	if err := seedExecutorState(t, storage, stack.StackID, old, "old-physical"); err != nil {
		t.Fatal(err)
	}
	if err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Label": "new"})}); err != nil {
		t.Fatalf("non-replacement update error = %v", err)
	}

	old.Resources["Base"] = template.Resource{Type: executorResourceType, Properties: map[string]any{"Label": "old"}, UpdateReplacePolicy: "Snapshot"}
	if err := seedExecutorState(t, storage, stack.StackID, old, "old-physical"); err != nil {
		t.Fatal(err)
	}
	newEvaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Replacement": "new"})
	newEvaluation.Resources["Base"] = template.Resource{Type: executorResourceType, Properties: map[string]any{"Replacement": "new"}, UpdateReplacePolicy: "Snapshot"}
	if err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: newEvaluation}); err == nil || !strings.Contains(err.Error(), "Snapshot") {
		t.Fatalf("replacement update error = %v, want Snapshot rejection", err)
	}
}

//nolint:goconst,wsl // The test isolates the unsupported update contract.
func TestExecutorRejectsSomeInterruptionUpdate(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType, schema: resource.Schema{TypeName: executorResourceType, PropertyUpdate: map[string]resource.PropertyUpdate{
		"Mode": resource.PropertyUpdate("SomeInterruption"),
	}}}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	old := executorEvaluation("Base", executorResourceType, map[string]any{"Mode": "old"})
	if err := seedExecutorState(t, storage, stack.StackID, old, "stable-physical"); err != nil {
		t.Fatal(err)
	}

	err := executor.Update(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Mode": "new"}),
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("Update() error = %v, want unsupported update rejection", err)
	}
	if len(adapter.calls) != 0 {
		t.Fatalf("adapter calls = %v, want no adapter call", adapter.calls)
	}
}

//nolint:goconst,wsl // The test covers both update failure boundaries with one fixture shape.
func TestExecutorFailedUpdatePreservesLastKnownGoodEvaluatedState(t *testing.T) {
	tests := []struct {
		name       string
		registry   func() *resource.Registry
		evaluation *template.Evaluation
	}{
		{
			name: "preflight",
			registry: func() *resource.Registry {
				registry := resource.NewRegistry()
				registry.Register(&executorAdapter{typeName: executorResourceType, schema: resource.Schema{
					TypeName:             executorResourceType,
					RequiredCapabilities: []string{"CAPABILITY_IAM"},
				}})

				return registry
			},
			evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "requested"}),
		},
		{
			name: "execution",
			registry: func() *resource.Registry {
				registry := resource.NewRegistry()
				registry.Register(&executorAdapter{typeName: executorResourceType, createErr: errors.New("update create failed")})

				return registry
			},
			evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "requested"}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storage := newExecutorStorage(t)
			executor := NewExecutor(storage, test.registry())
			stack := executorCreateStack(t, storage)
			knownGood := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "known-good"})
			if err := seedExecutorState(t, storage, stack.StackID, knownGood, "known-good-physical"); err != nil {
				t.Fatal(err)
			}

			err := executor.Update(t.Context(), LifecycleRequest{
				StackRef:   stack.StackID,
				Evaluation: test.evaluation,
				Options:    ExecutionOptions{ClientRequestToken: "failed-update-" + test.name},
			})
			if err == nil {
				t.Fatal("Update() error = nil, want failure")
			}
			stored, getErr := storage.GetStack(t.Context(), stack.StackID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if stored.StackStatus != StackStatusUpdateFailed || stored.EvaluatedState.Resources["Base"].Properties["Name"] != "known-good" {
				t.Fatalf("failed update state = %#v, want known-good evaluated state", stored)
			}
		})
	}
}

//nolint:cyclop,funlen,wsl // This regression covers the complete post-side-effect disabled-rollback state.
func TestExecutorDisableRollbackPreservesSideEffectsAndRetryInventory(t *testing.T) {
	storage := newExecutorStorage(t)
	adapter := newStatefulExecutorAdapter(executorResourceType)
	registry := resource.NewRegistry()
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	oldEvaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"AUpdate": {Type: executorResourceType, Properties: map[string]any{"Label": "old"}},
		"ADelete": {Type: executorResourceType, Properties: map[string]any{"Name": "a-delete"}},
		"BDelete": {Type: executorResourceType, Properties: map[string]any{"Name": "b-delete"}},
	}}
	adapter.seed("a-update-physical", map[string]any{"Label": "old"})
	adapter.seed("a-delete-physical", map[string]any{"Name": "a-delete"})
	adapter.seed("b-delete-physical", map[string]any{"Name": "b-delete"})
	if err := seedExecutorState(t, storage, stack.StackID, oldEvaluation, "a-delete-physical", "a-update-physical", "b-delete-physical"); err != nil {
		t.Fatal(err)
	}
	adapter.failDelete = "a-delete-physical"
	requested := &template.Evaluation{Resources: map[string]template.Resource{
		"AUpdate": {Type: executorResourceType, Properties: map[string]any{"Label": "new"}},
		"Added":   {Type: executorResourceType, Properties: map[string]any{"Name": "added"}},
	}}

	if err := executor.Update(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: requested,
		Options:    ExecutionOptions{ClientRequestToken: "disable-rollback-update", DisableRollback: true},
	}); err == nil {
		t.Fatalf("Update() error = nil, want failed delete; calls = %v", adapter.calls)
	}

	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.StackStatus != StackStatusUpdateFailed {
		t.Fatalf("stack status = %q, want %q", stored.StackStatus, StackStatusUpdateFailed)
	}
	if stored.EvaluatedState.Resources["AUpdate"].Properties["Label"] != "new" || stored.EvaluatedState.Resources["Added"].Properties["Name"] != "added" {
		t.Fatalf("failed evaluated state = %#v, want requested side effects", stored.EvaluatedState)
	}
	if _, ok := stored.EvaluatedState.Resources["ADelete"]; !ok {
		t.Fatal("failed evaluated state omitted live failed deletion")
	}
	resources := stackResourceMap(stored.Resources)
	if _, ok := resources["Added"]; !ok {
		t.Fatalf("failed resource inventory = %#v, want successful create", stored.Resources)
	}
	if _, ok := resources["BDelete"]; ok {
		t.Fatalf("failed resource inventory = %#v, contains successfully deleted resource", stored.Resources)
	}
	if _, ok := resources["ADelete"]; !ok {
		t.Fatalf("failed resource inventory = %#v, omitted failed deletion", stored.Resources)
	}
	if resources["ADelete"].ResourceStatus != ResourceStatusUpdateFailed {
		t.Fatalf("failed deletion resource status = %q, want %q", resources["ADelete"].ResourceStatus, ResourceStatusUpdateFailed)
	}
	if got := adapter.propertiesFor("a-update-physical")["Label"]; got != "new" {
		t.Fatalf("backend update property = %v, want new", got)
	}
	if !adapter.exists("added-physical") || adapter.exists("b-delete-physical") || !adapter.exists("a-delete-physical") {
		t.Fatalf("backend resources = %#v, want added/a-delete only", adapter.resources)
	}

	initialCalls := len(adapter.calls)
	adapter.failDelete = ""
	if err := executor.Update(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: requested,
		Options:    ExecutionOptions{ClientRequestToken: "disable-rollback-retry", DisableRollback: true},
	}); err != nil {
		t.Fatalf("retry Update() error = %v", err)
	}
	if got := len(adapter.calls) - initialCalls; got != 1 || adapter.calls[len(adapter.calls)-1] != "delete:a-delete-physical" {
		t.Fatalf("retry side effects = %v, want one failed deletion retry", adapter.calls[initialCalls:])
	}
	if adapter.exists("a-delete-physical") {
		t.Fatal("retry left failed deletion backend resource")
	}
}

//nolint:wsl // The test keeps the injected persistence failure beside compensation assertions.
func TestExecutorCompensatesSuccessfulCreateWhenAfterJournalPersistenceFails(t *testing.T) {
	storage := newAfterJournalFailureStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage.executorStorage)
	storage.failAfter.Store(true)

	err := executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})})
	if err == nil || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("Create() error = %v, want after-journal failure", err)
	}
	if !reflect.DeepEqual(adapter.calls, []string{"create:base", "delete:base"}) {
		t.Fatalf("adapter calls = %v, want compensation delete", adapter.calls)
	}
}

//nolint:wsl // Empty adapter identifiers must still be represented in compensation state.
func TestExecutorCompensatesSuccessfulEmptyIdentifierWhenAfterJournalPersistenceFails(t *testing.T) {
	storage := newAfterJournalFailureStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType, emptyIdentifier: true}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage.executorStorage)
	storage.failAfter.Store(true)

	err := executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})})
	if err == nil || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("Create() error = %v, want after-journal failure", err)
	}
	if len(adapter.calls) != 2 || !strings.HasPrefix(adapter.calls[1], "delete:Base-") {
		t.Fatalf("adapter calls = %v, want compensation delete using fallback identifier", adapter.calls)
	}
}

//nolint:wsl // Journal assertions follow one operation fixture.
func TestExecutorJournalPreservesAdapterErrorReason(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType, createErr: errors.New("adapter create boom")}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	if err := executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})}); err == nil {
		t.Fatal("Create() error = nil, want adapter failure")
	}
	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range stored.Journal {
		if entry.Status == journalFailed && strings.Contains(entry.Reason, "adapter create boom") {
			found = true
		}
	}
	if !found {
		t.Fatalf("journal = %#v, want adapter error reason", stored.Journal)
	}
}

//nolint:wsl // Event ordering and status assertions stay together.
func TestExecutorFailedSideEffectEventsUseFailedStatus(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType, createErr: errors.New("create failed")}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)

	err := executor.Create(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"}),
		Options:    ExecutionOptions{ClientRequestToken: executorFailureToken},
	})
	if err == nil {
		t.Fatal("Create() error = nil, want adapter failure")
	}
	events, err := storage.ListStackEvents(t.Context(), stack.StackID, executorFailureToken)
	if err != nil {
		t.Fatal(err)
	}
	for index := range events {
		event := events[index]
		if event.LogicalResourceID == "Base" && strings.Contains(event.ResourceStatusReason, "create failed") && event.ResourceStatus == StackStatusCreateInProgress {
			t.Fatalf("failed side-effect event = %#v, must not remain in progress", event)
		}
	}
	if !containsEventStatus(events, StackStatusCreateFailed) {
		t.Fatalf("events = %#v, want CREATE_FAILED event", events)
	}
}

//nolint:goconst,wsl // The fixture deliberately repeats logical IDs and capability values.
func TestExecutorRollbackRestoresExplicitlyEmptyJournalCapabilities(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	first := &capabilityRecordingAdapter{typeName: executorResourceType}
	second := &executorAdapter{typeName: executorSecondType, deleteErr: errors.New("second delete failed")}
	registry.Register(first)
	registry.Register(second)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	old := &template.Evaluation{Resources: map[string]template.Resource{
		"First":  {Type: executorResourceType, Properties: map[string]any{"Name": "old"}, DependsOn: []string{"Second"}},
		"Second": {Type: executorSecondType, Properties: map[string]any{"Name": "second-old"}},
	}}
	if err := seedExecutorState(t, storage, stack.StackID, old, "old-physical", "second-old-physical"); err != nil {
		t.Fatal(err)
	}
	if err := storage.AppendStackJournalEntry(t.Context(), stack.StackID, StackJournalEntry{
		LogicalResourceID:   "First",
		PhysicalResourceID:  "old-physical",
		Identifier:          "old-physical",
		Properties:          map[string]any{"Name": "old"},
		CapabilitiesPresent: true,
	}); err != nil {
		t.Fatal(err)
	}

	err := executor.Update(t.Context(), LifecycleRequest{
		StackRef: stack.StackID,
		Evaluation: &template.Evaluation{Resources: map[string]template.Resource{
			"First": {Type: executorResourceType, Properties: map[string]any{"Name": "new"}},
		}},
		Options: ExecutionOptions{ClientRequestToken: executorFailureToken, Capabilities: []string{"CAPABILITY_IAM"}},
	})
	if err == nil {
		t.Fatal("Update() error = nil, want second create failure")
	}
	if first.rollbackCapabilities == nil {
		t.Fatal("rollback capabilities = nil, want explicitly empty capabilities")
	}
	if len(first.rollbackCapabilities) != 0 {
		t.Fatalf("rollback capabilities = %v, want empty", first.rollbackCapabilities)
	}
}

//nolint:wsl // Table-driven dependency setup keeps each ordering assertion adjacent.
func TestExecutorUpdateCreatesAddedResourcesInExplicitAndImplicitDependencyOrder(t *testing.T) {
	tests := []struct {
		name       string
		properties map[string]any
		dependsOn  []string
	}{
		{name: "explicit", dependsOn: []string{"Base"}},
		{name: "implicit", properties: map[string]any{"Ref": map[string]any{"Ref": "Base"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storage := newExecutorStorage(t)
			registry := resource.NewRegistry()
			base := &executorAdapter{typeName: executorResourceType, order: &storage.adapterOrder, orderLabel: "Base"}
			child := &executorAdapter{typeName: executorSecondType, order: &storage.adapterOrder, orderLabel: "Child"}
			registry.Register(base)
			registry.Register(child)
			executor := NewExecutor(storage, registry)
			stack := executorCreateStack(t, storage)
			if err := seedExecutorState(t, storage, stack.StackID, &template.Evaluation{Resources: map[string]template.Resource{}}); err != nil {
				t.Fatal(err)
			}
			evaluation := &template.Evaluation{Resources: map[string]template.Resource{
				"Base":  {Type: executorResourceType, Properties: map[string]any{"Name": "base"}},
				"Child": {Type: executorSecondType, Properties: test.properties, DependsOn: test.dependsOn},
			}}
			if err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: evaluation}); err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			if !reflect.DeepEqual(storage.adapterOrder, []string{"Base", "Child"}) {
				t.Fatalf("adapter order = %v, want [Base Child]", storage.adapterOrder)
			}
		})
	}
}

//nolint:wsl // The regression assertion intentionally follows the order calculation.
func TestEvaluationOrderIgnoresInactiveIfBranchDependencies(t *testing.T) {
	evaluation := &template.Evaluation{
		Conditions: map[string]bool{"UseDependency": false},
		Resources: map[string]template.Resource{
			"A": {
				Type: "Example::Resource",
				Properties: map[string]any{"Value": map[string]any{"Fn::If": []any{
					"UseDependency", map[string]any{"Ref": "B"}, "literal",
				}}},
			},
			"B": {Type: "Example::Resource", Properties: map[string]any{"Value": map[string]any{"Ref": "A"}}},
		},
	}
	order, err := evaluationOrder(evaluation)
	if err != nil {
		t.Fatalf("evaluationOrder() error = %v, want inactive branch not to create a cycle", err)
	}
	if !reflect.DeepEqual(order, []string{"A", "B"}) {
		t.Fatalf("evaluation order = %v, want [A B]", order)
	}
}

//nolint:wsl // Rollback order setup and assertion are intentionally adjacent.
func TestExecutorRollbackDeletesImplicitDependentsBeforeDependencies(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	first := &executorAdapter{typeName: executorResourceType, order: &storage.adapterOrder, orderLabel: "Base"}
	second := &executorAdapter{typeName: executorSecondType, order: &storage.adapterOrder, orderLabel: "Child", createErr: errors.New("create failed")}
	registry.Register(first)
	registry.Register(second)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	evaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"Base":  {Type: executorResourceType, Properties: map[string]any{"Name": "base"}},
		"Child": {Type: executorSecondType, Properties: map[string]any{"Name": "child", "Ref": map[string]any{"Ref": "Base"}}},
	}}
	if err := executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: evaluation}); err == nil {
		t.Fatal("Create() error = nil, want create failure")
	}
	if !reflect.DeepEqual(storage.adapterOrder, []string{"Base", "Child", "Base"}) {
		t.Fatalf("rollback adapter order = %v, want [Base Child Base]", storage.adapterOrder)
	}
}

//nolint:wsl // Replacement setup and ordering assertions are intentionally adjacent.
func TestExecutorReplacementCreatesAllNewResourcesBeforeReverseCleanup(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType, order: &storage.adapterOrder}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	old := &template.Evaluation{Resources: map[string]template.Resource{
		"Base":  {Type: executorResourceType, Properties: map[string]any{"Name": "base-old"}},
		"Child": {Type: executorResourceType, Properties: map[string]any{"Name": "child-old", "Ref": map[string]any{"Ref": "Base"}}},
	}}
	if err := seedExecutorState(t, storage, stack.StackID, old, "base-old-physical", "child-old-physical"); err != nil {
		t.Fatal(err)
	}
	newEvaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"Base":  {Type: executorResourceType, Properties: map[string]any{"Name": "base-new"}},
		"Child": {Type: executorResourceType, Properties: map[string]any{"Name": "child-new", "Ref": map[string]any{"Ref": "Base"}}},
	}}
	if err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: newEvaluation}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if got := adapter.calls; !reflect.DeepEqual(got, []string{"create:base-new", "create:child-new", "delete:child-old", "delete:base-old"}) {
		t.Fatalf("replacement calls = %v, want create-all then reverse delete", got)
	}
}

//nolint:wsl // Type-change policy setup and assertions are intentionally adjacent.
func TestExecutorTypeChangeRejectsSnapshotReplacementPolicy(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	oldAdapter := &executorAdapter{typeName: executorResourceType}
	newAdapter := &executorAdapter{typeName: executorSecondType}
	registry.Register(oldAdapter)
	registry.Register(newAdapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	old := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "old"})
	old.Resources["Base"] = template.Resource{Type: executorResourceType, Properties: map[string]any{"Name": "old"}}
	if err := seedExecutorState(t, storage, stack.StackID, old, "old-physical"); err != nil {
		t.Fatal(err)
	}
	newEvaluation := executorEvaluation("Base", executorSecondType, map[string]any{"Name": "new"})
	newEvaluation.Resources["Base"] = template.Resource{Type: executorSecondType, Properties: map[string]any{"Name": "new"}, UpdateReplacePolicy: snapshotPolicy}
	if err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: newEvaluation}); err == nil || !strings.Contains(err.Error(), snapshotPolicy) {
		t.Fatalf("Update() error = %v, want type-change Snapshot rejection", err)
	}
	if len(oldAdapter.calls) != 0 || len(newAdapter.calls) != 0 {
		t.Fatalf("adapter calls = old %v, new %v, want no calls", oldAdapter.calls, newAdapter.calls)
	}
}

//nolint:goconst,wsl // Replacement assertions intentionally repeat the fixture values.
func TestExecutorReplacementRetainSkipsOldResourceCleanup(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	old := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "old"})
	old.Resources["Base"] = template.Resource{Type: executorResourceType, Properties: map[string]any{"Name": "old"}}
	if err := seedExecutorState(t, storage, stack.StackID, old, "old-physical"); err != nil {
		t.Fatal(err)
	}
	newEvaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "new"})
	newEvaluation.Resources["Base"] = template.Resource{Type: executorResourceType, Properties: map[string]any{"Name": "new"}, UpdateReplacePolicy: retainPolicy}
	if err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: newEvaluation}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !reflect.DeepEqual(adapter.calls, []string{"create:new"}) {
		t.Fatalf("replacement calls = %v, want retained old resource", adapter.calls)
	}
}

//nolint:goconst,wsl // Compensation inventory assertions intentionally repeat resource names.
func TestExecutorRollbackReportsOnlyLiveFailedCompensationResources(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType, createErrFor: "Broken", deleteErrFor: "Stuck"}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	evaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"Clean":  {Type: executorResourceType, Properties: map[string]any{"Name": "Clean"}},
		"Stuck":  {Type: executorResourceType, Properties: map[string]any{"Name": "Stuck"}},
		"Broken": {Type: executorResourceType, Properties: map[string]any{"Name": "Broken"}, DependsOn: []string{"Clean", "Stuck"}},
	}}
	if err := executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: evaluation}); err == nil {
		t.Fatal("Create() error = nil, want create failure")
	}
	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Resources) != 1 || stored.Resources[0].LogicalResourceID != "Stuck" || stored.Resources[0].PhysicalResourceID != "Stuck-physical" || stored.Resources[0].ResourceStatus != ResourceStatusDeleteFailed {
		t.Fatalf("rollback inventory = %#v, want only live failed compensation resource", stored.Resources)
	}
}

//nolint:wsl // Cancellation and rollback context assertions are one fixture.
func TestExecutorUsesDetachedContextDuringRollback(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	first := &executorAdapter{typeName: executorResourceType}
	second := &executorAdapter{typeName: executorSecondType, createErr: errors.New("create failed")}
	registry.Register(first)
	registry.Register(second)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	ctx, cancel := context.WithCancel(t.Context())
	first.onDelete = func(ctx context.Context) {
		if err := ctx.Err(); err != nil {
			t.Errorf("rollback delete context error = %v, want detached context", err)
		}
	}
	second.onCreate = func(context.Context) { cancel() }
	if err := executor.Create(ctx, LifecycleRequest{StackRef: stack.StackID, Evaluation: &template.Evaluation{Resources: map[string]template.Resource{
		"First":  {Type: executorResourceType, Properties: map[string]any{"Name": "first"}},
		"Second": {Type: executorSecondType, Properties: map[string]any{"Name": "second"}, DependsOn: []string{"First"}},
	}}}); err == nil {
		t.Fatal("Create() error = nil, want create failure")
	}
}

//nolint:wsl // Rejection and duplicate-provisioning assertions are intentionally adjacent.
func TestExecutorRejectsUpdateWithoutStoredEvaluatedState(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})})
	if err == nil || !strings.Contains(err.Error(), "evaluated state") {
		t.Fatalf("Update() error = %v, want missing evaluated state rejection", err)
	}
	if len(adapter.calls) != 0 {
		t.Fatalf("adapter calls = %v, want no duplicate provisioning", adapter.calls)
	}
}

//nolint:wsl // The adapter observation is the core assertion of this test.
func TestExecutorPersistsInProgressStateBeforeAdapterCall(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	adapter.onCreate = func(ctx context.Context) {
		observed, err := storage.GetStack(ctx, stack.StackID)
		if err != nil {
			t.Errorf("GetStack() error = %v", err)

			return
		}
		if observed.StackStatus != StackStatusCreateInProgress || len(observed.Resources) != 1 || observed.Resources[0].ResourceStatus != ResourceStatusCreateInProgress {
			t.Errorf("in-progress state = %#v, want persisted create-in-progress state", observed)
		}
	}
	if err := executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
}

//nolint:wsl // The test keeps load and save failure setup beside both assertions.
func TestMemoryStoragePropagatesLoadAndSaveErrors(t *testing.T) {
	path := t.TempDir() + "/not-a-directory"
	if err := os.WriteFile(path, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMemoryStorageWithError(WithDataDir(path)); err == nil {
		t.Fatal("NewMemoryStorageWithError() error = nil, want load error")
	}

	dir := t.TempDir()
	storage := NewMemoryStorage(WithDataDir(dir))
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "save-error", TemplateBody: testEmptyResourcesTemplate}); err == nil {
		t.Fatal("CreateStack() error = nil, want save error")
	}
}

//nolint:wsl // Terminal transaction failure and event assertions are intentionally adjacent.
func TestExecutorDoesNotReportSuccessWhenTerminalStatePersistenceFails(t *testing.T) {
	storage := newTerminalFailureStorage(t)
	registry := resource.NewRegistry()
	registry.Register(&executorAdapter{typeName: executorResourceType})
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage.executorStorage)
	if err := executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})}); err == nil {
		t.Fatal("Create() error = nil, want terminal persistence failure")
	}
	if !storage.terminalEventSeen {
		t.Fatal("terminal event was not included in the terminal state transaction")
	}
}

//nolint:wsl // Rollback setup and restoration assertions are intentionally adjacent.
func TestExecutorRollbackUsesJournaledCapabilitiesAndRecreatedIdentifier(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	first := &rollbackIdentifierAdapter{typeName: executorResourceType, required: "CAPABILITY_IAM"}
	second := &executorAdapter{typeName: executorSecondType, deleteErr: errors.New("second delete failed")}
	registry.Register(first)
	registry.Register(second)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	old := &template.Evaluation{Resources: map[string]template.Resource{
		"First":  {Type: executorResourceType, Properties: map[string]any{"Name": "old"}, DependsOn: []string{"Second"}},
		"Second": {Type: executorSecondType, Properties: map[string]any{"Name": "second-old"}},
	}}
	if err := seedExecutorState(t, storage, stack.StackID, old, "old-physical", "second-old-physical"); err != nil {
		t.Fatal(err)
	}
	newEvaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"First":  {Type: executorResourceType, Properties: map[string]any{"Name": "new"}},
		"Second": {Type: executorSecondType, Properties: map[string]any{"Name": rollbackSecondNewName}},
	}}
	if err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: newEvaluation, Options: ExecutionOptions{Capabilities: []string{"CAPABILITY_IAM"}}}); err == nil {
		t.Fatal("Update() error = nil, want cleanup failure")
	}
	if !first.rollbackCapabilitySeen {
		t.Fatal("rollback create did not receive journaled capability")
	}
	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range stored.Resources {
		if resource.LogicalResourceID == "First" && resource.PhysicalResourceID != rollbackRestoredID {
			t.Fatalf("restored resource = %#v, want CreateResult identifier", resource)
		}
	}
}

//nolint:wsl // The fixture forces a rollback recreation to fail after its side effect.
func TestExecutorRollbackRecreationRetainsIdentifierAndCompensatesAfterJournalFailure(t *testing.T) {
	storage := newRollbackRecreationJournalFailureStorage(t)
	registry := resource.NewRegistry()
	first := &rollbackRecreationAdapter{typeName: executorResourceType, deleteRestoredErr: errors.New("delete recreated resource failed")}
	second := &executorAdapter{typeName: executorSecondType, deleteErr: errors.New("second delete failed")}
	registry.Register(first)
	registry.Register(second)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage.executorStorage)
	old := &template.Evaluation{Resources: map[string]template.Resource{
		"First":  {Type: executorResourceType, Properties: map[string]any{"Name": "old"}, DependsOn: []string{"Second"}},
		"Second": {Type: executorSecondType, Properties: map[string]any{"Name": "second-old"}},
	}}
	if err := seedExecutorState(t, storage.executorStorage, stack.StackID, old, "old-physical", "second-old-physical"); err != nil {
		t.Fatal(err)
	}
	newEvaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"First":  {Type: executorResourceType, Properties: map[string]any{"Name": "new"}},
		"Second": {Type: executorSecondType, Properties: map[string]any{"Name": rollbackSecondNewName}},
	}}

	err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: newEvaluation, Options: ExecutionOptions{ClientRequestToken: "recreate-journal-failure"}})
	if err == nil || !strings.Contains(err.Error(), "after journal") {
		t.Fatalf("Update() error = %v, want rollback recreation journal failure", err)
	}
	if !slices.Contains(first.calls, "delete:"+rollbackRestoredID) {
		t.Fatalf("recreation calls = %v, want compensation delete", first.calls)
	}
	stored, getErr := storage.GetStack(t.Context(), stack.StackID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	var restored StackResource
	for _, resource := range stored.Resources {
		if resource.LogicalResourceID == "First" {
			restored = resource
		}
	}
	if restored.PhysicalResourceID != rollbackRestoredID || restored.ResourceStatus != ResourceStatusCreateFailed {
		t.Fatalf("restored resource = %#v, want recreated identifier and failed status", restored)
	}
}

//nolint:goconst,wsl // Export failure fixture keeps the final-state assertion local.
func TestExecutorCompensatesExportsWhenFinalStateCommitFails(t *testing.T) {
	storage := newCommitFailureStorage(t)
	registry := resource.NewRegistry()
	registry.Register(&executorAdapter{typeName: executorResourceType})
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage.executorStorage)
	storage.failNextCommit.Store(true)
	evaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})
	evaluation.Outputs = map[string]template.Output{"URL": {Value: "new", ExportName: "NewExport"}}
	if err := executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: evaluation}); err == nil {
		t.Fatal("Create() error = nil, want final state commit failure")
	}
	exports, err := storage.GetExports(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if len(exports) != 0 {
		t.Fatalf("exports = %#v, want compensation after state failure", exports)
	}
}

//nolint:wsl // Failure persistence injection and joined-error assertions are one fixture.
func TestExecutorJoinsFailurePersistenceErrors(t *testing.T) {
	storage := newFailurePersistenceStorage(t)
	registry := resource.NewRegistry()
	registry.Register(&executorAdapter{typeName: executorResourceType, createErr: errors.New("adapter failure")})
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage.executorStorage)
	storage.failState.Store(true)
	err := executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})})
	if err == nil || !strings.Contains(err.Error(), "adapter failure") || !strings.Contains(err.Error(), "state persistence") || !strings.Contains(err.Error(), "event persistence") {
		t.Fatalf("Create() error = %v, want joined operation and persistence errors", err)
	}
	if storage.stateAttempts.Load() == 0 || storage.eventAttempts.Load() == 0 {
		t.Fatalf("failure persistence attempts = state %d, event %d, want both independently attempted", storage.stateAttempts.Load(), storage.eventAttempts.Load())
	}
}

//nolint:wsl // The test keeps token replay and conflicting reuse assertions together.
func TestExecutorPersistsExecutionTokenFingerprintAndReplaysPriorResult(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	request := LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": executorSameName}),
		Options:    ExecutionOptions{ClientRequestToken: "persisted-token"},
	}
	if err := executor.Create(t.Context(), request); err != nil {
		t.Fatalf("initial Create() error = %v", err)
	}
	if err := executor.Create(t.Context(), request); err != nil {
		t.Fatalf("replayed Create() error = %v", err)
	}
	if !reflect.DeepEqual(adapter.calls, []string{"create:" + executorSameName}) {
		t.Fatalf("replayed adapter calls = %v, want one side effect", adapter.calls)
	}
	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if record, ok := stored.Executions["persisted-token"]; !ok || !record.Completed || !record.Success || record.Fingerprint == "" {
		t.Fatalf("execution record = %#v, want completed fingerprinted success", stored.Executions)
	}
	request.Evaluation = executorEvaluation("Base", executorResourceType, map[string]any{"Name": "different"})
	if err := executor.Create(t.Context(), request); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("conflicting token reuse error = %v, want token conflict", err)
	}
	if !reflect.DeepEqual(adapter.calls, []string{"create:same"}) {
		t.Fatalf("conflicting adapter calls = %v, want no additional side effect", adapter.calls)
	}
}

//nolint:wsl // Fingerprint inputs are deliberately equivalent except for slice order.
func TestLifecycleFingerprintSortsDependencySlices(t *testing.T) {
	first := &template.Evaluation{Resources: map[string]template.Resource{
		fingerprintLogicalID: {Type: executorResourceType, DependsOn: []string{"Z", "A"}, Dependencies: []string{"Y", "B"}},
	}}
	second := &template.Evaluation{Resources: map[string]template.Resource{
		fingerprintLogicalID: {Type: executorResourceType, DependsOn: []string{"A", "Z"}, Dependencies: []string{"B", "Y"}},
	}}

	firstFingerprint, err := lifecycleFingerprint("CREATE", LifecycleRequest{StackRef: "stack", Evaluation: first})
	if err != nil {
		t.Fatal(err)
	}
	secondFingerprint, err := lifecycleFingerprint("CREATE", LifecycleRequest{StackRef: "stack", Evaluation: second})
	if err != nil {
		t.Fatal(err)
	}
	if firstFingerprint != secondFingerprint {
		t.Fatalf("fingerprints differ for reordered dependency slices: %q != %q", firstFingerprint, secondFingerprint)
	}
}

//nolint:wsl // The test coordinates two lifecycle calls around one adapter call.
func TestExecutorRejectsMatchingTokenWhileOperationIsInProgress(t *testing.T) {
	storage := NewMemoryStorage()
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "in-progress-token", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	adapter.onCreate = func(context.Context) {
		close(started)
		<-release
	}
	request := LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"}),
		Options:    ExecutionOptions{ClientRequestToken: "in-progress-token"},
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- executor.Create(t.Context(), request) }()
	<-started

	secondDone := make(chan error, 1)
	go func() { secondDone <- executor.Create(t.Context(), request) }()
	select {
	case err := <-secondDone:
		if err == nil || !strings.Contains(err.Error(), "already in progress") {
			t.Fatalf("matching in-progress token error = %v, want immediate token rejection", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("matching token waited for the active lifecycle operation")
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	if !reflect.DeepEqual(adapter.calls, []string{"create:base"}) {
		t.Fatalf("adapter calls = %v, want one side effect", adapter.calls)
	}
}

//nolint:wsl // The test keeps the terminal operation and replay assertion together.
func TestExecutorReplaysCompletedDeleteTokenFromTombstone(t *testing.T) {
	storage := NewMemoryStorage()
	registry := resource.NewRegistry()
	registry.Register(&executorAdapter{typeName: executorResourceType})
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "replay-delete", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	evaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})
	if err := storage.SetEvaluatedState(t.Context(), stack.StackID, evaluation); err != nil {
		t.Fatal(err)
	}
	request := LifecycleRequest{StackRef: stack.StackID, Options: ExecutionOptions{ClientRequestToken: "delete-token"}}
	executor := NewExecutor(storage, registry)
	if err := executor.Delete(t.Context(), request); err != nil {
		t.Fatalf("initial Delete() error = %v", err)
	}
	if err := executor.Delete(t.Context(), request); err != nil {
		t.Fatalf("replayed Delete() error = %v", err)
	}
}

//nolint:wsl // The test covers terminal state commit followed by token persistence failure.
func TestExecutorRecoversUpdateTokenAfterTerminalCompletionPersistenceFailure(t *testing.T) {
	storage := &failingExecutionCompletionStorage{MemoryStorage: NewMemoryStorage(), fail: true}
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "recover-update-token", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	old := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "old"})
	if err := seedExecutorState(t, &executorStorage{MemoryStorage: storage.MemoryStorage}, stack.StackID, old, "old-physical"); err != nil {
		t.Fatal(err)
	}
	request := LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "new"}),
		Options:    ExecutionOptions{ClientRequestToken: "recover-update-token"},
	}
	executor := NewExecutor(storage, registry)
	if err := executor.Update(t.Context(), request); err == nil || !strings.Contains(err.Error(), "completion persistence") {
		t.Fatalf("initial Update() error = %v, want completion persistence failure", err)
	}
	if err := executor.Update(t.Context(), request); err != nil {
		t.Fatalf("replayed Update() error = %v, want committed result replay", err)
	}
	if !reflect.DeepEqual(adapter.calls, []string{"create:new", executorOldDeleteCall}) {
		t.Fatalf("replayed update adapter calls = %v, want one lifecycle execution", adapter.calls)
	}
}

//nolint:wsl // The test covers tombstone commit followed by token persistence failure.
func TestExecutorRecoversDeleteTokenAfterTerminalCompletionPersistenceFailure(t *testing.T) {
	storage := &failingExecutionCompletionStorage{MemoryStorage: NewMemoryStorage(), fail: true}
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "recover-delete-token", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	evaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})
	if err := seedExecutorState(t, &executorStorage{MemoryStorage: storage.MemoryStorage}, stack.StackID, evaluation, "base-physical"); err != nil {
		t.Fatal(err)
	}
	request := LifecycleRequest{StackRef: stack.StackID, Options: ExecutionOptions{ClientRequestToken: "recover-delete-token"}}
	executor := NewExecutor(storage, registry)
	if err := executor.Delete(t.Context(), request); err == nil || !strings.Contains(err.Error(), "completion persistence") {
		t.Fatalf("initial Delete() error = %v, want completion persistence failure", err)
	}
	if err := executor.Delete(t.Context(), request); err != nil {
		t.Fatalf("replayed Delete() error = %v, want committed result replay", err)
	}
	if !reflect.DeepEqual(adapter.calls, []string{"delete:base"}) {
		t.Fatalf("replayed delete adapter calls = %v, want one lifecycle execution", adapter.calls)
	}
}

//nolint:wsl // The test compares every externally visible stack mutation before and after rejection.
func TestExecutorRejectsProtectedDeleteBeforeAnyMutation(t *testing.T) {
	storage := NewMemoryStorage()
	registry := resource.NewRegistry()
	registry.Register(&executorAdapter{typeName: executorResourceType})
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "protected-delete", TemplateBody: testEmptyResourcesTemplate, EnableTerminationProtection: true})
	if err != nil {
		t.Fatal(err)
	}
	before, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	nameIndex := cloneStringMap(storage.LiveNameIndex)
	createExecutions := cloneCreateExecutions(storage.CreateExecutions)
	err = NewExecutor(storage, registry).Delete(t.Context(), LifecycleRequest{StackRef: stack.StackID, Options: ExecutionOptions{ClientRequestToken: "protected-delete-token"}})
	var cfnErr *Error
	if !errors.As(err, &cfnErr) || cfnErr.Code != errInvalidParameter || !strings.Contains(cfnErr.Message, "Termination protection") {
		t.Fatalf("Delete() error = %v, want validation protection error", err)
	}
	after, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(nameIndex, storage.LiveNameIndex) || !reflect.DeepEqual(createExecutions, storage.CreateExecutions) {
		t.Fatalf("protected delete mutated state: before=%#v after=%#v names=%#v createExecutions=%#v", before, after, storage.LiveNameIndex, storage.CreateExecutions)
	}
}

//nolint:wsl // The test keeps lock-save failure setup beside the rejection and persistence assertions.
func TestExecutorRejectsProtectedDeleteBeforeLockSaveFailure(t *testing.T) {
	dir := t.TempDir()
	storage := &exclusiveTrackingStorage{MemoryStorage: NewMemoryStorage(WithDataDir(dir))}
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{
		StackName:                   "protected-delete-save-failure",
		TemplateBody:                testEmptyResourcesTemplate,
		EnableTerminationProtection: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	nameIndex := cloneStringMap(storage.LiveNameIndex)
	createExecutions := cloneCreateExecutions(storage.CreateExecutions)
	persistedBefore, err := os.ReadFile(filepath.Join(dir, "cloudformation.json")) //nolint:gosec // The path is a test-owned temporary directory.
	if err != nil {
		t.Fatal(err)
	}

	badDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(badDir, []byte("save failure"), 0o600); err != nil {
		t.Fatal(err)
	}
	storage.dataDir = badDir

	err = NewExecutor(storage, nil).Delete(t.Context(), LifecycleRequest{
		StackRef: stack.StackID,
		Options:  ExecutionOptions{ClientRequestToken: "protected-delete-save-failure-token"},
	})
	var cfnErr *Error
	if !errors.As(err, &cfnErr) || cfnErr.Code != errInvalidParameter || !strings.Contains(cfnErr.Message, "Termination protection") {
		t.Fatalf("Delete() error = %v, want validation protection error before lock save", err)
	}
	if got := storage.exclusiveCalls.Load(); got != 0 {
		t.Fatalf("exclusive operation calls = %d, want zero before lock acquisition", got)
	}
	after, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(nameIndex, storage.LiveNameIndex) || !reflect.DeepEqual(createExecutions, storage.CreateExecutions) {
		t.Fatalf("protected delete mutated state: before=%#v after=%#v names=%#v createExecutions=%#v", before, after, storage.LiveNameIndex, storage.CreateExecutions)
	}
	persistedAfter, err := os.ReadFile(filepath.Join(dir, "cloudformation.json")) //nolint:gosec // The path is a test-owned temporary directory.
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persistedBefore, persistedAfter) {
		t.Fatal("protected delete persisted an operation lock or lifecycle mutation")
	}
}

//nolint:wsl // The test verifies name resolution remains valid through tombstone cleanup.
func TestExecutorDeleteByNameCompletesTokenAfterTombstoneCleanup(t *testing.T) {
	storage := NewMemoryStorage()
	registry := resource.NewRegistry()
	registry.Register(&executorAdapter{typeName: executorResourceType})
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "delete-by-name", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	evaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})
	if err := storage.RunStackOperation(t.Context(), stack.StackID, func(_ context.Context, working *Stack) error {
		working.EvaluatedState = evaluation
		working.Resources = []StackResource{{LogicalResourceID: "Base", PhysicalResourceID: "base-physical", ResourceType: executorResourceType, ResourceStatus: ResourceStatusCreateComplete, StackID: working.StackID, StackName: working.StackName}}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	token := deleteByNameRequestID
	if err := NewExecutor(storage, registry).Delete(t.Context(), LifecycleRequest{StackRef: stack.StackName, Options: ExecutionOptions{ClientRequestToken: token}}); err != nil {
		t.Fatalf("Delete() by name error = %v", err)
	}
	if _, err := storage.DescribeStacks(t.Context(), stack.StackName); err == nil {
		t.Fatal("deleted stack remained addressable by live name")
	}
	deleted, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if record := deleted.Executions[token]; !record.Completed || !record.Success {
		t.Fatalf("delete execution record = %#v, want completed success", record)
	}
}

//nolint:wsl // The test verifies both the persisted lock during nested mutation and terminal cleanup.
func TestExecutorPreservesOperationLockThroughNestedMutations(t *testing.T) {
	storage := NewMemoryStorage()
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "durable-lock", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	adapter.onCreate = func(ctx context.Context) {
		observed, getErr := storage.GetStack(ctx, stack.StackID)
		if getErr != nil {
			t.Errorf("GetStack() error = %v", getErr)

			return
		}
		if observed.OperationLock == nil {
			t.Error("nested mutation cleared the durable operation lock")
		}
	}
	if err := NewExecutor(storage, registry).Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OperationLock != nil {
		t.Fatalf("terminal operation lock = %#v, want cleared", stored.OperationLock)
	}
}

//nolint:wsl // The test validates preflight and planning failures through one durable failure boundary.
func TestExecutorPersistsPreflightAndPlanningFailures(t *testing.T) {
	tests := []struct {
		name       string
		evaluation *template.Evaluation
		registry   func() *resource.Registry
	}{
		{
			name:       "preflight",
			evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"}),
			registry: func() *resource.Registry {
				registry := resource.NewRegistry()
				registry.Register(&executorAdapter{typeName: executorResourceType, schema: resource.Schema{TypeName: executorResourceType, RequiredCapabilities: []string{"CAPABILITY_IAM"}}})

				return registry
			},
		},
		{
			name: "planning",
			evaluation: &template.Evaluation{Resources: map[string]template.Resource{
				"A": {Type: executorResourceType, DependsOn: []string{"B"}},
				"B": {Type: executorResourceType, DependsOn: []string{"A"}},
			}},
			registry: func() *resource.Registry {
				registry := resource.NewRegistry()
				registry.Register(&executorAdapter{typeName: executorResourceType})

				return registry
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storage := newExecutorStorage(t)
			executor := NewExecutor(storage, test.registry())
			stack := executorCreateStack(t, storage)
			if err := executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: test.evaluation, Options: ExecutionOptions{ClientRequestToken: "failed-" + test.name}}); err == nil {
				t.Fatal("Create() error = nil, want lifecycle failure")
			}
			stored, err := storage.GetStack(t.Context(), stack.StackID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.StackStatus != StackStatusCreateFailed || !containsEventStatus(stored.Events, StackStatusCreateFailed) {
				t.Fatalf("failed lifecycle state = %#v, want CREATE_FAILED state and event", stored)
			}
		})
	}
}

//nolint:wsl // Tombstone assertions follow one complete delete fixture.
func TestExecutorDeleteFinalizesTombstoneAndCleansExports(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	registry.Register(&executorAdapter{typeName: executorResourceType})
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	evaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})
	if err := seedExecutorState(t, storage, stack.StackID, evaluation, "base-physical"); err != nil {
		t.Fatal(err)
	}
	stage, err := storage.StageExports(t.Context(), stack.StackID, []StackExport{{Name: "BaseExport", Value: "base"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.CommitExports(t.Context(), stage); err != nil {
		t.Fatal(err)
	}
	if err := executor.Delete(t.Context(), LifecycleRequest{StackRef: stack.StackID}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := storage.DescribeStacks(t.Context(), stack.StackName); err == nil {
		t.Fatal("deleted stack remained addressable by live name")
	}
	deleted, err := storage.DescribeStacks(t.Context(), stack.StackID)
	if err != nil || len(deleted) != 1 || deleted[0].StackStatus != StackStatusDeleteComplete || len(deleted[0].Exports) != 0 || len(deleted[0].Imports) != 0 {
		t.Fatalf("deleted stack = %#v, error = %v", deleted, err)
	}
}

//nolint:wsl // Partial-delete assertions follow one failure fixture.
func TestExecutorDeletePersistsPartialResourceStatus(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	child := &executorAdapter{typeName: executorSecondType}
	base := &executorAdapter{typeName: executorResourceType, deleteErr: errors.New("base delete failed")}
	registry.Register(base)
	registry.Register(child)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	evaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"Base":  {Type: executorResourceType, Properties: map[string]any{"Name": "base"}},
		"Child": {Type: executorSecondType, Properties: map[string]any{"Name": "child"}, DependsOn: []string{"Base"}},
	}}
	if err := seedExecutorState(t, storage, stack.StackID, evaluation, "base-physical", "child-physical"); err != nil {
		t.Fatal(err)
	}
	if err := executor.Delete(t.Context(), LifecycleRequest{StackRef: stack.StackID}); err == nil {
		t.Fatal("Delete() error = nil, want base deletion failure")
	}
	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[string]string, len(stored.Resources))
	for _, resource := range stored.Resources {
		statuses[resource.LogicalResourceID] = resource.ResourceStatus
	}
	if statuses["Child"] != ResourceStatusDeleteComplete || statuses["Base"] != ResourceStatusDeleteFailed || stored.StackStatus != StackStatusDeleteFailed {
		t.Fatalf("partial delete state = %#v, want child complete and base failed", stored)
	}
}

//nolint:funlen,wsl // Three lifecycle phases are required to verify shared token/event rules.
func TestExecutorUpdateDeleteAndRollbackUseUniqueTokenedEventsAfterCancellation(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)

	updateStack := executorCreateStack(t, storage)
	old := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "old"})
	if err := seedExecutorState(t, storage, updateStack.StackID, old, "old-physical"); err != nil {
		t.Fatal(err)
	}
	updateCtx, updateCancel := context.WithCancel(t.Context())
	adapter.onCreate = func(context.Context) { updateCancel() }
	if err := executor.Update(updateCtx, LifecycleRequest{StackRef: updateStack.StackID, Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": "new"}), Options: ExecutionOptions{ClientRequestToken: "update-cancel-token"}}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	deleteStack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "delete-cancel", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	deleteEvaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "delete"})
	if err := seedExecutorState(t, storage, deleteStack.StackID, deleteEvaluation, "delete-physical"); err != nil {
		t.Fatal(err)
	}
	deleteCtx, deleteCancel := context.WithCancel(t.Context())
	adapter.onDelete = func(context.Context) { deleteCancel() }
	if err := executor.Delete(deleteCtx, LifecycleRequest{StackRef: deleteStack.StackID, Options: ExecutionOptions{ClientRequestToken: "delete-cancel-token"}}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	rollbackStack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "rollback-cancel", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	first := &executorAdapter{typeName: executorResourceType}
	second := &executorAdapter{typeName: executorSecondType, createErr: errors.New("rollback trigger")}
	first.onDelete = func(context.Context) { updateCancel() }
	registry = resource.NewRegistry()
	registry.Register(first)
	registry.Register(second)
	executor = NewExecutor(storage, registry)
	if err := executor.Create(t.Context(), LifecycleRequest{StackRef: rollbackStack.StackID, Evaluation: &template.Evaluation{Resources: map[string]template.Resource{
		"First":  {Type: executorResourceType, Properties: map[string]any{"Name": "first"}},
		"Second": {Type: executorSecondType, Properties: map[string]any{"Name": "second"}, DependsOn: []string{"First"}},
	}}, Options: ExecutionOptions{ClientRequestToken: "rollback-cancel-token"}}); err == nil || !strings.Contains(err.Error(), "rollback trigger") {
		t.Fatalf("Create() rollback error = %v, want trigger error", err)
	}

	for _, fixture := range []struct {
		stackID string
		token   string
	}{
		{updateStack.StackID, "update-cancel-token"},
		{deleteStack.StackID, "delete-cancel-token"},
		{rollbackStack.StackID, "rollback-cancel-token"},
	} {
		events, err := storage.ListStackEvents(t.Context(), fixture.stackID, fixture.token)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]struct{}{}
		for _, event := range events {
			if event.ClientRequestToken != fixture.token {
				t.Fatalf("event token = %q, want %q", event.ClientRequestToken, fixture.token)
			}
			if _, exists := seen[event.EventID]; exists {
				t.Fatalf("duplicate event ID %q", event.EventID)
			}
			seen[event.EventID] = struct{}{}
		}
	}
}

//nolint:wsl // Event assertions intentionally inspect the durable sequence.
func TestExecutorCreateJournalsEverySideEffectAndAssociatesUniqueEvents(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	evaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})

	if err := executor.Create(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: evaluation,
		Options:    ExecutionOptions{ClientRequestToken: executorCreateToken},
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatalf("GetStack() error = %v", err)
	}
	if len(stored.Journal) < 3 {
		t.Fatalf("journal entries = %#v, want operation, before, and after entries", stored.Journal)
	}
	if !containsJournalReason(stored.Journal, "CREATE_IN_PROGRESS") || !containsJournalReason(stored.Journal, "CREATE_COMPLETE") {
		t.Fatalf("journal = %#v, want create before/after records", stored.Journal)
	}
	events, err := storage.ListStackEvents(t.Context(), stack.StackID, executorCreateToken)
	if err != nil {
		t.Fatalf("ListStackEvents() error = %v", err)
	}
	if len(events) < 2 {
		t.Fatalf("events = %#v, want before and after events", events)
	}
	seen := make(map[string]struct{}, len(events))
	for _, event := range events {
		if event.ClientRequestToken != executorCreateToken {
			t.Fatalf("event token = %q, want %q", event.ClientRequestToken, executorCreateToken)
		}
		if _, exists := seen[event.EventID]; exists {
			t.Fatalf("duplicate event ID %q", event.EventID)
		}
		seen[event.EventID] = struct{}{}
	}
	if len(storage.history) < 4 || storage.history[0] != "journal" || storage.history[1] != "event" {
		t.Fatalf("durable side-effect history = %v, want journal/event before adapter", storage.history)
	}
}

//nolint:wsl // Cancellation setup and detached-context assertion intentionally remain together.
func TestExecutorCreatePassesCapabilitiesAndIgnoresCallerCancellationDuringAdapterCall(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{
		typeName: executorResourceType,
		schema: resource.Schema{
			TypeName:             executorResourceType,
			RequiredCapabilities: []string{"CAPABILITY_IAM"},
		},
	}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	ctx, cancel := context.WithCancel(t.Context())
	adapter.onCreate = func(ctx context.Context) {
		cancel()
		if err := ctx.Err(); err != nil {
			t.Errorf("adapter context error = %v, want detached context", err)
		}
	}

	err := executor.Create(ctx, LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: executorEvaluation("Base", executorResourceType, nil),
		Options: ExecutionOptions{
			Capabilities:       []string{"CAPABILITY_NAMED_IAM"},
			ClientRequestToken: executorCreateToken,
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !reflect.DeepEqual(adapter.capabilities, []string{"CAPABILITY_NAMED_IAM"}) {
		t.Fatalf("adapter capabilities = %v, want named IAM", adapter.capabilities)
	}
}

//nolint:wsl // Repeated fixture names make rollback assertions readable.
func TestExecutorCreateRollsBackCreatedResourcesByDefault(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	first := &executorAdapter{typeName: executorResourceType}
	second := &executorAdapter{typeName: executorSecondType, createErr: errors.New("create failed")}
	registry.Register(first)
	registry.Register(second)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	evaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"First":  {Type: executorResourceType, Properties: map[string]any{"Name": "first"}},
		"Second": {Type: executorSecondType, Properties: map[string]any{"Name": "second"}, DependsOn: []string{"First"}},
	}}

	err := executor.Create(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Evaluation: evaluation,
		Options:    ExecutionOptions{ClientRequestToken: executorFailureToken},
	})
	if err == nil || !strings.Contains(err.Error(), "create failed") {
		t.Fatalf("Create() error = %v, want create failure", err)
	}
	if !reflect.DeepEqual(first.calls, []string{"create:first", "delete:first"}) {
		t.Fatalf("first adapter calls = %v, want create then rollback delete", first.calls)
	}
	stored, getErr := storage.GetStack(t.Context(), stack.StackID)
	if getErr != nil {
		t.Fatalf("GetStack() error = %v", getErr)
	}
	if stored.StackStatus != StackStatusCreateFailed {
		t.Fatalf("stack status = %q, want %q", stored.StackStatus, StackStatusCreateFailed)
	}
	if len(stored.Resources) != 0 {
		t.Fatalf("rolled-back resources = %#v, want none", stored.Resources)
	}
}

//nolint:wsl // Repeated fixture names make diff assertions readable.
func TestExecutorUpdateUsesEvaluatedDiffAndCreatesReplacementBeforeDelete(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	oldEvaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "old"})
	if err := seedExecutorState(t, storage, stack.StackID, oldEvaluation, "old-physical"); err != nil {
		t.Fatal(err)
	}
	adapter.calls = nil
	newEvaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "new"})

	if err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: newEvaluation, Options: ExecutionOptions{ClientRequestToken: executorUpdateToken}}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if got := adapter.calls; !reflect.DeepEqual(got, []string{"create:new", executorOldDeleteCall}) {
		t.Fatalf("adapter calls = %v, want replacement calls", got)
	}
	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.StackStatus != StackStatusUpdateComplete || stored.EvaluatedState.Resources["Base"].Properties["Name"] != "new" {
		t.Fatalf("stored update state = %#v", stored)
	}
}

//nolint:wsl // Repeated fixture names make update assertions readable.
func TestExecutorUpdateNoInterruptionChangeDoesNotRecreateResource(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType, schema: resource.Schema{TypeName: executorResourceType, PropertyUpdate: map[string]resource.PropertyUpdate{"Label": resource.PropertyUpdate("NoInterruption")}}}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	old := executorEvaluation("Base", executorResourceType, map[string]any{"Label": "old"})
	if err := seedExecutorState(t, storage, stack.StackID, old, "stable-physical"); err != nil {
		t.Fatal(err)
	}
	adapter.calls = nil

	if err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Label": "new"}), Options: ExecutionOptions{ClientRequestToken: executorUpdateToken}}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !reflect.DeepEqual(adapter.calls, []string{"update:stable-physical:old->new"}) {
		t.Fatalf("adapter calls = %v, want backend update", adapter.calls)
	}
}

//nolint:cyclop,funlen,goconst,wsl // The fixture deliberately repeats old and new state at each rollback boundary.
func TestExecutorUpdateRollbackCompensatesSuccessfulInPlaceUpdate(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	updated := &compensatingUpdateAdapter{typeName: executorResourceType, omitUpdateAttributes: true, schema: resource.Schema{TypeName: executorResourceType, PropertyUpdate: map[string]resource.PropertyUpdate{"Label": resource.PropertyUpdate("NoInterruption")}}}
	laterErr := errors.New("later create failed")
	failed := &executorAdapter{typeName: executorSecondType, createErr: laterErr}
	registry.Register(updated)
	registry.Register(failed)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	oldEvaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Label": "old"})
	if err := seedExecutorState(t, storage, stack.StackID, oldEvaluation, "stable-physical"); err != nil {
		t.Fatal(err)
	}
	if err := storage.AppendStackJournalEntry(t.Context(), stack.StackID, StackJournalEntry{
		Operation:           "UpdateResource",
		Status:              journalAfter,
		LogicalResourceID:   "Base",
		PhysicalResourceID:  "stable-physical",
		Identifier:          "stable-physical",
		ResourceType:        executorResourceType,
		Properties:          map[string]any{"Label": "old"},
		Capabilities:        []string{"CAPABILITY_OLD"},
		CapabilitiesPresent: true,
		Attributes:          map[string]any{"Arn": "arn:old"},
	}); err != nil {
		t.Fatal(err)
	}

	err := executor.Update(t.Context(), LifecycleRequest{
		StackRef: stack.StackID,
		Evaluation: &template.Evaluation{Resources: map[string]template.Resource{
			"Base":   {Type: executorResourceType, Properties: map[string]any{"Label": "new"}},
			"Broken": {Type: executorSecondType, Properties: map[string]any{"Name": "broken"}},
		}},
		Options: ExecutionOptions{ClientRequestToken: executorFailureToken, Capabilities: []string{"CAPABILITY_NEW"}},
	})
	if err == nil || !strings.Contains(err.Error(), "later create failed") || !errors.Is(err, laterErr) {
		t.Fatalf("Update() error = %v, want later create failure", err)
	}
	if !reflect.DeepEqual(updated.properties, map[string]any{"Label": "old"}) {
		t.Fatalf("compensated properties = %#v, want old properties", updated.properties)
	}
	if !reflect.DeepEqual(updated.attributes, map[string]any{"Arn": "arn:old"}) {
		t.Fatalf("compensated attributes = %#v, want old attributes", updated.attributes)
	}
	if len(updated.updateCapabilities) != 2 || !reflect.DeepEqual(updated.updateCapabilities[1], []string{"CAPABILITY_OLD"}) {
		t.Fatalf("update capabilities = %#v, want old capabilities for compensation", updated.updateCapabilities)
	}
	stored, getErr := storage.GetStack(t.Context(), stack.StackID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.EvaluatedState.Resources["Base"].Properties["Label"] != "old" || stored.StackStatus != StackStatusUpdateFailed {
		t.Fatalf("rollback state = %#v, want old evaluated state and update failure", stored)
	}
	if len(stored.Resources) != 1 || stored.Resources[0].LogicalResourceID != "Base" || stored.Resources[0].ResourceStatus != ResourceStatusCreateComplete {
		t.Fatalf("rollback resources = %#v, want only the restored base resource", stored.Resources)
	}
	if !latestJournalHas(t, stored.Journal, "Base", journalAfter, map[string]any{"Label": "old"}, map[string]any{"Arn": "arn:old"}) {
		t.Fatalf("rollback journal = %#v, want successful old-state compensation", stored.Journal)
	}
}

//nolint:goconst,wsl // The fixture keeps joined failure and durable-state assertions together.
func TestExecutorUpdateRollbackReportsFailedInPlaceCompensation(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	restoreErr := errors.New("restore update failed")
	updated := &compensatingUpdateAdapter{typeName: executorResourceType, restoreErr: restoreErr, schema: resource.Schema{TypeName: executorResourceType, PropertyUpdate: map[string]resource.PropertyUpdate{"Label": resource.PropertyUpdate("NoInterruption")}}}
	laterErr := errors.New("later create failed")
	failed := &executorAdapter{typeName: executorSecondType, createErr: laterErr}
	registry.Register(updated)
	registry.Register(failed)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	oldEvaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Label": "old"})
	if err := seedExecutorState(t, storage, stack.StackID, oldEvaluation, "stable-physical"); err != nil {
		t.Fatal(err)
	}
	if err := storage.AppendStackJournalEntry(t.Context(), stack.StackID, StackJournalEntry{
		Operation:           "UpdateResource",
		Status:              journalAfter,
		LogicalResourceID:   "Base",
		PhysicalResourceID:  "stable-physical",
		Identifier:          "stable-physical",
		ResourceType:        executorResourceType,
		Properties:          map[string]any{"Label": "old"},
		Capabilities:        []string{"CAPABILITY_OLD"},
		CapabilitiesPresent: true,
		Attributes:          map[string]any{"Arn": "arn:old"},
	}); err != nil {
		t.Fatal(err)
	}

	err := executor.Update(t.Context(), LifecycleRequest{
		StackRef: stack.StackID,
		Evaluation: &template.Evaluation{Resources: map[string]template.Resource{
			"Base":   {Type: executorResourceType, Properties: map[string]any{"Label": "new"}},
			"Broken": {Type: executorSecondType, Properties: map[string]any{"Name": "broken"}},
		}},
		Options: ExecutionOptions{ClientRequestToken: "failed-compensation", Capabilities: []string{"CAPABILITY_NEW"}},
	})
	if err == nil || !strings.Contains(err.Error(), "later create failed") || !strings.Contains(err.Error(), "restore update failed") || !errors.Is(err, laterErr) || !errors.Is(err, restoreErr) {
		t.Fatalf("Update() error = %v, want joined operation and compensation failures", err)
	}
	stored, getErr := storage.GetStack(t.Context(), stack.StackID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.EvaluatedState.Resources["Base"].Properties["Label"] != "old" {
		t.Fatalf("failed compensation evaluated state = %#v, want old state", stored.EvaluatedState)
	}
	if len(stored.Resources) != 1 || stored.Resources[0].ResourceStatus != ResourceStatusUpdateFailed {
		t.Fatalf("failed compensation resources = %#v, want one UPDATE_FAILED resource", stored.Resources)
	}
	if !latestJournalStatus(stored.Journal, "Base", journalFailed) {
		t.Fatalf("failed compensation journal = %#v, want FAILED entry", stored.Journal)
	}
}

func TestExecutorCreateFeedsAdapterAttributesIntoDependentEvaluation(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	producer := &executorAdapter{typeName: executorResourceType, schema: resource.Schema{TypeName: executorResourceType, Attributes: []string{"Arn"}}}
	consumer := &executorAdapter{typeName: executorSecondType}
	registry.Register(producer)
	registry.Register(consumer)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	parsed, err := template.Parse(`
Resources:
  Producer:
    Type: Example::Resource
    Properties:
      Name: producer
  Consumer:
    Type: Example::Dependent
    Properties:
      Source: !GetAtt Producer.Arn
`)
	if err != nil {
		t.Fatal(err)
	}

	if err := executor.Create(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Template:   parsed,
		Parameters: map[string]string{},
		Options:    ExecutionOptions{ClientRequestToken: executorCreateToken},
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got := consumer.lastProperties["Source"]; got != "arn:producer" {
		t.Fatalf("dependent source = %#v, want arn:producer", got)
	}
}

//nolint:wsl // Replacement and addition assertions intentionally share one planning fixture.
func TestExecutorUpdateEvaluatesDependentsAgainstCurrentReplacementIDs(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	oldEvaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "old"})
	if err := seedExecutorState(t, storage, stack.StackID, oldEvaluation, "old-physical"); err != nil {
		t.Fatal(err)
	}
	parsed, err := template.Parse(`
Resources:
  Base:
    Type: Example::Resource
    Properties:
      Name: new
  Added:
    Type: Example::Resource
    Properties:
      Name: !Ref Base
`)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := parsed.Evaluate(template.EvaluationInput{ResourceAttributes: map[string]map[string]string{"Base": {"Ref": "old-physical"}}})
	if err != nil {
		t.Fatal(err)
	}

	err = executor.Update(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Template:   parsed,
		Evaluation: planned,
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !reflect.DeepEqual(adapter.calls, []string{"create:new", "create:new-physical", "delete:old"}) {
		t.Fatalf("adapter calls = %v, want dependent create to use new physical ID", adapter.calls)
	}
}

//nolint:wsl // The fixture verifies import graph replacement and updated property evaluation together.
func TestExecutorUpdateResolvesChangedImportValueFromDesiredGraph(t *testing.T) {
	storage := NewMemoryStorage()
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	exporter, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "updated-import-exporter", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.RunStackOperation(t.Context(), exporter.StackID, func(_ context.Context, working *Stack) error {
		working.StackStatus = StackStatusCreateComplete
		working.Exports = []StackExport{{Name: "NewExport", Value: "new-value", ExporterStackARN: working.StackID}}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "updated-import", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.RunStackOperation(t.Context(), stack.StackID, func(_ context.Context, working *Stack) error {
		working.StackStatus = StackStatusCreateComplete
		working.Imports = []StackImport{{ExportName: "OldExport", ImporterStackARN: working.StackID}}
		working.EvaluatedState = executorEvaluation("Base", executorResourceType, map[string]any{"Name": "old-value"})
		working.Resources = []StackResource{{LogicalResourceID: "Base", PhysicalResourceID: "old-physical", ResourceType: executorResourceType, ResourceStatus: ResourceStatusCreateComplete, StackID: working.StackID, StackName: working.StackName}}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	parsed, err := template.Parse(`
Resources:
  Base:
    Type: Example::Resource
    Properties:
      Name: !ImportValue NewExport
`)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := parsed.Evaluate(template.EvaluationInput{Imports: map[string]string{"NewExport": "new-value"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := executor.Update(t.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Template:   parsed,
		Evaluation: planned,
		Imports:    []StackImport{{ExportName: "NewExport"}},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !reflect.DeepEqual(adapter.calls, []string{"create:new-value", "delete:old"}) {
		t.Fatalf("adapter calls = %v, want desired import value in replacement", adapter.calls)
	}
}

//nolint:wsl // Replacement state assertions intentionally cover the full committed evaluation.
func TestExecutorUpdateCommitsReplacementSideEffectState(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	producer := &executorAdapter{typeName: executorResourceType, schema: resource.Schema{TypeName: executorResourceType, Attributes: []string{"Arn"}, PropertyUpdate: map[string]resource.PropertyUpdate{"Name": resource.PropertyUpdate("Replacement")}}}
	consumer := &executorAdapter{typeName: executorSecondType}
	registry.Register(producer)
	registry.Register(consumer)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	oldEvaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "old"})
	oldEvaluation.Resources["Consumer"] = template.Resource{Type: executorSecondType, Properties: map[string]any{"Source": "old-physical"}, DependsOn: []string{"Base"}}
	if err := seedExecutorState(t, storage, stack.StackID, oldEvaluation, "old-physical", "consumer-physical"); err != nil {
		t.Fatal(err)
	}
	parsed, err := template.Parse(`
Resources:
  Base:
    Type: Example::Resource
    Properties:
      Name: new
  Consumer:
    Type: Example::Dependent
    Properties:
      Source: !Ref Base
    DependsOn: Base
Outputs:
  ARN:
    Value: !GetAtt Base.Arn
    Export:
      Name: ReplacementExport
`)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := parsed.Evaluate(template.EvaluationInput{ResourceAttributes: map[string]map[string]string{"Base": {"Ref": "old-physical", "Arn": "arn:old"}, "Consumer": {"Ref": "consumer-physical"}}})
	if err != nil {
		t.Fatal(err)
	}

	if err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Template: parsed, Evaluation: planned}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Resources[0].PhysicalResourceID; got != "new-physical" {
		t.Fatalf("replacement physical ID = %q, want new-physical", got)
	}
	if got := stored.EvaluatedState.Resources["Consumer"].Properties["Source"]; got != "new-physical" {
		t.Fatalf("committed dependent Ref = %#v, want new-physical", got)
	}
	if got := stored.EvaluatedState.Outputs["ARN"].Value; got != "arn:new" {
		t.Fatalf("committed output = %#v, want arn:new", got)
	}
	if len(stored.Exports) != 1 || stored.Exports[0].Value != "arn:new" {
		t.Fatalf("committed exports = %#v, want post-replacement output", stored.Exports)
	}
}

//nolint:wsl // In-place state assertions intentionally cover post-update attributes and dependent refs.
func TestExecutorUpdateCommitsInPlaceSideEffectAttributes(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	base := &compensatingUpdateAdapter{typeName: executorResourceType, schema: resource.Schema{TypeName: executorResourceType, Attributes: []string{"Arn"}, PropertyUpdate: map[string]resource.PropertyUpdate{"Label": resource.PropertyUpdate("NoInterruption")}}}
	consumer := &executorAdapter{typeName: executorSecondType}
	registry.Register(base)
	registry.Register(consumer)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	oldEvaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Label": "old"})
	oldEvaluation.Resources["Consumer"] = template.Resource{Type: executorSecondType, Properties: map[string]any{"Source": "arn:old"}, DependsOn: []string{"Base"}}
	if err := seedExecutorState(t, storage, stack.StackID, oldEvaluation, "stable-physical", "consumer-physical"); err != nil {
		t.Fatal(err)
	}
	if err := storage.AppendStackJournalEntry(t.Context(), stack.StackID, StackJournalEntry{Operation: "CreateResource", Status: journalAfter, LogicalResourceID: "Base", PhysicalResourceID: "stable-physical", Identifier: "stable-physical", Attributes: map[string]any{"Arn": "arn:old"}}); err != nil {
		t.Fatal(err)
	}
	parsed, err := template.Parse(`
Resources:
  Base:
    Type: Example::Resource
    Properties:
      Label: new
  Consumer:
    Type: Example::Dependent
    Properties:
      Source: !GetAtt Base.Arn
    DependsOn: Base
`)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := parsed.Evaluate(template.EvaluationInput{ResourceAttributes: map[string]map[string]string{"Base": {"Ref": "stable-physical", "Arn": "arn:old"}, "Consumer": {"Ref": "consumer-physical"}}})
	if err != nil {
		t.Fatal(err)
	}

	if err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Template: parsed, Evaluation: planned}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.EvaluatedState.Resources["Consumer"].Properties["Source"]; got != "arn:new" {
		t.Fatalf("committed dependent GetAtt = %#v, want arn:new", got)
	}
}

//nolint:wsl // The test keeps partial mutation and compensation assertions adjacent.
func TestExecutorCompensatesUpdateThatMutatesBeforeReturningError(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &partialUpdateAdapter{typeName: executorResourceType, schema: resource.Schema{TypeName: executorResourceType, PropertyUpdate: map[string]resource.PropertyUpdate{
		"Label": resource.PropertyUpdate("NoInterruption"),
	}}}
	later := &executorAdapter{typeName: executorSecondType, createErr: errors.New("later create failed")}
	registry.Register(adapter)
	registry.Register(later)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	old := executorEvaluation("Base", executorResourceType, map[string]any{"Label": "old"})
	if err := seedExecutorState(t, storage, stack.StackID, old, "stable-physical"); err != nil {
		t.Fatal(err)
	}

	err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: &template.Evaluation{Resources: map[string]template.Resource{
		"Base":   {Type: executorResourceType, Properties: map[string]any{"Label": "new"}},
		"Broken": {Type: executorSecondType, Properties: map[string]any{"Name": "broken"}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "partial update failed") {
		t.Fatalf("Update() error = %v, want partial update failure", err)
	}
	if !reflect.DeepEqual(adapter.properties, map[string]any{"Label": "old"}) {
		t.Fatalf("partially mutated properties = %#v, want compensation to restore old properties", adapter.properties)
	}
}

//nolint:wsl // The test keeps reload failure and create rollback assertions together.
func TestExecutorCreateRollsBackWhenPostCreateReloadFails(t *testing.T) {
	storage := &reloadFailureStorage{executorStorage: newExecutorStorage(t), failReload: true}
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	stack := executorCreateStack(t, storage.executorStorage)
	parsed, err := template.Parse(`
Resources:
  Base:
    Type: Example::Resource
    Properties:
      Name: base
`)
	if err != nil {
		t.Fatal(err)
	}

	err = NewExecutor(storage, registry).Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Template: parsed})
	if err == nil || !strings.Contains(err.Error(), "reload stack after creating resource") {
		t.Fatalf("Create() error = %v, want post-create reload failure", err)
	}
	if !reflect.DeepEqual(adapter.calls, []string{"create:base", "delete:base"}) {
		t.Fatalf("adapter calls = %v, want create rollback compensation", adapter.calls)
	}
}

//nolint:wsl // No-op setup and assertions intentionally remain adjacent.
func TestExecutorUpdateNoOpEvaluatedDiffSkipsAdapters(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	evaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Name": executorSameName})
	if err := seedExecutorState(t, storage, stack.StackID, evaluation, "same-physical"); err != nil {
		t.Fatal(err)
	}
	adapter.calls = nil

	if err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: executorEvaluation("Base", executorResourceType, map[string]any{"Name": executorSameName}), Options: ExecutionOptions{ClientRequestToken: executorUpdateToken}}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if len(adapter.calls) != 0 {
		t.Fatalf("adapter calls = %v, want no-op", adapter.calls)
	}
}

//nolint:wsl // Failure setup and retention assertions intentionally remain together.
func TestExecutorCreateFailureCanRetainCreatedResources(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	first := &executorAdapter{typeName: executorResourceType}
	second := &executorAdapter{typeName: executorSecondType, createErr: errors.New("create failed")}
	registry.Register(first)
	registry.Register(second)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	evaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"First":  {Type: executorResourceType, Properties: map[string]any{"Name": "first"}},
		"Second": {Type: executorSecondType, Properties: map[string]any{"Name": "second"}, DependsOn: []string{"First"}},
	}}

	err := executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: evaluation, Options: ExecutionOptions{ClientRequestToken: executorFailureToken, OnFailure: FailurePolicyRetain}})
	if err == nil {
		t.Fatal("Create() error = nil, want create failure")
	}
	if !reflect.DeepEqual(first.calls, []string{"create:first"}) {
		t.Fatalf("retained adapter calls = %v, want no rollback delete", first.calls)
	}
}

//nolint:wsl // Retain and Snapshot policy cases share one lifecycle fixture.
func TestExecutorDeleteHonorsRetainAndRejectsSnapshot(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	evaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"Retained": {Type: executorResourceType, Properties: map[string]any{"Name": "retained"}, DeletionPolicy: "Retain"},
	}}
	if err := seedExecutorState(t, storage, stack.StackID, evaluation, "retained-physical"); err != nil {
		t.Fatal(err)
	}
	if err := executor.Delete(t.Context(), LifecycleRequest{StackRef: stack.StackID, Options: ExecutionOptions{ClientRequestToken: executorUpdateToken}}); err != nil {
		t.Fatalf("Delete() with Retain error = %v", err)
	}
	if len(adapter.calls) != 0 {
		t.Fatalf("Retain adapter calls = %v, want none", adapter.calls)
	}

	snapshotStack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "executor-snapshot-stack", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &template.Evaluation{Resources: map[string]template.Resource{"Snapshot": {Type: executorResourceType, Properties: map[string]any{"Name": "snapshot"}, DeletionPolicy: "Snapshot"}}}
	if err := seedExecutorState(t, storage, snapshotStack.StackID, snapshot, "snapshot-physical"); err != nil {
		t.Fatal(err)
	}
	if err := executor.Delete(t.Context(), LifecycleRequest{StackRef: snapshotStack.StackID, Options: ExecutionOptions{ClientRequestToken: executorUpdateToken}}); err == nil || !strings.Contains(err.Error(), "Snapshot") {
		t.Fatalf("Delete() Snapshot error = %v, want rejection", err)
	}
}

//nolint:wsl // Failure setup and restoration assertions intentionally remain together.
func TestExecutorUpdateFailureRestoresDeletedOldResourceFromJournal(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	first := &executorAdapter{typeName: executorResourceType}
	second := &executorAdapter{typeName: executorSecondType, deleteErr: errors.New("second delete failed")}
	registry.Register(first)
	registry.Register(second)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	old := &template.Evaluation{Resources: map[string]template.Resource{
		"First":  {Type: executorResourceType, Properties: map[string]any{"Name": "old"}, DependsOn: []string{"Second"}},
		"Second": {Type: executorSecondType, Properties: map[string]any{"Name": "second-old"}},
	}}
	if err := seedExecutorState(t, storage, stack.StackID, old, "old-physical", "second-old-physical"); err != nil {
		t.Fatal(err)
	}
	first.calls = nil
	newEvaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"First":  {Type: executorResourceType, Properties: map[string]any{"Name": "new"}},
		"Second": {Type: executorSecondType, Properties: map[string]any{"Name": rollbackSecondNewName}},
	}}
	if err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: newEvaluation, Options: ExecutionOptions{ClientRequestToken: executorFailureToken}}); err == nil || !strings.Contains(err.Error(), "second delete failed") {
		t.Fatalf("Update() error = %v, want second cleanup failure", err)
	}
	if !reflect.DeepEqual(first.calls, []string{"create:new", executorOldDeleteCall, "delete:new", "create:old"}) {
		t.Fatalf("first adapter calls = %v, want journal-backed restoration", first.calls)
	}
	stored, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EvaluatedState.Resources["First"].Properties["Name"] != "old" || stored.StackStatus != StackStatusUpdateFailed {
		t.Fatalf("restored update state = %#v", stored)
	}
}

//nolint:wsl // Reverse dependency setup and assertions intentionally remain together.
func TestExecutorDeleteUsesReverseDependencyOrder(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType, order: &storage.adapterOrder, orderLabel: "Base"}
	dependent := &executorAdapter{typeName: executorSecondType, order: &storage.adapterOrder, orderLabel: "Child"}
	registry.Register(adapter)
	registry.Register(dependent)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	evaluation := &template.Evaluation{Resources: map[string]template.Resource{
		"Base":  {Type: executorResourceType, Properties: map[string]any{"Name": "base"}},
		"Child": {Type: executorSecondType, Properties: map[string]any{"Name": "child"}, DependsOn: []string{"Base"}},
	}}
	if err := seedExecutorState(t, storage, stack.StackID, evaluation, "base-physical", "child-physical"); err != nil {
		t.Fatal(err)
	}
	if err := executor.Delete(t.Context(), LifecycleRequest{StackRef: stack.StackID, Options: ExecutionOptions{ClientRequestToken: executorUpdateToken}}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if got := storage.adapterOrder; !reflect.DeepEqual(got, []string{"Child", "Base"}) {
		t.Fatalf("delete order = %v", got)
	}
}

//nolint:goconst,wsl // Repeated fixture names make policy assertions readable.
func TestExecutorReplacementRejectsUnchangedFixedCustomNameBeforeCreate(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType, schema: resource.Schema{TypeName: executorResourceType, PropertyUpdate: map[string]resource.PropertyUpdate{
		"QueueName": resource.PropertyUpdate("Replacement"),
		"FifoQueue": resource.PropertyUpdate("Replacement"),
	}}}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	old := executorEvaluation("Queue", executorResourceType, map[string]any{"QueueName": "fixed", "FifoQueue": false})
	if err := seedExecutorState(t, storage, stack.StackID, old, "fixed"); err != nil {
		t.Fatal(err)
	}
	newEvaluation := executorEvaluation("Queue", executorResourceType, map[string]any{"QueueName": "fixed", "FifoQueue": true})

	err := executor.Update(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: newEvaluation, Options: ExecutionOptions{ClientRequestToken: executorFailureToken}})
	if err == nil || !strings.Contains(err.Error(), "custom name") {
		t.Fatalf("Update() error = %v, want fixed custom-name rejection", err)
	}
	if len(adapter.calls) != 0 {
		t.Fatalf("adapter calls = %v, want preflight rejection", adapter.calls)
	}
}

//nolint:wsl // Repeated fixture names make export assertions readable.
func TestExecutorStagesAndCommitsEvaluatedExportsOnlyAfterSuccessfulExecution(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	adapter := &executorAdapter{typeName: executorResourceType}
	registry.Register(adapter)
	executor := NewExecutor(storage, registry)
	stack := executorCreateStack(t, storage)
	evaluation := executorEvaluation("Base", executorResourceType, map[string]any{"Name": "base"})
	evaluation.Outputs = map[string]template.Output{"URL": {Value: "https://example.test", ExportName: "ExecutorURL"}}

	if err := executor.Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: evaluation, Options: ExecutionOptions{ClientRequestToken: executorCreateToken}}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	exports, err := storage.GetExports(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if len(exports) != 1 || exports[0].Name != "ExecutorURL" || exports[0].Value != "https://example.test" {
		t.Fatalf("committed exports = %#v", exports)
	}
}

type statefulExecutorAdapter struct {
	mu         sync.Mutex
	typeName   string
	resources  map[string]map[string]any
	calls      []string
	failDelete string
}

func newStatefulExecutorAdapter(typeName string) *statefulExecutorAdapter {
	return &statefulExecutorAdapter{typeName: typeName, resources: make(map[string]map[string]any)}
}

func (a *statefulExecutorAdapter) Schema() resource.Schema {
	return resource.Schema{TypeName: a.typeName, PropertyUpdate: map[string]resource.PropertyUpdate{
		"Label": resource.PropertyUpdate("NoInterruption"),
		"Name":  resource.PropertyUpdate("Replacement"),
	}}
}

func (a *statefulExecutorAdapter) seed(identifier string, properties map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resources[identifier] = cloneAnyMap(properties)
}

func (a *statefulExecutorAdapter) Create(_ context.Context, req resource.CreateRequest) (resource.CreateResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	name, _ := req.Properties["Name"].(string)
	identifier := name + "-physical"
	a.resources[identifier] = cloneAnyMap(req.Properties)
	a.calls = append(a.calls, "create:"+identifier)

	return resource.CreateResult{Identifier: identifier}, nil
}

func (a *statefulExecutorAdapter) Update(_ context.Context, req resource.UpdateRequest) (resource.UpdateResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resources[req.Identifier] = cloneAnyMap(req.Properties)
	a.calls = append(a.calls, "update:"+req.Identifier)

	return resource.UpdateResult{}, nil
}

func (a *statefulExecutorAdapter) Delete(_ context.Context, req resource.DeleteRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, "delete:"+req.Identifier)
	if req.Identifier == a.failDelete {
		return errors.New("stateful delete failed")
	}
	delete(a.resources, req.Identifier)

	return nil
}

func (a *statefulExecutorAdapter) propertiesFor(identifier string) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()

	return cloneAnyMap(a.resources[identifier])
}

func (a *statefulExecutorAdapter) exists(identifier string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	_, exists := a.resources[identifier]

	return exists
}

type executorAdapter struct {
	mu              sync.Mutex
	typeName        string
	schema          resource.Schema
	createErr       error
	createErrFor    string
	deleteErr       error
	deleteErrFor    string
	emptyIdentifier bool
	calls           []string
	capabilities    []string
	onCreate        func(context.Context)
	order           *[]string
	orderLabel      string
	onDelete        func(context.Context)
	lastProperties  map[string]any
}

type compensatingUpdateAdapter struct {
	mu                   sync.Mutex
	typeName             string
	schema               resource.Schema
	properties           map[string]any
	attributes           map[string]any
	updateCapabilities   [][]string
	restoreErr           error
	omitUpdateAttributes bool
}

type partialUpdateAdapter struct {
	mu         sync.Mutex
	typeName   string
	schema     resource.Schema
	properties map[string]any
	failed     bool
}

func (a *partialUpdateAdapter) Schema() resource.Schema { return a.schema }

func (a *partialUpdateAdapter) Create(_ context.Context, req resource.CreateRequest) (resource.CreateResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.properties = cloneAnyMap(req.Properties)

	return resource.CreateResult{Identifier: "stable-physical"}, nil
}

func (a *partialUpdateAdapter) Update(_ context.Context, req resource.UpdateRequest) (resource.UpdateResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.properties = cloneAnyMap(req.Properties)
	if !a.failed {
		a.failed = true

		return resource.UpdateResult{}, errors.New("partial update failed")
	}

	return resource.UpdateResult{}, nil
}

func (*partialUpdateAdapter) Delete(context.Context, resource.DeleteRequest) error { return nil }

func (a *compensatingUpdateAdapter) Schema() resource.Schema { return a.schema }

func (a *compensatingUpdateAdapter) Create(_ context.Context, req resource.CreateRequest) (resource.CreateResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.properties = cloneAnyMap(req.Properties)
	label, _ := req.Properties["Label"].(string)
	a.attributes = map[string]any{"Arn": "arn:" + label}

	return resource.CreateResult{Identifier: "stable-physical", Attributes: cloneAnyMap(a.attributes)}, nil
}

func (a *compensatingUpdateAdapter) Update(_ context.Context, req resource.UpdateRequest) (resource.UpdateResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updateCapabilities = append(a.updateCapabilities, append([]string(nil), req.Capabilities...))
	label, _ := req.Properties["Label"].(string)
	if label == "old" && a.restoreErr != nil {
		return resource.UpdateResult{}, a.restoreErr
	}
	a.properties = cloneAnyMap(req.Properties)
	a.attributes = map[string]any{"Arn": "arn:" + label}
	if a.omitUpdateAttributes {
		return resource.UpdateResult{}, nil
	}

	return resource.UpdateResult{Attributes: cloneAnyMap(a.attributes)}, nil
}

func (*compensatingUpdateAdapter) Delete(context.Context, resource.DeleteRequest) error { return nil }

func (a *executorAdapter) Schema() resource.Schema {
	if a.schema.TypeName == "" {
		a.schema = resource.Schema{TypeName: a.typeName, PropertyUpdate: map[string]resource.PropertyUpdate{
			"Name": resource.PropertyUpdate("Replacement"),
		}}
	}

	return a.schema
}

//nolint:wsl // The fake records one adapter call and its test-only side effects.
func (a *executorAdapter) Create(ctx context.Context, req resource.CreateRequest) (resource.CreateResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	name, _ := req.Properties["Name"].(string)
	a.calls = append(a.calls, "create:"+name)
	a.lastProperties = cloneAnyMap(req.Properties)
	if a.order != nil {
		*a.order = append(*a.order, a.orderLabel)
	}
	a.capabilities = append([]string(nil), req.Capabilities...)
	if a.onCreate != nil {
		a.onCreate(ctx)
	}
	if a.createErr != nil {
		return resource.CreateResult{}, a.createErr
	}
	if a.createErrFor != "" && a.createErrFor == name {
		return resource.CreateResult{}, errors.New("create failed")
	}
	if a.emptyIdentifier {
		return resource.CreateResult{}, nil
	}

	attributes := map[string]any{}
	for _, attribute := range a.Schema().Attributes {
		if attribute == "Arn" {
			attributes[attribute] = "arn:" + name
		}
	}

	return resource.CreateResult{Identifier: name + "-physical", Attributes: attributes}, nil
}

func (a *executorAdapter) Update(_ context.Context, req resource.UpdateRequest) (resource.UpdateResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	name, _ := req.Properties["Label"].(string)
	a.calls = append(a.calls, "update:"+req.Identifier+":"+"old->"+name)
	a.lastProperties = cloneAnyMap(req.Properties)

	return resource.UpdateResult{}, nil
}

//nolint:wsl // The fake records one adapter call and its test-only side effects.
func (a *executorAdapter) Delete(ctx context.Context, req resource.DeleteRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, "delete:"+strings.TrimSuffix(req.Identifier, "-physical"))
	if a.order != nil {
		*a.order = append(*a.order, a.orderLabel)
	}
	if a.onDelete != nil {
		a.onDelete(ctx)
	}
	if a.deleteErrFor != "" && a.deleteErrFor == strings.TrimSuffix(req.Identifier, "-physical") {
		return errors.New("delete failed")
	}

	return a.deleteErr
}

type capabilityRecordingAdapter struct {
	mu                   sync.Mutex
	typeName             string
	rollbackCapabilities []string
}

func (a *capabilityRecordingAdapter) Schema() resource.Schema {
	return resource.Schema{TypeName: a.typeName, PropertyUpdate: map[string]resource.PropertyUpdate{"Name": resource.PropertyUpdate("Replacement")}}
}

func (a *capabilityRecordingAdapter) Create(_ context.Context, req resource.CreateRequest) (resource.CreateResult, error) {
	name, _ := req.Properties["Name"].(string)
	if name == "old" {
		a.mu.Lock()
		a.rollbackCapabilities = make([]string, len(req.Capabilities))
		copy(a.rollbackCapabilities, req.Capabilities)
		a.mu.Unlock()
	}

	return resource.CreateResult{Identifier: name + "-physical"}, nil
}

func (*capabilityRecordingAdapter) Delete(context.Context, resource.DeleteRequest) error { return nil }

type terminalFailureStorage struct {
	*executorStorage
	terminalEventSeen bool
}

type failingExecutionCompletionStorage struct {
	*MemoryStorage
	fail bool
}

func (s *failingExecutionCompletionStorage) CompleteExecution(ctx context.Context, stackRef, token string, success bool, errorCode, reason string) error {
	if s.fail {
		s.fail = false

		return errors.New("injected completion persistence failure")
	}

	return s.MemoryStorage.CompleteExecution(ctx, stackRef, token, success, errorCode, reason)
}

func newTerminalFailureStorage(t *testing.T) *terminalFailureStorage {
	t.Helper()

	return &terminalFailureStorage{executorStorage: newExecutorStorage(t)}
}

func (s *terminalFailureStorage) RunStackOperation(ctx context.Context, stackRef string, operation func(context.Context, *Stack) error) error {
	return s.MemoryStorage.RunStackOperation(ctx, stackRef, func(operationCtx context.Context, working *Stack) error {
		err := operation(operationCtx, working)
		if err == nil && (working.StackStatus == StackStatusCreateComplete || working.StackStatus == StackStatusUpdateComplete || working.StackStatus == StackStatusDeleteComplete) {
			s.terminalEventSeen = hasEventStatus(working.Events, working.StackStatus)

			return errors.New("terminal state persistence failed")
		}

		return err
	})
}

type executorStorage struct {
	*MemoryStorage
	mu              sync.Mutex
	adapterOrder    []string
	history         []string
	operationActive atomic.Bool
	exclusiveCalls  atomic.Int32
}

type exclusiveTrackingStorage struct {
	*MemoryStorage
	exclusiveCalls atomic.Int32
}

func (s *exclusiveTrackingStorage) RunExclusiveStackOperation(ctx context.Context, stackRef string, operation func(context.Context) error) error {
	s.exclusiveCalls.Add(1)

	return s.MemoryStorage.RunExclusiveStackOperation(ctx, stackRef, operation)
}

func newExecutorStorage(t *testing.T) *executorStorage {
	t.Helper()

	return &executorStorage{MemoryStorage: NewMemoryStorage()}
}

func (s *executorStorage) RunExclusiveStackOperation(ctx context.Context, _ string, operation func(context.Context) error) error {
	s.exclusiveCalls.Add(1)
	s.operationActive.Store(true)
	defer s.operationActive.Store(false)

	return operation(ctx)
}

//nolint:gocritic // The event value is intentionally copied at the storage boundary.
func (s *executorStorage) AppendStackEvent(ctx context.Context, stackRef string, event StackEvent) error {
	s.mu.Lock()
	s.history = append(s.history, "event")
	s.mu.Unlock()

	return s.MemoryStorage.AppendStackEvent(ctx, stackRef, event)
}

func (s *executorStorage) AppendStackJournal(ctx context.Context, stackRef, operation, status, reason string) error {
	s.mu.Lock()
	s.history = append(s.history, "journal")
	s.mu.Unlock()

	return s.MemoryStorage.AppendStackJournal(ctx, stackRef, operation, status, reason)
}

//nolint:gocritic // The journal value is intentionally copied at the storage boundary.
func (s *executorStorage) AppendStackJournalEntry(ctx context.Context, stackRef string, entry StackJournalEntry) error {
	s.mu.Lock()
	s.history = append(s.history, "journal")
	s.mu.Unlock()

	return s.MemoryStorage.AppendStackJournalEntry(ctx, stackRef, entry)
}

//nolint:wsl // Stack creation setup is intentionally kept with its error assertion.
func executorCreateStack(t *testing.T, storage *executorStorage) *Stack {
	t.Helper()
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{
		StackName:    executorStackName,
		TemplateBody: testEmptyResourcesTemplate,
	})
	if err != nil {
		t.Fatalf("CreateStack() error = %v", err)
	}

	return stack
}

func executorEvaluation(logicalID, typeName string, properties map[string]any) *template.Evaluation {
	return &template.Evaluation{Resources: map[string]template.Resource{
		logicalID: {Type: typeName, Properties: properties},
	}}
}

func containsJournalReason(entries []StackJournalEntry, value string) bool {
	for index := range entries {
		if strings.Contains(entries[index].Reason, value) {
			return true
		}
	}

	return false
}

func latestJournalHas(t *testing.T, entries []StackJournalEntry, logicalID, status string, properties, attributes map[string]any) bool {
	t.Helper()
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.LogicalResourceID != logicalID {
			continue
		}

		return entry.Status == status && reflect.DeepEqual(entry.Properties, properties) && reflect.DeepEqual(entry.Attributes, attributes)
	}

	return false
}

func latestJournalStatus(entries []StackJournalEntry, logicalID, status string) bool {
	for index := len(entries) - 1; index >= 0; index-- {
		if entries[index].LogicalResourceID == logicalID {
			return entries[index].Status == status
		}
	}

	return false
}

func containsEventStatus(events []StackEvent, status string) bool {
	for index := range events {
		if events[index].ResourceStatus == status {
			return true
		}
	}

	return false
}

func hasEventStatus(events []StackEvent, status string) bool {
	return containsEventStatus(events, status)
}

//nolint:wsl // The helper accepts both adapter types used by lifecycle fixtures.
func seedExecutorState(t *testing.T, storage *executorStorage, stackRef string, evaluation *template.Evaluation, physicalIDs ...string) error {
	t.Helper()

	return storage.RunStackOperation(t.Context(), stackRef, func(_ context.Context, stack *Stack) error {
		stack.EvaluatedState = evaluation
		stack.Resources = make([]StackResource, 0, len(evaluation.Resources))
		logicalIDs := make([]string, 0, len(evaluation.Resources))
		for logicalID := range evaluation.Resources {
			logicalIDs = append(logicalIDs, logicalID)
		}
		slices.Sort(logicalIDs)
		for index, logicalID := range logicalIDs {
			physicalID := logicalID + "-physical"
			if index < len(physicalIDs) {
				physicalID = physicalIDs[index]
			}
			stack.Resources = append(stack.Resources, StackResource{LogicalResourceID: logicalID, PhysicalResourceID: physicalID, ResourceType: evaluation.Resources[logicalID].Type, ResourceStatus: ResourceStatusCreateComplete, StackID: stack.StackID, StackName: stack.StackName})
		}

		return nil
	})
}

type afterJournalFailureStorage struct {
	*executorStorage
	failAfter atomic.Bool
}

type reloadFailureStorage struct {
	*executorStorage
	failReload bool
	getCalls   atomic.Int32
}

func (s *reloadFailureStorage) GetStack(ctx context.Context, stackRef string) (*Stack, error) {
	if s.failReload && s.getCalls.Add(1) > 1 {
		return nil, errors.New("injected post-create stack reload failure")
	}

	return s.executorStorage.GetStack(ctx, stackRef)
}

type rollbackRecreationJournalFailureStorage struct {
	*executorStorage
}

func newRollbackRecreationJournalFailureStorage(t *testing.T) *rollbackRecreationJournalFailureStorage {
	t.Helper()

	return &rollbackRecreationJournalFailureStorage{executorStorage: newExecutorStorage(t)}
}

//nolint:gocritic // The journal value is intentionally copied at the storage seam.
func (s *rollbackRecreationJournalFailureStorage) AppendStackJournalEntry(ctx context.Context, stackRef string, entry StackJournalEntry) error {
	name, _ := entry.Properties["Name"].(string)
	if entry.Status == journalAfter && name == "old" {
		return errors.New("rollback recreation after journal persistence failed")
	}

	return s.executorStorage.AppendStackJournalEntry(ctx, stackRef, entry)
}

type commitFailureStorage struct {
	*executorStorage
	failNextCommit atomic.Bool
}

func newCommitFailureStorage(t *testing.T) *commitFailureStorage {
	t.Helper()

	return &commitFailureStorage{executorStorage: newExecutorStorage(t)}
}

func (s *commitFailureStorage) RunStackOperation(ctx context.Context, stackRef string, operation func(context.Context, *Stack) error) error {
	if s.failNextCommit.Swap(false) {
		return errors.New("state commit failed")
	}

	return s.executorStorage.RunStackOperation(ctx, stackRef, operation)
}

type failurePersistenceStorage struct {
	*executorStorage
	failState     atomic.Bool
	stateAttempts atomic.Int32
	eventAttempts atomic.Int32
}

func newFailurePersistenceStorage(t *testing.T) *failurePersistenceStorage {
	t.Helper()

	return &failurePersistenceStorage{executorStorage: newExecutorStorage(t)}
}

func (s *failurePersistenceStorage) RunStackOperation(ctx context.Context, stackRef string, operation func(context.Context, *Stack) error) error {
	return s.MemoryStorage.RunStackOperation(ctx, stackRef, func(operationCtx context.Context, working *Stack) error {
		if err := operation(operationCtx, working); err != nil {
			return err
		}

		if s.failState.Load() && (working.StackStatus == StackStatusCreateFailed || working.StackStatus == StackStatusUpdateFailed || working.StackStatus == StackStatusDeleteFailed) {
			s.stateAttempts.Add(1)

			return errors.Join(errors.New("state persistence failed"), errors.New("event persistence failed"))
		}

		return nil
	})
}

//nolint:gocritic // The event value is intentionally copied at the storage seam.
func (s *failurePersistenceStorage) AppendStackEvent(ctx context.Context, stackRef string, event StackEvent) error {
	if event.ResourceStatus == StackStatusCreateFailed {
		s.eventAttempts.Add(1)

		return errors.New("event persistence failed")
	}

	return s.executorStorage.AppendStackEvent(ctx, stackRef, event)
}

func newAfterJournalFailureStorage(t *testing.T) *afterJournalFailureStorage {
	t.Helper()

	return &afterJournalFailureStorage{executorStorage: newExecutorStorage(t)}
}

//nolint:gocritic // The journal value is intentionally copied at the storage seam.
func (s *afterJournalFailureStorage) AppendStackJournalEntry(ctx context.Context, stackRef string, entry StackJournalEntry) error {
	if s.failAfter.Load() && entry.Status == journalAfter {
		return errors.New("after journal persistence failed")
	}

	return s.executorStorage.AppendStackJournalEntry(ctx, stackRef, entry)
}

type rollbackIdentifierAdapter struct {
	mu                     sync.Mutex
	typeName               string
	required               string
	rollbackCapabilitySeen bool
}

type rollbackRecreationAdapter struct {
	mu                sync.Mutex
	typeName          string
	deleteRestoredErr error
	calls             []string
}

func (a *rollbackRecreationAdapter) Schema() resource.Schema {
	return resource.Schema{TypeName: a.typeName, PropertyUpdate: map[string]resource.PropertyUpdate{"Name": resource.PropertyUpdate("Replacement")}}
}

func (a *rollbackRecreationAdapter) Create(_ context.Context, req resource.CreateRequest) (resource.CreateResult, error) {
	name, _ := req.Properties["Name"].(string)

	a.mu.Lock()
	a.calls = append(a.calls, "create:"+name)
	a.mu.Unlock()

	if name == "old" {
		return resource.CreateResult{Identifier: rollbackRestoredID}, nil
	}

	return resource.CreateResult{Identifier: name + "-physical"}, nil
}

func (a *rollbackRecreationAdapter) Delete(_ context.Context, req resource.DeleteRequest) error {
	a.mu.Lock()
	a.calls = append(a.calls, "delete:"+req.Identifier)
	a.mu.Unlock()

	if req.Identifier == rollbackRestoredID {
		return a.deleteRestoredErr
	}

	return nil
}

func (a *rollbackIdentifierAdapter) Schema() resource.Schema {
	return resource.Schema{TypeName: a.typeName, RequiredCapabilities: []string{a.required}, PropertyUpdate: map[string]resource.PropertyUpdate{
		"Name": resource.PropertyUpdate("Replacement"),
	}}
}

func (a *rollbackIdentifierAdapter) Create(_ context.Context, req resource.CreateRequest) (resource.CreateResult, error) {
	name, _ := req.Properties["Name"].(string)
	if name == "old" && slices.Contains(req.Capabilities, a.required) {
		a.mu.Lock()
		a.rollbackCapabilitySeen = true
		a.mu.Unlock()
	}

	if name == "old" {
		return resource.CreateResult{Identifier: rollbackRestoredID}, nil
	}

	return resource.CreateResult{Identifier: name + "-physical"}, nil
}

func (*rollbackIdentifierAdapter) Delete(context.Context, resource.DeleteRequest) error { return nil }
