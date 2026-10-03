package cloudformation

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sivchari/kumo/internal/service/cloudformation/resource"
	"github.com/sivchari/kumo/internal/service/cloudformation/template"
)

const (
	changeSetBaseTemplate       = `{"Parameters":{"Environment":{"Type":"String","Default":"dev"}},"Resources":{}}`
	testS3BucketResourceType    = "AWS::S3::Bucket"
	testFirstChangeSetName      = "first"
	testSiblingChangeSetName    = "sibling"
	testSecondChangeSetName     = "second"
	changeSetDefaultEnvironment = "dev"
	changeSetUpdateLogicalID    = "AUpdate"
	changeSetDeleteLogicalID    = "ADelete"
	changeSetLabelProperty      = "Label"
	changeSetNameProperty       = "Name"
	changeSetOldValue           = "old"
	changeSetDeleteName         = "a-delete"
)

//nolint:wsl // The test keeps review setup and immutable-state assertions together.
func TestCreateChangeSetStoresImmutableEffectiveStateAndReplacementChanges(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	createStackForChangeSet(t, svc, "change-set-stack", changeSetBaseTemplate)

	oldTemplate := `{"Resources":{"Bucket":{"Type":"AWS::S3::Bucket","Properties":{"BucketName":"old-name"}}}}`
	if err := storage.RunStackOperation(t.Context(), "change-set-stack", func(_ context.Context, stack *Stack) error {
		stack.TemplateBody = oldTemplate
		stack.EvaluatedState = &template.Evaluation{Resources: map[string]template.Resource{
			testBucketLogicalID: {Type: testS3BucketResourceType, Properties: map[string]any{"BucketName": "old-name"}},
		}}
		stack.Resources = []StackResource{{LogicalResourceID: testBucketLogicalID, PhysicalResourceID: "bucket-old", ResourceType: testS3BucketResourceType, ResourceStatus: ResourceStatusCreateComplete, StackID: stack.StackID, StackName: stack.StackName}}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	newTemplate := `{"Parameters":{"Environment":{"Type":"String","Default":"dev"}},"Resources":{"Bucket":{"Type":"AWS::S3::Bucket","Properties":{"BucketName":"new-name"}}}}`
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("CreateChangeSet", `{"StackName":"change-set-stack","ChangeSetName":"replacement","ChangeSetType":"UPDATE","TemplateBody":`+strconv.Quote(newTemplate)+`,"Parameters":[{"ParameterKey":"Environment","ParameterValue":"prod"}],"ClientToken":"change-set-token"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateChangeSet() status/body = %d/%s", rec.Code, rec.Body.String())
	}

	changeSet, err := storage.GetChangeSet(t.Context(), "change-set-stack", "replacement")
	if err != nil {
		t.Fatal(err)
	}
	if changeSet.Status != ChangeSetStatusCreateComplete || changeSet.BaseRevision == 0 {
		t.Fatalf("change set = %#v, want complete revision-bound review", changeSet)
	}
	if changeSet.TemplateBody != newTemplate || changeSet.Parameters["Environment"] != "prod" {
		t.Fatalf("change set effective state = %#v, want immutable template and parameters", changeSet)
	}
	if len(changeSet.ResourceChanges) != 1 || changeSet.ResourceChanges[0].Action != ChangeActionModify || changeSet.ResourceChanges[0].Replacement != ReplacementTrue {
		t.Fatalf("resource changes = %#v, want one replacement modification", changeSet.ResourceChanges)
	}

	changeSet.TemplateBody = "mutated"
	changeSet.Parameters["Environment"] = "mutated"
	changeSet.ResourceChanges[0].Replacement = ReplacementFalse
	stored, err := storage.GetChangeSet(t.Context(), "change-set-stack", "replacement")
	if err != nil {
		t.Fatal(err)
	}
	if stored.TemplateBody != newTemplate || stored.Parameters["Environment"] != "prod" || stored.ResourceChanges[0].Replacement != ReplacementTrue {
		t.Fatalf("stored change set was mutable through returned snapshot: %#v", stored)
	}
}

//nolint:wsl // The test keeps retained failure assertions beside the request.
func TestCreateChangeSetRetainsEmptyDiffAsFailed(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	createStackForChangeSet(t, svc, "empty-change-set-stack", changeSetBaseTemplate)

	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("CreateChangeSet", `{"StackName":"empty-change-set-stack","ChangeSetName":"empty","ChangeSetType":"UPDATE","TemplateBody":`+strconv.Quote(changeSetBaseTemplate)+`}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateChangeSet() status/body = %d/%s, want retained failed review", rec.Code, rec.Body.String())
	}
	changeSet, err := storage.GetChangeSet(t.Context(), "empty-change-set-stack", "empty")
	if err != nil {
		t.Fatal(err)
	}
	if changeSet.Status != ChangeSetStatusFailed || changeSet.ExecutionStatus != ChangeSetExecutionUnavailable || len(changeSet.ResourceChanges) != 0 {
		t.Fatalf("empty change set = %#v, want failed retained empty diff", changeSet)
	}
}

//nolint:wsl // The test keeps sibling review and execution assertions together.
func TestExecuteChangeSetUsesExecutorAndObsoletesSiblings(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	createStackForChangeSet(t, svc, "execute-change-set-stack", changeSetBaseTemplate)

	for _, name := range []string{testFirstChangeSetName, testSiblingChangeSetName} {
		rec := httptest.NewRecorder()
		svc.DispatchAction(rec, jsonRequest("CreateChangeSet", `{"StackName":"execute-change-set-stack","ChangeSetName":"`+name+`","ChangeSetType":"UPDATE","TemplateBody":`+strconv.Quote(changeSetBaseTemplate)+`,"Parameters":[{"ParameterKey":"Environment","ParameterValue":"`+name+`"}]}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("CreateChangeSet(%q) status/body = %d/%s", name, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("ExecuteChangeSet", `{"StackName":"execute-change-set-stack","ChangeSetName":"`+testFirstChangeSetName+`","ClientRequestToken":"execute-token"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("ExecuteChangeSet() status/body = %d/%s", rec.Code, rec.Body.String())
	}
	stack, err := storage.GetStack(t.Context(), "execute-change-set-stack")
	if err != nil {
		t.Fatal(err)
	}
	if stack.EvaluatedState == nil || stack.EvaluatedState.Parameters["Environment"] != testFirstChangeSetName || !hasEventToken(stack.Events, "execute-token") {
		t.Fatalf("executed stack = %#v, want executor update and request token", stack)
	}
	first, err := storage.GetChangeSet(t.Context(), "execute-change-set-stack", testFirstChangeSetName)
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := storage.GetChangeSet(t.Context(), "execute-change-set-stack", testSiblingChangeSetName)
	if err != nil {
		t.Fatal(err)
	}
	if first.ExecutionStatus != ChangeSetExecutionComplete || sibling.Status != ChangeSetStatusObsolete {
		t.Fatalf("change set statuses = %#v/%#v, want executed first and obsolete sibling", first, sibling)
	}
}

//nolint:wsl // The test keeps stale review and execution rejection together.
func TestExecuteChangeSetRejectsStaleBaseRevision(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	createStackForChangeSet(t, svc, "stale-change-set-stack", changeSetBaseTemplate)

	create := httptest.NewRecorder()
	staleTemplate := `{"Parameters":{"Environment":{"Type":"String","Default":"dev"}},"Resources":{}}`
	svc.DispatchAction(create, jsonRequest("CreateChangeSet", `{"StackName":"stale-change-set-stack","ChangeSetName":"stale","ChangeSetType":"UPDATE","TemplateBody":`+strconv.Quote(staleTemplate)+`,"Parameters":[{"ParameterKey":"Environment","ParameterValue":"prod"}]}`))
	if create.Code != http.StatusOK {
		t.Fatalf("CreateChangeSet() status/body = %d/%s", create.Code, create.Body.String())
	}
	if err := storage.RunStackOperation(t.Context(), "stale-change-set-stack", func(_ context.Context, stack *Stack) error {
		stack.StackStatusReason = "changed after review"

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("ExecuteChangeSet", `{"StackName":"stale-change-set-stack","ChangeSetName":"stale"}`))
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "stale") {
		t.Fatalf("ExecuteChangeSet() status/body = %d/%s, want stale revision rejection", rec.Code, rec.Body.String())
	}
}

//nolint:wsl // The test keeps review-shell lifecycle assertions together.
func TestCreateChangeSetCreateReviewShellAndExecute(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	templateBody := `{"Resources":{}}`
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("CreateChangeSet", `{"StackName":"review-shell-stack","ChangeSetName":"create","ChangeSetType":"CREATE","TemplateBody":`+strconv.Quote(templateBody)+`}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateChangeSet(CREATE) status/body = %d/%s", rec.Code, rec.Body.String())
	}
	stack, err := storage.GetStack(t.Context(), "review-shell-stack")
	if err != nil {
		t.Fatal(err)
	}
	if stack.StackStatus != StackStatusReviewInProgress {
		t.Fatalf("review shell status = %q, want REVIEW_IN_PROGRESS", stack.StackStatus)
	}

	rec = httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("ExecuteChangeSet", `{"StackName":"review-shell-stack","ChangeSetName":"create"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("ExecuteChangeSet(CREATE) status/body = %d/%s", rec.Code, rec.Body.String())
	}
	stack, err = storage.GetStack(t.Context(), "review-shell-stack")
	if err != nil {
		t.Fatal(err)
	}
	if stack.StackStatus != StackStatusCreateComplete {
		t.Fatalf("executed review shell status = %q, want CREATE_COMPLETE", stack.StackStatus)
	}
}

//nolint:wsl // Cross-stack review and execution assertions intentionally share one lifecycle fixture.
func TestCreateChangeSetCreateExecutionResolvesCrossStackImport(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	createStackForChangeSet(t, svc, "change-set-import-exporter", changeSetBaseTemplate)
	exporter, err := storage.GetStack(t.Context(), "change-set-import-exporter")
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.RunStackOperation(t.Context(), exporter.StackID, func(_ context.Context, stack *Stack) error {
		stack.Exports = []StackExport{{Name: "SharedBucketName", Value: "change-set-imported-bucket", ExporterStackARN: stack.StackID}}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	templateBody := `Resources:
  Bucket:
    Type: AWS::S3::Bucket
    Properties:
      BucketName: !ImportValue SharedBucketName
`
	create := httptest.NewRecorder()
	svc.DispatchAction(create, jsonRequest("CreateChangeSet", `{"StackName":"change-set-import-consumer","ChangeSetName":"create","ChangeSetType":"CREATE","TemplateBody":`+strconv.Quote(templateBody)+`}`))
	if create.Code != http.StatusOK {
		t.Fatalf("CreateChangeSet() status/body = %d/%s", create.Code, create.Body.String())
	}
	execute := httptest.NewRecorder()
	svc.DispatchAction(execute, jsonRequest("ExecuteChangeSet", `{"StackName":"change-set-import-consumer","ChangeSetName":"create"}`))
	if execute.Code != http.StatusOK {
		t.Fatalf("ExecuteChangeSet() status/body = %d/%s", execute.Code, execute.Body.String())
	}
	imports, err := storage.GetImports(t.Context(), "change-set-import-consumer")
	if err != nil {
		t.Fatal(err)
	}
	if len(imports) != 1 || imports[0].ExportName != "SharedBucketName" {
		t.Fatalf("committed imports = %#v, want SharedBucketName edge", imports)
	}
}

//nolint:wsl // The test keeps both lifecycle requests beside their assertions.
func TestCreateChangeSetRejectsUpdateOnReviewShell(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	templateBody := `{"Resources":{}}`
	create := httptest.NewRecorder()
	svc.DispatchAction(create, jsonRequest("CreateChangeSet", `{"StackName":"review-shell-update","ChangeSetName":"create","ChangeSetType":"CREATE","TemplateBody":`+strconv.Quote(templateBody)+`}`))
	if create.Code != http.StatusOK {
		t.Fatalf("CreateChangeSet(CREATE) status/body = %d/%s", create.Code, create.Body.String())
	}

	update := httptest.NewRecorder()
	svc.DispatchAction(update, jsonRequest("CreateChangeSet", `{"StackName":"review-shell-update","ChangeSetName":"update","ChangeSetType":"UPDATE","TemplateBody":`+strconv.Quote(templateBody)+`}`))
	if update.Code == http.StatusOK || !strings.Contains(update.Body.String(), "UPDATE change sets require") {
		t.Fatalf("CreateChangeSet(UPDATE) status/body = %d/%s, want review-shell rejection", update.Code, update.Body.String())
	}
}

//nolint:wsl // The table-driven test keeps request and persisted review assertions together.
func TestCreateChangeSetRunsExecutorPreflight(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	createStackForChangeSet(t, svc, "change-set-preflight", changeSetBaseTemplate)

	cases := []struct {
		name          string
		template      string
		resourceTypes []string
		reason        string
	}{
		{
			name:     "unknown-resource",
			template: `{"Parameters":{"Environment":{"Type":"String","Default":"dev"}},"Resources":{"Unknown":{"Type":"Custom::Unavailable"}}}`,
			reason:   "unregistered type",
		},
		{
			name:     "missing-capability",
			template: `{"Parameters":{"Environment":{"Type":"String","Default":"dev"}},"Resources":{"Role":{"Type":"AWS::IAM::Role"}}}`,
			reason:   "requires CAPABILITY_IAM",
		},
		{
			name:          "disallowed-resource-type",
			template:      `{"Parameters":{"Environment":{"Type":"String","Default":"dev"}},"Resources":{"Bucket":{"Type":"AWS::S3::Bucket"}}}`,
			resourceTypes: []string{"AWS::IAM::Role"},
			reason:        "uses disallowed resource type",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			resourceTypes := ""
			if len(testCase.resourceTypes) > 0 {
				resourceTypes = `,"ResourceTypes":["` + strings.Join(testCase.resourceTypes, `","`) + `"]`
			}
			svc.DispatchAction(rec, jsonRequest("CreateChangeSet", `{"StackName":"change-set-preflight","ChangeSetName":"`+testCase.name+`","ChangeSetType":"UPDATE","TemplateBody":`+strconv.Quote(testCase.template)+resourceTypes+`}`))
			if rec.Code != http.StatusOK {
				t.Fatalf("CreateChangeSet() status/body = %d/%s", rec.Code, rec.Body.String())
			}
			changeSet, err := storage.GetChangeSet(t.Context(), "change-set-preflight", testCase.name)
			if err != nil {
				t.Fatal(err)
			}
			if changeSet.Status != ChangeSetStatusFailed || changeSet.ExecutionStatus != ChangeSetExecutionUnavailable || !strings.Contains(changeSet.StatusReason, testCase.reason) {
				t.Fatalf("change set = %#v, want failed unavailable preflight", changeSet)
			}
		})
	}
}

//nolint:wsl // The test coordinates an injected revision mutation at the commit boundary.
func TestCreateChangeSetRejectsRevisionChangedBeforeCommit(t *testing.T) {
	base := NewMemoryStorage()
	stack, err := base.CreateStack(t.Context(), &CreateStackRequest{StackName: "change-set-revision-race", TemplateBody: changeSetBaseTemplate})
	if err != nil {
		t.Fatal(err)
	}
	if err := base.RunStackOperation(t.Context(), stack.StackID, func(_ context.Context, stack *Stack) error {
		stack.StackStatus = StackStatusCreateComplete
		stack.EvaluatedState = &template.Evaluation{Parameters: map[string]string{"Environment": changeSetDefaultEnvironment}, Resources: map[string]template.Resource{}}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	storage := &changeSetRevisionRaceStorage{MemoryStorage: base}
	svc := New(storage)
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("CreateChangeSet", `{"StackName":"change-set-revision-race","ChangeSetName":"race","ChangeSetType":"UPDATE","TemplateBody":`+strconv.Quote(changeSetBaseTemplate)+`}`))
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "StaleChangeSetException") {
		t.Fatalf("CreateChangeSet() status/body = %d/%s, want stale revision rejection", rec.Code, rec.Body.String())
	}
	if _, err := base.GetChangeSet(t.Context(), stack.StackID, "race"); err == nil {
		t.Fatal("stale change set was persisted")
	}
}

//nolint:wsl // The table-driven test keeps request variants and state assertions together.
func TestExecuteChangeSetDisableRollbackDefaultsTrueAndHonorsExplicitFalse(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		disableRollback string
		wantDisableRoll bool
	}{
		{name: "default", wantDisableRoll: true},
		{name: "explicit-false", disableRollback: `,"DisableRollback":false`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			storage := NewMemoryStorage()
			svc := New(storage)
			createStackForChangeSet(t, svc, "rollback-"+testCase.name, changeSetBaseTemplate)
			templateBody := `{"Parameters":{"Environment":{"Type":"String","Default":"dev"}},"Resources":{"Bucket":{"Type":"AWS::S3::Bucket"}}}`
			body := `{"StackName":"rollback-` + testCase.name + `","ChangeSetName":"update","ChangeSetType":"UPDATE","TemplateBody":` + strconv.Quote(templateBody) + `}`
			create := httptest.NewRecorder()
			svc.DispatchAction(create, jsonRequest("CreateChangeSet", body))
			if create.Code != http.StatusOK {
				t.Fatalf("CreateChangeSet() status/body = %d/%s", create.Code, create.Body.String())
			}
			execute := httptest.NewRecorder()
			svc.DispatchAction(execute, jsonRequest("ExecuteChangeSet", `{"StackName":"rollback-`+testCase.name+`","ChangeSetName":"update"`+testCase.disableRollback+`}`))
			if execute.Code != http.StatusOK {
				t.Fatalf("ExecuteChangeSet() status/body = %d/%s", execute.Code, execute.Body.String())
			}
			stack, err := storage.GetStack(t.Context(), "rollback-"+testCase.name)
			if err != nil {
				t.Fatal(err)
			}
			if stack.DisableRollback != testCase.wantDisableRoll {
				t.Fatalf("DisableRollback = %v, want %v", stack.DisableRollback, testCase.wantDisableRoll)
			}
		})
	}
}

//nolint:wsl // The test compares both idempotent responses at one assertion boundary.
func TestExecuteChangeSetReplaysFailedClientToken(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	createStackForChangeSet(t, svc, "failed-execution-replay", changeSetBaseTemplate)
	invalidTemplate := `{"Parameters":{"Environment":{"Type":"String","Default":"dev"}},"Resources":{"Bucket":{"Type":"AWS::S3::Bucket","Properties":{"BucketName":123}}}}`
	create := httptest.NewRecorder()
	svc.DispatchAction(create, jsonRequest("CreateChangeSet", `{"StackName":"failed-execution-replay","ChangeSetName":"invalid","ChangeSetType":"UPDATE","TemplateBody":`+strconv.Quote(invalidTemplate)+`}`))
	if create.Code != http.StatusOK {
		t.Fatalf("CreateChangeSet() status/body = %d/%s", create.Code, create.Body.String())
	}

	first := httptest.NewRecorder()
	svc.DispatchAction(first, jsonRequest("ExecuteChangeSet", `{"StackName":"failed-execution-replay","ChangeSetName":"invalid","ClientRequestToken":"failed-token"}`))
	second := httptest.NewRecorder()
	svc.DispatchAction(second, jsonRequest("ExecuteChangeSet", `{"StackName":"failed-execution-replay","ChangeSetName":"invalid","ClientRequestToken":"failed-token"}`))
	if first.Code == http.StatusOK || second.Code == http.StatusOK {
		t.Fatalf("ExecuteChangeSet() statuses = %d/%d, want repeated failure", first.Code, second.Code)
	}
	if errorMessage(first.Body.Bytes()) != errorMessage(second.Body.Bytes()) {
		t.Fatalf("replayed error messages = %q/%q, want identical recorded result", errorMessage(first.Body.Bytes()), errorMessage(second.Body.Bytes()))
	}
}

//nolint:wsl // The failure injection and atomic-state assertions are one lifecycle boundary.
func TestExecuteChangeSetCompensatesLifecycleWhenTerminalMarkFails(t *testing.T) {
	base := NewMemoryStorage()
	storage := &changeSetMarkFailureStorage{MemoryStorage: base}
	svc := New(storage)
	createStackForChangeSet(t, svc, "change-set-mark-atomic", changeSetBaseTemplate)
	templateBody := `{"Parameters":{"Environment":{"Type":"String","Default":"dev"}},"Resources":{}}`
	create := httptest.NewRecorder()
	svc.DispatchAction(create, jsonRequest("CreateChangeSet", `{"StackName":"change-set-mark-atomic","ChangeSetName":"atomic","ChangeSetType":"UPDATE","TemplateBody":`+strconv.Quote(templateBody)+`,"Parameters":[{"ParameterKey":"Environment","ParameterValue":"prod"}]}`))
	if create.Code != http.StatusOK {
		t.Fatalf("CreateChangeSet() status/body = %d/%s", create.Code, create.Body.String())
	}
	beforeStack, err := base.GetStack(t.Context(), "change-set-mark-atomic")
	if err != nil {
		t.Fatal(err)
	}
	storage.fail.Store(true)

	execute := httptest.NewRecorder()
	svc.DispatchAction(execute, jsonRequest("ExecuteChangeSet", `{"StackName":"change-set-mark-atomic","ChangeSetName":"atomic"}`))
	if execute.Code == http.StatusOK {
		t.Fatalf("ExecuteChangeSet() status/body = %d/%s, want terminal-mark failure", execute.Code, execute.Body.String())
	}
	afterStack, err := base.GetStack(t.Context(), "change-set-mark-atomic")
	if err != nil {
		t.Fatal(err)
	}
	if afterStack.Revision != beforeStack.Revision || afterStack.EvaluatedState.Parameters["Environment"] != changeSetDefaultEnvironment {
		t.Fatalf("stack after failed mark = %#v, want lifecycle state and revision restored", afterStack)
	}
	changeSet, err := base.GetChangeSet(t.Context(), "change-set-mark-atomic", "atomic")
	if err != nil {
		t.Fatal(err)
	}
	if changeSet.Status != ChangeSetStatusCreateComplete || changeSet.ExecutionStatus != ChangeSetExecutionAvailable {
		t.Fatalf("change set after failed mark = %#v, want available review", changeSet)
	}
}

//nolint:wsl // The test verifies adapter state across a complete change-set transaction.
func TestExecuteChangeSetCompensatesNonEmptyLifecycleWhenTerminalMarkFails(t *testing.T) {
	base := NewMemoryStorage()
	storage := &changeSetMarkFailureStorage{MemoryStorage: base}
	adapter := newStatefulExecutorAdapter(executorResourceType)
	registry := resource.NewRegistry()
	registry.Register(adapter)
	svc := NewWithRegistry(storage, registry)
	createStackForChangeSet(t, svc, "change-set-mark-resources", changeSetBaseTemplate)
	stack, err := base.GetStack(t.Context(), "change-set-mark-resources")
	if err != nil {
		t.Fatal(err)
	}
	oldEvaluation := &template.Evaluation{Resources: map[string]template.Resource{
		changeSetUpdateLogicalID: {Type: executorResourceType, Properties: map[string]any{changeSetLabelProperty: changeSetOldValue}},
		changeSetDeleteLogicalID: {Type: executorResourceType, Properties: map[string]any{changeSetNameProperty: changeSetDeleteName}},
	}}
	adapter.seed("a-update-physical", map[string]any{changeSetLabelProperty: changeSetOldValue})
	adapter.seed("a-delete-physical", map[string]any{changeSetNameProperty: changeSetDeleteName})
	if err := base.RunStackOperation(t.Context(), stack.StackID, func(_ context.Context, working *Stack) error {
		working.TemplateBody = changeSetBaseTemplate
		working.EvaluatedState = oldEvaluation
		working.Resources = []StackResource{
			{LogicalResourceID: changeSetUpdateLogicalID, PhysicalResourceID: "a-update-physical", ResourceType: executorResourceType, ResourceStatus: ResourceStatusCreateComplete, StackID: working.StackID, StackName: working.StackName},
			{LogicalResourceID: changeSetDeleteLogicalID, PhysicalResourceID: "a-delete-physical", ResourceType: executorResourceType, ResourceStatus: ResourceStatusCreateComplete, StackID: working.StackID, StackName: working.StackName},
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	beforeStack, err := base.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	templateBody := `{"Resources":{"AUpdate":{"Type":"` + executorResourceType + `","Properties":{"Label":"new"}},"Added":{"Type":"` + executorResourceType + `","Properties":{"Name":"added"}}}}`
	create := httptest.NewRecorder()
	svc.DispatchAction(create, jsonRequest("CreateChangeSet", `{"StackName":"change-set-mark-resources","ChangeSetName":"resources","ChangeSetType":"UPDATE","TemplateBody":`+strconv.Quote(templateBody)+`}`))
	if create.Code != http.StatusOK {
		t.Fatalf("CreateChangeSet() status/body = %d/%s", create.Code, create.Body.String())
	}
	storage.fail.Store(true)

	execute := httptest.NewRecorder()
	svc.DispatchAction(execute, jsonRequest("ExecuteChangeSet", `{"StackName":"change-set-mark-resources","ChangeSetName":"resources"}`))
	if execute.Code == http.StatusOK {
		t.Fatalf("ExecuteChangeSet() status/body = %d/%s, want terminal-mark failure", execute.Code, execute.Body.String())
	}
	afterStack, err := base.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if afterStack.Revision != beforeStack.Revision || !reflect.DeepEqual(afterStack.EvaluatedState, beforeStack.EvaluatedState) || !reflect.DeepEqual(afterStack.Resources, beforeStack.Resources) {
		t.Fatalf("stack after failed mark = %#v, want logical snapshot restored", afterStack)
	}
	if adapter.exists("added-physical") || !adapter.exists("a-delete-physical") || adapter.propertiesFor("a-update-physical")[changeSetLabelProperty] != changeSetOldValue {
		t.Fatalf("backend resources after failed mark = %#v, want pre-execution state", adapter.resources)
	}
}

//nolint:wsl // The test keeps idempotency and sorted describe assertions together.
func TestCreateChangeSetClientTokenIdempotencyAndDescribeOrdering(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	createStackForChangeSet(t, svc, "idempotent-change-set-stack", changeSetBaseTemplate)
	templateBody := `{"Parameters":{"Environment":{"Type":"String","Default":"dev"}},"Resources":{"ZBucket":{"Type":"AWS::S3::Bucket"},"ABucket":{"Type":"AWS::S3::Bucket"}}}`
	body := `{"StackName":"idempotent-change-set-stack","ChangeSetName":"ordered","ChangeSetType":"UPDATE","TemplateBody":` + strconv.Quote(templateBody) + `,"ClientToken":"same-change-set-token"}`
	first := httptest.NewRecorder()
	svc.DispatchAction(first, jsonRequest("CreateChangeSet", body))
	second := httptest.NewRecorder()
	svc.DispatchAction(second, jsonRequest("CreateChangeSet", body))
	if first.Code != http.StatusOK || second.Code != http.StatusOK || changeSetIDFromResponse(t, first.Body.Bytes()) != changeSetIDFromResponse(t, second.Body.Bytes()) {
		t.Fatalf("idempotent CreateChangeSet responses = %d/%s and %d/%s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}

	describe := httptest.NewRecorder()
	svc.DispatchAction(describe, jsonRequest("DescribeChangeSet", `{"StackName":"idempotent-change-set-stack","ChangeSetName":"ordered"}`))
	if describe.Code != http.StatusOK || strings.Index(describe.Body.String(), "ABucket") > strings.Index(describe.Body.String(), "ZBucket") {
		t.Fatalf("DescribeChangeSet() status/body = %d/%s, want sorted resource changes", describe.Code, describe.Body.String())
	}

	conflict := httptest.NewRecorder()
	svc.DispatchAction(conflict, jsonRequest("CreateChangeSet", `{"StackName":"idempotent-change-set-stack","ChangeSetName":"different","ChangeSetType":"UPDATE","TemplateBody":`+strconv.Quote(changeSetBaseTemplate)+`,"ClientToken":"same-change-set-token"}`))
	if conflict.Code == http.StatusOK || !strings.Contains(conflict.Body.String(), "TokenAlreadyExistsException") {
		t.Fatalf("conflicting CreateChangeSet() status/body = %d/%s, want token conflict", conflict.Code, conflict.Body.String())
	}
}

//nolint:wsl // The helper keeps common stack creation request setup concise.
func createStackForChangeSet(t *testing.T, svc *Service, name, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("CreateStack", `{"StackName":"`+name+`","TemplateBody":`+strconv.Quote(body)+`}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateStack() status/body = %d/%s", rec.Code, rec.Body.String())
	}
}

//nolint:wsl // XML decoding and the single identifier return form one helper.
func changeSetIDFromResponse(t *testing.T, body []byte) string {
	t.Helper()
	var response struct {
		Result struct {
			ID string `xml:"Id"`
		} `xml:"CreateChangeSetResult"`
	}
	if err := xml.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}

	return response.Result.ID
}

func errorMessage(body []byte) string {
	var response XMLErrorResponse
	if err := xml.Unmarshal(body, &response); err != nil {
		return ""
	}

	return response.Error.Code + ":" + response.Error.Message
}

type changeSetRevisionRaceStorage struct {
	*MemoryStorage
	mutated bool
}

type changeSetMarkFailureStorage struct {
	*MemoryStorage
	fail atomic.Bool
}

func (s *changeSetMarkFailureStorage) MarkChangeSetExecution(context.Context, string, string, bool, string) error {
	if s.fail.Swap(false) {
		return errors.New("mark change set persistence failed")
	}

	return nil
}

func (s *changeSetRevisionRaceStorage) CreateChangeSet(ctx context.Context, req *CreateChangeSetRequest, evaluation *template.Evaluation) (*ChangeSet, error) {
	if !s.mutated {
		s.mutated = true
		if err := s.RunStackOperation(ctx, req.StackName, func(_ context.Context, stack *Stack) error {
			stack.StackStatusReason = "changed during review"

			return nil
		}); err != nil {
			return nil, err
		}
	}

	return s.MemoryStorage.CreateChangeSet(ctx, req, evaluation)
}
