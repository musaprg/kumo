//nolint:goconst // Handler fixtures intentionally reuse logical IDs and export names.
package cloudformation

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sivchari/kumo/internal/service/cloudformation/template"
)

const handlerTestTemplate = `{"Parameters":{"Environment":{"Type":"String","Default":"dev"}},"Resources":{}}`

//nolint:wsl // The test keeps lifecycle setup and assertions together.
func TestCreateStackDecodesParametersAndRunsExecutor(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	req := jsonRequest("CreateStack", `{"StackName":"handler-stack","TemplateBody":`+strconv.Quote(handlerTestTemplate)+`,"ClientRequestToken":"create-token","Capabilities":["CAPABILITY_IAM"],"DisableRollback":true,"Parameters":[{"ParameterKey":"Environment","ParameterValue":"`+testParameterProd+`"}]}`)
	rec := httptest.NewRecorder()

	svc.DispatchAction(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("CreateStack() status = %d, body = %s", rec.Code, rec.Body.String())
	}
	stack, err := storage.GetStack(t.Context(), "handler-stack")
	if err != nil {
		t.Fatal(err)
	}
	if stack.Parameters["Environment"] != testParameterProd || stack.EvaluatedState.Parameters["Environment"] != testParameterProd {
		t.Fatalf("stored parameters = %#v, evaluated parameters = %#v", stack.Parameters, stack.EvaluatedState.Parameters)
	}
	if !hasEventToken(stack.Events, "create-token") {
		t.Fatalf("events = %#v, want executor event with client token", stack.Events)
	}
}

//nolint:wsl // The test keeps stale-update setup and assertions together.
func TestUpdateStackRejectsRevisionChangedAfterEvaluation(t *testing.T) {
	base := NewMemoryStorage()
	stack, err := base.CreateStack(t.Context(), &CreateStackRequest{StackName: "stale-update-stack", TemplateBody: handlerTestTemplate})
	if err != nil {
		t.Fatal(err)
	}
	storage := &revisionChangingStorage{MemoryStorage: base}
	if err := NewExecutor(base, nil).Create(t.Context(), LifecycleRequest{StackRef: stack.StackID, Evaluation: &template.Evaluation{Resources: map[string]template.Resource{}}}); err != nil {
		t.Fatal(err)
	}
	storage.changeAfterFirstRead = true

	rec := httptest.NewRecorder()
	New(storage).DispatchAction(rec, jsonRequest("UpdateStack", `{"StackName":"stale-update-stack","TemplateBody":`+strconv.Quote(handlerTestTemplate)+`}`))
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), revisionConflictException) {
		t.Fatalf("UpdateStack() status/body = %d/%s, want revision conflict", rec.Code, rec.Body.String())
	}
	updated, err := base.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.StackStatus != StackStatusCreateComplete {
		t.Fatalf("stale update status = %q, want unchanged create-complete", updated.StackStatus)
	}
}

type revisionChangingStorage struct {
	*MemoryStorage
	mu                   sync.Mutex
	changeAfterFirstRead bool
	changed              bool
}

//nolint:wsl // The embedded storage is intentionally exercised through the wrapper.
func (s *revisionChangingStorage) GetStack(ctx context.Context, stackRef string) (*Stack, error) {
	stack, err := s.MemoryStorage.GetStack(ctx, stackRef)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	shouldChange := s.changeAfterFirstRead && !s.changed
	if shouldChange {
		s.changed = true
	}
	s.mu.Unlock()
	if shouldChange {
		if err := s.RunStackOperation(ctx, stack.StackID, func(context.Context, *Stack) error { return nil }); err != nil {
			return nil, err
		}
	}

	return stack, nil
}

//nolint:wsl // The test keeps effective-parameter assertions together.
func TestCreateStackPersistsEffectiveParameterDefaults(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	rec := httptest.NewRecorder()

	svc.DispatchAction(rec, jsonRequest("CreateStack", `{"StackName":"default-parameter-stack","TemplateBody":`+strconv.Quote(handlerTestTemplate)+`}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("CreateStack() status = %d, body = %s", rec.Code, rec.Body.String())
	}
	stack, err := storage.GetStack(t.Context(), "default-parameter-stack")
	if err != nil {
		t.Fatal(err)
	}
	if stack.Parameters["Environment"] != "dev" {
		t.Fatalf("stored parameters = %#v, want effective default", stack.Parameters)
	}
}

//nolint:wsl // The test keeps lifecycle metadata assertions together.
func TestLifecycleMetadataCommitsWithCreateResult(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	rec := httptest.NewRecorder()
	body := `{"StackName":"metadata-stack","TemplateBody":` + strconv.Quote(handlerTestTemplate) + `,"RoleARN":"arn:aws:iam::123456789012:role/cfn","Tags":[{"Key":"env","Value":"prod"}],"DeploymentConfig":{"DisableRollback":true}}`
	svc.DispatchAction(rec, jsonRequest("CreateStack", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateStack() status/body = %d/%s", rec.Code, rec.Body.String())
	}
	stack, err := storage.GetStack(t.Context(), "metadata-stack")
	if err != nil {
		t.Fatal(err)
	}
	if stack.RoleARN == "" || len(stack.Tags) != 1 || stack.DeploymentConfig == nil || !stack.DeploymentConfig.DisableRollback {
		t.Fatalf("stack metadata = %#v, want lifecycle metadata committed with result", stack)
	}
}

//nolint:wsl // The test keeps failed-create state assertions together.
func TestCreateStackDoesNotLeaveInvalidTemplateComplete(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	rec := httptest.NewRecorder()

	svc.DispatchAction(rec, jsonRequest("CreateStack", `{"StackName":"invalid-template-stack","TemplateBody":"{}"}`))

	if rec.Code == http.StatusOK {
		t.Fatalf("CreateStack() status = %d, want failure", rec.Code)
	}
	stack, err := storage.GetStack(t.Context(), "invalid-template-stack")
	if err != nil {
		t.Fatal(err)
	}
	if stack.StackStatus == StackStatusCreateComplete || !containsEventStatus(stack.Events, StackStatusCreateFailed) {
		t.Fatalf("failed create stack = %#v, want failed status and event", stack)
	}
}

//nolint:wsl // The test keeps capability error and failed-state assertions together.
func TestCreateStackCapabilityFailureUsesCloudFormationErrorCode(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	rec := httptest.NewRecorder()
	templateBody := `{"Resources":{"Role":{"Type":"AWS::IAM::Role","Properties":{"AssumeRolePolicyDocument":{"Version":"2012-10-17"}}}}}`

	svc.DispatchAction(rec, jsonRequest("CreateStack", `{"StackName":"capability-stack","TemplateBody":`+strconv.Quote(templateBody)+`}`))

	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "<Code>InsufficientCapabilities</Code>") {
		t.Fatalf("CreateStack() status/body = %d/%s, want InsufficientCapabilities XML", rec.Code, rec.Body.String())
	}
	stack, err := storage.GetStack(t.Context(), "capability-stack")
	if err != nil {
		t.Fatal(err)
	}
	if stack.StackStatus == StackStatusCreateComplete {
		t.Fatalf("capability failure stack = %#v, must not be CREATE_COMPLETE", stack)
	}
}

func TestCreateStackRejectsUsePreviousValue(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	rec := httptest.NewRecorder()

	svc.DispatchAction(rec, jsonRequest("CreateStack", `{"StackName":"create-previous-value","TemplateBody":`+strconv.Quote(handlerTestTemplate)+`,"Parameters":[{"ParameterKey":"Environment","UsePreviousValue":true}]}`))

	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "UsePreviousValue") {
		t.Fatalf("CreateStack() status/body = %d/%s, want UsePreviousValue rejection", rec.Code, rec.Body.String())
	}
}

//nolint:wsl // The test keeps lifecycle setup and assertions together.
func TestUpdateStackUsesPreviousTemplateAndParameterValues(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	create := jsonRequest("CreateStack", `{"StackName":"previous-stack","TemplateBody":`+strconv.Quote(handlerTestTemplate)+`,"Parameters":[{"ParameterKey":"Environment","ParameterValue":"`+testParameterProd+`"}]}`)
	createRec := httptest.NewRecorder()
	svc.DispatchAction(createRec, create)
	if createRec.Code != http.StatusOK {
		t.Fatalf("CreateStack() status = %d, body = %s", createRec.Code, createRec.Body.String())
	}

	update := jsonRequest("UpdateStack", `{"StackName":"previous-stack","UsePreviousTemplate":true,"ClientRequestToken":"update-token","Parameters":[{"ParameterKey":"Environment","UsePreviousValue":true}]}`)
	updateRec := httptest.NewRecorder()
	svc.DispatchAction(updateRec, update)

	if updateRec.Code != http.StatusOK {
		t.Fatalf("UpdateStack() status = %d, body = %s", updateRec.Code, updateRec.Body.String())
	}
	stack, err := storage.GetStack(t.Context(), "previous-stack")
	if err != nil {
		t.Fatal(err)
	}
	if stack.TemplateBody != handlerTestTemplate || stack.EvaluatedState.Parameters["Environment"] != testParameterProd {
		t.Fatalf("updated stack template/parameters = %q/%#v", stack.TemplateBody, stack.EvaluatedState.Parameters)
	}
	if !hasEventToken(stack.Events, "update-token") {
		t.Fatalf("events = %#v, want executor update event", stack.Events)
	}
}

//nolint:wsl // The test keeps protection preservation and deletion rejection together.
func TestUpdateStackPreservesTerminationProtectionAndDeleteRejectsProtectedStack(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	createRec := httptest.NewRecorder()
	svc.DispatchAction(createRec, jsonRequest("CreateStack", `{"StackName":"protected-stack","TemplateBody":`+strconv.Quote(handlerTestTemplate)+`,"EnableTerminationProtection":true}`))
	if createRec.Code != http.StatusOK {
		t.Fatalf("CreateStack() status/body = %d/%s", createRec.Code, createRec.Body.String())
	}

	updateRec := httptest.NewRecorder()
	svc.DispatchAction(updateRec, jsonRequest("UpdateStack", `{"StackName":"protected-stack","UsePreviousTemplate":true,"ClientRequestToken":"protected-update"}`))
	if updateRec.Code != http.StatusOK {
		t.Fatalf("UpdateStack() status/body = %d/%s", updateRec.Code, updateRec.Body.String())
	}
	updated, err := storage.GetStack(t.Context(), "protected-stack")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.EnableTerminationProtection {
		t.Fatal("UpdateStack() cleared existing termination protection")
	}

	deleteRec := httptest.NewRecorder()
	svc.DispatchAction(deleteRec, jsonRequest("DeleteStack", `{"StackName":"protected-stack","ClientRequestToken":"protected-delete"}`))
	if deleteRec.Code == http.StatusOK || !strings.Contains(deleteRec.Body.String(), "Termination protection") {
		t.Fatalf("DeleteStack() status/body = %d/%s, want termination-protection rejection", deleteRec.Code, deleteRec.Body.String())
	}
	unchanged, err := storage.GetStack(t.Context(), "protected-stack")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.StackStatus == StackStatusDeleteComplete {
		t.Fatal("DeleteStack() deleted a protected stack")
	}
}

//nolint:wsl // The test keeps conflicting-input setup and rejection together.
func TestUpdateStackRejectsConflictingTemplateInputs(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	createRec := httptest.NewRecorder()
	svc.DispatchAction(createRec, jsonRequest("CreateStack", `{"StackName":"conflicting-template-stack","TemplateBody":`+strconv.Quote(handlerTestTemplate)+`}`))
	if createRec.Code != http.StatusOK {
		t.Fatalf("CreateStack() status/body = %d/%s", createRec.Code, createRec.Body.String())
	}

	updateRec := httptest.NewRecorder()
	svc.DispatchAction(updateRec, jsonRequest("UpdateStack", `{"StackName":"conflicting-template-stack","TemplateBody":`+strconv.Quote(handlerTestTemplate)+`,"UsePreviousTemplate":true}`))
	if updateRec.Code == http.StatusOK || !strings.Contains(updateRec.Body.String(), "UsePreviousTemplate") {
		t.Fatalf("UpdateStack() status/body = %d/%s, want conflicting template rejection", updateRec.Code, updateRec.Body.String())
	}
}

//nolint:wsl // The test keeps lifecycle setup and assertions together.
func TestDeleteStackRunsExecutorAndPreservesTombstoneReads(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "delete-handler-stack", TemplateBody: handlerTestTemplate})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.SetEvaluatedState(t.Context(), stack.StackID, &template.Evaluation{Resources: map[string]template.Resource{}}); err != nil {
		t.Fatal(err)
	}

	req := jsonRequest("DeleteStack", `{"StackName":"`+stack.StackID+`","ClientRequestToken":"delete-token"}`)
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("DeleteStack() status = %d, body = %s", rec.Code, rec.Body.String())
	}
	deleted, err := storage.GetStack(t.Context(), stack.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.StackStatus != StackStatusDeleteComplete || !hasEventToken(deleted.Events, "delete-token") {
		t.Fatalf("deleted stack = %#v, want tombstone and executor token", deleted)
	}
}

//nolint:wsl // The test keeps read setup and XML assertions together.
func TestReadHandlersReturnSortedEventsResourcesAndExports(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "read-handler-stack", TemplateBody: handlerTestTemplate})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.RunStackOperation(t.Context(), stack.StackID, func(_ context.Context, working *Stack) error {
		working.Resources = []StackResource{
			{LogicalResourceID: "ZResource", PhysicalResourceID: "z", ResourceType: "Example::Z", ResourceStatus: ResourceStatusCreateComplete, Timestamp: time.Unix(2, 0), StackID: working.StackID, StackName: working.StackName},
			{LogicalResourceID: "AResource", PhysicalResourceID: "a", ResourceType: "Example::A", ResourceStatus: ResourceStatusCreateComplete, Timestamp: time.Unix(1, 0), StackID: working.StackID, StackName: working.StackName},
		}
		working.Events = []StackEvent{
			{EventID: "event-z", StackID: working.StackID, StackName: working.StackName, ResourceStatus: StackStatusUpdateComplete, Timestamp: time.Unix(2, 0)},
			{EventID: "event-a", StackID: working.StackID, StackName: working.StackName, ResourceStatus: StackStatusCreateComplete, Timestamp: time.Unix(1, 0)},
		}
		working.Exports = []StackExport{
			{Name: "ZExport", Value: "z", ExporterStackARN: working.StackID},
			{Name: testSortedAExport, Value: "a", ExporterStackARN: working.StackID},
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	eventsRec := httptest.NewRecorder()
	svc.DispatchAction(eventsRec, jsonRequest("DescribeStackEvents", `{"StackName":"`+stack.StackID+`"}`))
	assertOrderedXML(t, eventsRec, "event-z", "event-a")

	resourceRec := httptest.NewRecorder()
	svc.DispatchAction(resourceRec, jsonRequest("DescribeStackResource", `{"StackName":"`+stack.StackID+`","LogicalResourceId":"AResource"}`))
	if resourceRec.Code != http.StatusOK || !strings.Contains(resourceRec.Body.String(), "<LogicalResourceId>AResource</LogicalResourceId>") {
		t.Fatalf("DescribeStackResource() status/body = %d/%s", resourceRec.Code, resourceRec.Body.String())
	}

	exportsRec := httptest.NewRecorder()
	svc.DispatchAction(exportsRec, jsonRequest("ListExports", `{}`))
	assertOrderedXML(t, exportsRec, "AExport", "ZExport")
}

//nolint:wsl // The test keeps unsupported-pagination setup and rejection together.
func TestDescribeStackEventsRejectsUnsupportedNextToken(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "events-next-token", TemplateBody: handlerTestTemplate})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("DescribeStackEvents", `{"StackName":"`+stack.StackID+`","NextToken":"opaque"}`))
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "NextToken") {
		t.Fatalf("DescribeStackEvents() status/body = %d/%s, want explicit NextToken rejection", rec.Code, rec.Body.String())
	}
}

//nolint:wsl // The test keeps tombstone setup and read assertions together.
func TestReadHandlersPreserveDeletedStackTombstone(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "tombstone-read-stack", TemplateBody: handlerTestTemplate})
	if err != nil {
		t.Fatal(err)
	}

	deleteRec := httptest.NewRecorder()
	svc.DispatchAction(deleteRec, jsonRequest("DeleteStack", `{"StackName":"`+stack.StackID+`","ClientRequestToken":"tombstone-delete"}`))
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("DeleteStack() status/body = %d/%s", deleteRec.Code, deleteRec.Body.String())
	}

	listRec := httptest.NewRecorder()
	svc.DispatchAction(listRec, jsonRequest("ListStacks", `{"StackStatusFilter":["DELETE_COMPLETE"]}`))
	if listRec.Code != http.StatusOK || !strings.Contains(listRec.Body.String(), stack.StackID) || !strings.Contains(listRec.Body.String(), "DELETE_COMPLETE") {
		t.Fatalf("ListStacks() status/body = %d/%s", listRec.Code, listRec.Body.String())
	}

	eventsRec := httptest.NewRecorder()
	svc.DispatchAction(eventsRec, jsonRequest("DescribeStackEvents", `{"StackName":"`+stack.StackID+`"}`))
	if eventsRec.Code != http.StatusOK || !strings.Contains(eventsRec.Body.String(), "tombstone-delete") || !strings.Contains(eventsRec.Body.String(), "DELETE_COMPLETE") {
		t.Fatalf("DescribeStackEvents() status/body = %d/%s", eventsRec.Code, eventsRec.Body.String())
	}
}

//nolint:wsl // The test keeps the rejection assertion beside the request.
func TestValidateTemplateRejectsTemplateURL(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("ValidateTemplate", `{"TemplateURL":"https://example.com/template.yaml"}`))
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "TemplateURL") {
		t.Fatalf("ValidateTemplate() status/body = %d/%s, want explicit TemplateURL rejection", rec.Code, rec.Body.String())
	}
}

//nolint:wsl // The test keeps valid and invalid validation assertions together.
func TestValidateTemplateAcceptsYAMLAndRejectsUnsupportedStructure(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	yamlRequest := jsonRequest("ValidateTemplate", `{"TemplateBody":"Description: YAML template\nResources:\n  Bucket:\n    Type: AWS::S3::Bucket\n"}`)
	yamlResponse := httptest.NewRecorder()
	svc.DispatchAction(yamlResponse, yamlRequest)
	if yamlResponse.Code != http.StatusOK || !strings.Contains(yamlResponse.Body.String(), "YAML template") {
		t.Fatalf("ValidateTemplate(YAML) status/body = %d/%s", yamlResponse.Code, yamlResponse.Body.String())
	}

	invalidResponse := httptest.NewRecorder()
	svc.DispatchAction(invalidResponse, jsonRequest("ValidateTemplate", `{"TemplateBody":"Resources: []"}`))
	if invalidResponse.Code == http.StatusOK || !strings.Contains(invalidResponse.Body.String(), "mapping") {
		t.Fatalf("ValidateTemplate(invalid) status/body = %d/%s", invalidResponse.Code, invalidResponse.Body.String())
	}
}

//nolint:wsl // The test keeps output state setup beside its serialization assertion.
func TestDescribeStacksSerializesEvaluatedOutputsInNameOrder(t *testing.T) {
	storage := NewMemoryStorage()
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "outputs-stack", TemplateBody: handlerTestTemplate})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.SetEvaluatedState(t.Context(), stack.StackID, &template.Evaluation{Outputs: map[string]template.Output{
		"Zeta":  {Value: "z", ExportName: "export-z"},
		"Alpha": {Value: "a", ExportName: "export-a"},
	}}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	New(storage).DispatchAction(rec, jsonRequest("DescribeStacks", `{"StackName":"outputs-stack"}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<Outputs>") || strings.Index(rec.Body.String(), "Alpha") > strings.Index(rec.Body.String(), "Zeta") {
		t.Fatalf("DescribeStacks() status/body = %d/%s", rec.Code, rec.Body.String())
	}
}

//nolint:wsl // The test keeps the replay and conflict requests beside their shared token.
func TestCreateStackReplaysIdenticalClientTokenBeforeNameReservation(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	body := `{"StackName":"create-replay-stack","TemplateBody":` + strconv.Quote(handlerTestTemplate) + `,"ClientRequestToken":"create-replay-token"}`
	first := httptest.NewRecorder()
	svc.DispatchAction(first, jsonRequest("CreateStack", body))
	if first.Code != http.StatusOK {
		t.Fatalf("initial CreateStack() status/body = %d/%s", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	svc.DispatchAction(second, jsonRequest("CreateStack", body))
	if second.Code != http.StatusOK {
		t.Fatalf("replayed CreateStack() status/body = %d/%s", second.Code, second.Body.String())
	}
	var firstResponse, secondResponse XMLCreateStackResponse
	if err := xml.Unmarshal(first.Body.Bytes(), &firstResponse); err != nil {
		t.Fatal(err)
	}
	if err := xml.Unmarshal(second.Body.Bytes(), &secondResponse); err != nil {
		t.Fatal(err)
	}
	if firstResponse.Result.StackID == "" || firstResponse.Result.StackID != secondResponse.Result.StackID {
		t.Fatalf("replayed stack IDs = %q/%q, want identical IDs", firstResponse.Result.StackID, secondResponse.Result.StackID)
	}
	stack, err := storage.GetStack(t.Context(), firstResponse.Result.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stack.Events) != 2 {
		t.Fatalf("replayed create events = %#v, want no second lifecycle", stack.Events)
	}

	conflict := httptest.NewRecorder()
	svc.DispatchAction(conflict, jsonRequest("CreateStack", `{"StackName":"create-replay-stack","TemplateBody":"{}","ClientRequestToken":"create-replay-token"}`))
	if conflict.Code == http.StatusOK || !strings.Contains(conflict.Body.String(), "TokenAlreadyExistsException") {
		t.Fatalf("conflicting CreateStack() status/body = %d/%s, want token conflict", conflict.Code, conflict.Body.String())
	}
}

//nolint:wsl // The test keeps event-token setup and assertions together.
func TestCreateStackUsesOneGeneratedTokenForAllEvents(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("CreateStack", `{"StackName":"generated-token-stack","TemplateBody":`+strconv.Quote(handlerTestTemplate)+`}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateStack() status/body = %d/%s", rec.Code, rec.Body.String())
	}
	stack, err := storage.GetStack(t.Context(), "generated-token-stack")
	if err != nil {
		t.Fatal(err)
	}
	if len(stack.Events) == 0 || stack.Events[0].ClientRequestToken == "" {
		t.Fatalf("events = %#v, want generated client token", stack.Events)
	}
	token := stack.Events[0].ClientRequestToken
	for _, event := range stack.Events {
		if event.ClientRequestToken != token {
			t.Fatalf("event tokens = %#v, want one generated token %q", stack.Events, token)
		}
	}
}

//nolint:wsl // The test keeps validation and no-reservation assertions together.
func TestCreateStackRejectsInvalidOptionsBeforeReservation(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("CreateStack", `{"StackName":"invalid-options-stack","TemplateBody":`+strconv.Quote(handlerTestTemplate)+`,"OnFailure":"INVALID"}`))
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "failure policy") {
		t.Fatalf("CreateStack() status/body = %d/%s, want validation error", rec.Code, rec.Body.String())
	}
	if _, err := storage.GetStack(t.Context(), "invalid-options-stack"); err == nil {
		t.Fatal("invalid create option reserved a stack")
	}
}

//nolint:wsl // The test keeps failure response and tombstone assertions together.
func TestCreateStackOnFailureDeleteTombstonesPreflightFailure(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	templateBody := `{"Resources":{"Unsupported":{"Type":"Example::Unsupported"}}}`
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("CreateStack", `{"StackName":"preflight-delete-stack","TemplateBody":`+strconv.Quote(templateBody)+`,"OnFailure":"DELETE","ClientRequestToken":"preflight-delete-token"}`))
	if rec.Code == http.StatusOK {
		t.Fatal("CreateStack() succeeded with unsupported resource type")
	}
	stacks, err := storage.ListStacks(t.Context(), []string{StackStatusDeleteComplete})
	if err != nil {
		t.Fatal(err)
	}
	if len(stacks) != 1 || stacks[0].StackName != "preflight-delete-stack" || !hasEventToken(stacks[0].Events, "preflight-delete-token") {
		t.Fatalf("tombstones = %#v, want preflight DELETE_COMPLETE tombstone", stacks)
	}
	if len(stacks[0].Resources) != 0 {
		t.Fatalf("failed preflight tombstone resources = %#v, want no unprovisioned resources", stacks[0].Resources)
	}
}

//nolint:wsl // The test keeps evaluation failure and truthful resource state together.
func TestCreateStackEvaluationFailureDoesNotPersistFabricatedResources(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	templateBody := `{"Resources":{"Broken":{"Type":"Example::Thing","Properties":{"Value":{"Ref":"Missing"}}}}}`
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("CreateStack", `{"StackName":"evaluation-failure-stack","TemplateBody":`+strconv.Quote(templateBody)+`,"ClientRequestToken":"evaluation-failure-token"}`))
	if rec.Code == http.StatusOK {
		t.Fatal("CreateStack() succeeded with an unresolved template reference")
	}
	stack, err := storage.GetStack(t.Context(), "evaluation-failure-stack")
	if err != nil {
		t.Fatal(err)
	}
	if stack.StackStatus != StackStatusCreateFailed || len(stack.Resources) != 0 {
		t.Fatalf("failed evaluation stack = %#v, want CREATE_FAILED with no resources", stack)
	}
}

//nolint:wsl // The test keeps completion failure injection and replay assertions together.
func TestCreateStackReplaysAfterCompletionPersistenceFailure(t *testing.T) {
	storage := &failingCreateCompletionStorage{MemoryStorage: NewMemoryStorage(), fail: true}
	svc := New(storage)
	body := `{"StackName":"completion-recovery-stack","TemplateBody":` + strconv.Quote(handlerTestTemplate) + `,"ClientRequestToken":"completion-recovery-token"}`
	first := httptest.NewRecorder()
	svc.DispatchAction(first, jsonRequest("CreateStack", body))
	if first.Code == http.StatusOK {
		t.Fatalf("initial CreateStack() status/body = %d/%s, want injected completion failure", first.Code, first.Body.String())
	}
	committed, err := storage.GetStack(t.Context(), "completion-recovery-stack")
	if err != nil {
		t.Fatal(err)
	}
	if committed.StackStatus != StackStatusCreateComplete {
		t.Fatalf("committed stack status = %q, want CREATE_COMPLETE", committed.StackStatus)
	}

	second := httptest.NewRecorder()
	svc.DispatchAction(second, jsonRequest("CreateStack", body))
	if second.Code != http.StatusOK {
		t.Fatalf("replayed CreateStack() status/body = %d/%s", second.Code, second.Body.String())
	}
	recovered := storage.CreateExecutions["completion-recovery-token"]
	if !recovered.Completed || !recovered.Success {
		t.Fatalf("recovered create execution = %#v, want completed success", recovered)
	}
}

//nolint:wsl // The test keeps the original result and later lifecycle changes together.
func TestCreateStackReplaysOriginalResultAfterLaterUpdateAndDelete(t *testing.T) {
	storage := &failingCreateCompletionStorage{MemoryStorage: NewMemoryStorage(), fail: true}
	svc := New(storage)
	body := `{"StackName":"immutable-create-result","TemplateBody":` + strconv.Quote(handlerTestTemplate) + `,"ClientRequestToken":"immutable-create-token"}`
	first := httptest.NewRecorder()
	svc.DispatchAction(first, jsonRequest("CreateStack", body))
	if first.Code == http.StatusOK {
		t.Fatalf("initial CreateStack() status/body = %d/%s, want injected completion failure", first.Code, first.Body.String())
	}
	original, err := storage.GetStack(t.Context(), "immutable-create-result")
	if err != nil {
		t.Fatal(err)
	}

	update := httptest.NewRecorder()
	svc.DispatchAction(update, jsonRequest("UpdateStack", `{"StackName":"immutable-create-result","UsePreviousTemplate":true,"ClientRequestToken":"immutable-update-token"}`))
	if update.Code != http.StatusOK {
		t.Fatalf("UpdateStack() status/body = %d/%s", update.Code, update.Body.String())
	}
	deleteRec := httptest.NewRecorder()
	svc.DispatchAction(deleteRec, jsonRequest("DeleteStack", `{"StackName":"immutable-create-result","ClientRequestToken":"immutable-delete-token"}`))
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("DeleteStack() status/body = %d/%s", deleteRec.Code, deleteRec.Body.String())
	}

	replay := httptest.NewRecorder()
	svc.DispatchAction(replay, jsonRequest("CreateStack", body))
	if replay.Code != http.StatusOK {
		t.Fatalf("replayed CreateStack() status/body = %d/%s", replay.Code, replay.Body.String())
	}
	var response XMLCreateStackResponse
	if err := xml.Unmarshal(replay.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Result.StackID != original.StackID {
		t.Fatalf("replayed StackID = %q, want original %q", response.Result.StackID, original.StackID)
	}
}

//nolint:wsl // The test keeps the recovered CloudFormation error contract together.
func TestCreateStackRecoveryPreservesInsufficientCapabilitiesError(t *testing.T) {
	storage := &failingCreateCompletionStorage{MemoryStorage: NewMemoryStorage(), fail: true}
	svc := New(storage)
	templateBody := `{"Resources":{"Role":{"Type":"AWS::IAM::Role","Properties":{"AssumeRolePolicyDocument":{"Version":"2012-10-17"}}}}}`
	body := `{"StackName":"recovered-capability-error","TemplateBody":` + strconv.Quote(templateBody) + `,"ClientRequestToken":"recovered-capability-token"}`
	first := httptest.NewRecorder()
	svc.DispatchAction(first, jsonRequest("CreateStack", body))
	if first.Code == http.StatusOK || !strings.Contains(first.Body.String(), "<Code>InsufficientCapabilities</Code>") {
		t.Fatalf("initial CreateStack() status/body = %d/%s, want InsufficientCapabilities XML", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	svc.DispatchAction(second, jsonRequest("CreateStack", body))
	if second.Code == http.StatusOK || !strings.Contains(second.Body.String(), "<Code>InsufficientCapabilities</Code>") || !strings.Contains(second.Body.String(), "requires CAPABILITY_IAM") {
		t.Fatalf("recovered CreateStack() status/body = %d/%s, want original error code/message", second.Code, second.Body.String())
	}
}

//nolint:wsl // The test keeps the request and explicit pagination rejection together.
func TestListStacksRejectsUnsupportedNextToken(t *testing.T) {
	svc := New(NewMemoryStorage())
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("ListStacks", `{"NextToken":"opaque"}`))
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "NextToken") {
		t.Fatalf("ListStacks() status/body = %d/%s, want explicit pagination rejection", rec.Code, rec.Body.String())
	}
}

//nolint:wsl // The table keeps each unsupported wire field beside its request.
func TestGetTemplateRejectsUnsupportedChangeSetInputs(t *testing.T) {
	svc := New(NewMemoryStorage())
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "change set", body: `{"StackName":"stack","ChangeSetName":"change-set"}`},
		{name: "template stage", body: `{"StackName":"stack","TemplateStage":"Original"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			svc.DispatchAction(rec, jsonRequest("GetTemplate", test.body))
			if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "unsupported") {
				t.Fatalf("GetTemplate() status/body = %d/%s, want explicit unsupported-input rejection", rec.Code, rec.Body.String())
			}
		})
	}
}

//nolint:wsl // The table keeps each unsupported control beside its no-reservation assertion.
func TestCreateStackRejectsUnsupportedControlsBeforeReservation(t *testing.T) {
	for _, test := range []struct {
		name  string
		stack string
		field string
		value string
	}{
		{name: "rollback configuration", stack: "unsupported-rollback-configuration", field: "RollbackConfiguration", value: `{"MonitoringTimeInMinutes":10}`},
		{name: "stack policy body", stack: "unsupported-stack-policy-body", field: "StackPolicyBody", value: `"{}"`},
		{name: "stack policy URL", stack: "unsupported-stack-policy-url", field: "StackPolicyURL", value: `"https://example.com/policy"`},
		{name: "deployment mode", stack: "unsupported-deployment-mode", field: "DeploymentConfig", value: `{"Mode":"EXPRESS"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage := NewMemoryStorage()
			svc := New(storage)
			body := `{"StackName":"` + test.stack + `","TemplateBody":` + strconv.Quote(handlerTestTemplate) + `,"` + test.field + `":` + test.value + `}`
			rec := httptest.NewRecorder()
			svc.DispatchAction(rec, jsonRequest("CreateStack", body))
			if rec.Code == http.StatusOK || !strings.Contains(strings.ToLower(rec.Body.String()), "unsupported") {
				t.Fatalf("CreateStack() status/body = %d/%s, want unsupported-control rejection", rec.Code, rec.Body.String())
			}
			if _, err := storage.GetStack(t.Context(), test.stack); err == nil {
				t.Fatal("unsupported control reserved a stack")
			}
		})
	}
}

//nolint:wsl // The test keeps XML metadata assertions beside the response.
func TestDescribeStacksIncludesSupportedPersistedMetadata(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	body := `{"StackName":"describe-metadata-stack","TemplateBody":` + strconv.Quote(handlerTestTemplate) + `,"RoleARN":"arn:aws:iam::123456789012:role/cfn","NotificationARNs":["arn:aws:sns:us-east-1:123456789012:topic"],"Tags":[{"Key":"env","Value":"prod"}],"EnableTerminationProtection":true,"RetainExceptOnCreate":true,"DisableRollback":true,"DeploymentConfig":{"DisableRollback":true}}`
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("CreateStack", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateStack() status/body = %d/%s", rec.Code, rec.Body.String())
	}
	describe := httptest.NewRecorder()
	svc.DispatchAction(describe, jsonRequest("DescribeStacks", `{"StackName":"describe-metadata-stack"}`))
	if describe.Code != http.StatusOK {
		t.Fatalf("DescribeStacks() status/body = %d/%s", describe.Code, describe.Body.String())
	}
	for _, expected := range []string{"<RoleARN>arn:aws:iam::123456789012:role/cfn</RoleARN>", "<NotificationARNs>", "<Tags>", "<EnableTerminationProtection>true</EnableTerminationProtection>", "<RetainExceptOnCreate>true</RetainExceptOnCreate>", "<DisableRollback>true</DisableRollback>", "<DeploymentConfig>"} {
		if !strings.Contains(describe.Body.String(), expected) {
			t.Fatalf("DescribeStacks() body = %s, missing %q", describe.Body.String(), expected)
		}
	}
}

func jsonRequest(action, body string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-Amz-Target", "CloudFormation."+action)
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")

	return req
}

func hasEventToken(events []StackEvent, token string) bool {
	for index := range events {
		if events[index].ClientRequestToken == token {
			return true
		}
	}

	return false
}

//nolint:wsl // XML parsing and ordering assertions are one test helper boundary.
func assertOrderedXML(t *testing.T, rec *httptest.ResponseRecorder, first, second string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("response status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var root struct{}
	if err := xml.Unmarshal(rec.Body.Bytes(), &root); err != nil {
		t.Fatalf("response body is not XML text: %v", err)
	}
	firstIndex := strings.Index(rec.Body.String(), first)
	secondIndex := strings.Index(rec.Body.String(), second)
	if firstIndex < 0 || secondIndex < 0 || firstIndex >= secondIndex {
		t.Fatalf("XML body = %s, want %q before %q", rec.Body.String(), first, second)
	}
}

type failingCreateCompletionStorage struct {
	*MemoryStorage
	fail bool
}

func (s *failingCreateCompletionStorage) CompleteCreateExecution(ctx context.Context, token, stackID, errorCode, reason string, success bool) error {
	if s.fail {
		s.fail = false

		return errors.New("injected completion persistence failure")
	}

	return s.MemoryStorage.CompleteCreateExecution(ctx, token, stackID, errorCode, reason, success)
}
