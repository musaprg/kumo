package cloudformation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"

	"github.com/sivchari/kumo/internal/service/cloudformation/template"
)

// Error codes for CloudFormation.
const (
	errInvalidParameter   = "ValidationError"
	errInternalError      = "InternalServiceError"
	errInvalidAction      = "InvalidAction"
	errStackNotFound      = "StackNotFoundException"
	errTokenAlreadyExists = "TokenAlreadyExistsException"
)

const msgStackNotFound = "Stack not found"

const (
	msgStackNameRequired                 = "StackName is required"
	msgTemplateURLUnsupported            = "TemplateURL is unsupported"
	msgDeploymentModeUnsupported         = "DeploymentConfig.Mode is unsupported"
	msgCapabilitiesResourceTypesConflict = "Capabilities and ResourceTypes cannot both be specified"
	deletionModeStandard                 = "STANDARD"
	deletionModeForce                    = "FORCE_DELETE_STACK"
)

// CreateStack handles the CreateStack action.
//
//nolint:funlen,wsl,cyclop // Request evaluation and executor orchestration form one boundary.
func (s *Service) CreateStack(w http.ResponseWriter, r *http.Request) {
	var req CreateStackRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, err.Error(), http.StatusBadRequest)

		return
	}

	if err := validateCreateRequest(req); err != nil {
		handleCFNError(w, err)

		return
	}
	createStorage, ok := s.storage.(createExecutionStorage)
	if !ok {
		handleCFNError(w, errors.New("cloudformation storage does not support create execution reservations"))

		return
	}
	req.ClientRequestToken = executionToken(req.ClientRequestToken)
	fingerprint, err := createStackFingerprint(&req)
	if err != nil {
		handleCFNError(w, err)

		return
	}
	reserved, replayed, err := createStorage.ReserveCreateExecution(r.Context(), req.ClientRequestToken, fingerprint, req.StackName)
	if err != nil {
		handleCFNError(w, err)

		return
	}
	if replayed {
		if !reserved.Success {
			code := reserved.ErrorCode
			if code == "" {
				code = errInvalidParameter
			}
			writeCFNError(w, code, reserved.Error, http.StatusBadRequest)

			return
		}
		writeCreateStackResponse(w, reserved.StackID)

		return
	}

	stack, err := s.storage.CreateStack(r.Context(), &req)
	if err != nil {
		if releaseErr := createStorage.ReleaseCreateExecution(r.Context(), req.ClientRequestToken); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
		handleCFNError(w, err)

		return
	}
	complete := func(operationErr error) error {
		code := ""
		var cfnErr *Error
		if errors.As(operationErr, &cfnErr) {
			code = cfnErr.Code
		}

		return createStorage.CompleteCreateExecution(r.Context(), req.ClientRequestToken, stack.StackID, code, errorString(operationErr), operationErr == nil)
	}

	parsed, evaluationErr := template.Parse(req.TemplateBody)
	if evaluationErr != nil {
		evaluationErr = &Error{Code: errInvalidParameter, Message: evaluationErr.Error()}
		failureErr := markCreateFailure(r.Context(), s.storage, stack.StackID, evaluationErr, &req)
		completionErr := complete(errors.Join(evaluationErr, failureErr))
		handleCFNError(w, errors.Join(evaluationErr, failureErr, completionErr))

		return
	}
	parameters, evaluationErr := parsed.ResolveParameters(template.EvaluationInput{Parameters: requestParameterValues(req.parameters, req.Parameters, nil), PseudoParameters: pseudoParametersForStack(stack)})
	if evaluationErr != nil {
		failureErr := markCreateFailure(r.Context(), s.storage, stack.StackID, evaluationErr, &req)
		completionErr := complete(errors.Join(evaluationErr, failureErr))
		handleCFNError(w, errors.Join(evaluationErr, failureErr, completionErr))

		return
	}
	importNames, evaluationErr := parsed.ImportedExportNames(template.EvaluationInput{Parameters: parameters, PseudoParameters: pseudoParametersForStack(stack)})
	if evaluationErr != nil {
		failureErr := markCreateFailure(r.Context(), s.storage, stack.StackID, evaluationErr, &req)
		completionErr := complete(errors.Join(evaluationErr, failureErr))
		handleCFNError(w, errors.Join(evaluationErr, failureErr, completionErr))

		return
	}
	if err := s.executor().Create(r.Context(), LifecycleRequest{
		StackRef:   stack.StackID,
		Template:   parsed,
		Parameters: parameters,
		Imports:    stackImports(importNames),
		Options: ExecutionOptions{
			Capabilities:          req.Capabilities,
			ClientRequestToken:    req.ClientRequestToken,
			DisableRollback:       effectiveDisableRollback(req.DisableRollback, req.DeploymentConfig),
			OnFailure:             FailurePolicy(req.OnFailure),
			ResourceTypes:         append([]string(nil), req.ResourceTypes...),
			RoleARN:               req.RoleARN,
			DeploymentConfig:      cloneDeploymentConfig(req.DeploymentConfig),
			RollbackConfiguration: cloneRollbackConfiguration(req.RollbackConfiguration),
			Tags:                  cloneTags(req.Tags),
			RetainExceptOnCreate:  req.RetainExceptOnCreate,
		},
		Metadata: &LifecycleMetadata{
			TemplateBody:                req.TemplateBody,
			Parameters:                  parameters,
			Tags:                        cloneTags(req.Tags),
			RoleARN:                     req.RoleARN,
			ResourceTypes:               append([]string(nil), req.ResourceTypes...),
			NotificationARNs:            append([]string(nil), req.NotificationARNs...),
			RollbackConfiguration:       cloneRollbackConfiguration(req.RollbackConfiguration),
			DeploymentConfig:            cloneDeploymentConfig(req.DeploymentConfig),
			StackPolicyBody:             req.StackPolicyBody,
			EnableTerminationProtection: req.EnableTerminationProtection,
			RetainExceptOnCreate:        req.RetainExceptOnCreate,
			TimeoutInMinutes:            req.TimeoutInMinutes,
			DisableValidation:           req.DisableValidation,
			DisableRollback:             effectiveDisableRollback(req.DisableRollback, req.DeploymentConfig),
			OnFailure:                   effectiveOnFailure(req.OnFailure),
		},
	}); err != nil {
		completionErr := complete(err)
		handleCFNError(w, errors.Join(err, completionErr))

		return
	}
	if err := complete(nil); err != nil {
		handleCFNError(w, err)

		return
	}
	writeCreateStackResponse(w, stack.StackID)
}

// DeleteStack handles the DeleteStack action.
func (s *Service) DeleteStack(w http.ResponseWriter, r *http.Request) {
	var req DeleteStackRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}

	if req.StackName == "" {
		writeCFNError(w, errInvalidParameter, msgStackNameRequired, http.StatusBadRequest)

		return
	}

	if err := validateDeleteRequest(req); err != nil {
		handleCFNError(w, err)

		return
	}

	err := s.executor().Delete(r.Context(), LifecycleRequest{
		StackRef: req.StackName,
		Options: ExecutionOptions{
			ClientRequestToken: req.ClientRequestToken,
			DeletionMode:       req.DeletionMode,
			RetainResources:    append([]string(nil), req.RetainResources...),
			RoleARN:            req.RoleARN,
			DeploymentConfig:   cloneDeploymentConfig(req.DeploymentConfig),
		},
	})
	if err != nil {
		handleCFNError(w, err)

		return
	}

	writeCFNXMLResponse(w, XMLDeleteStackResponse{
		Xmlns:            cfnXMLNS,
		Result:           XMLDeleteStackResult{},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// DescribeStacks handles the DescribeStacks action.
//
//nolint:wsl // Conversion and deterministic ordering form one response boundary.
func (s *Service) DescribeStacks(w http.ResponseWriter, r *http.Request) {
	var req DescribeStacksRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}
	if req.NextToken != "" {
		writeCFNError(w, errInvalidParameter, "NextToken pagination is unsupported", http.StatusBadRequest)

		return
	}

	stacks, err := s.storage.DescribeStacks(r.Context(), req.StackName)
	if err != nil {
		handleCFNError(w, err)

		return
	}

	xmlStacks := make([]XMLStack, 0, len(stacks))

	for _, stack := range stacks {
		xmlStacks = append(xmlStacks, convertToXMLStack(stack))
	}
	sort.Slice(xmlStacks, func(left, right int) bool { return xmlStacks[left].StackID < xmlStacks[right].StackID })

	writeCFNXMLResponse(w, XMLDescribeStacksResponse{
		Xmlns: cfnXMLNS,
		Result: XMLDescribeStacksResult{
			Stacks: XMLStacks{Members: xmlStacks},
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// ListStacks handles the ListStacks action.
//
//nolint:wsl // Conversion and deterministic ordering form one response boundary.
func (s *Service) ListStacks(w http.ResponseWriter, r *http.Request) {
	var req ListStacksRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}
	if req.NextToken != "" {
		writeCFNError(w, errInvalidParameter, "NextToken pagination is unsupported", http.StatusBadRequest)

		return
	}

	stacks, err := s.storage.ListStacks(r.Context(), req.StackStatusFilter)
	if err != nil {
		handleCFNError(w, err)

		return
	}

	xmlSummaries := make([]XMLStackSummary, 0, len(stacks))

	for _, stack := range stacks {
		xmlSummaries = append(xmlSummaries, convertToXMLStackSummary(stack))
	}
	sort.Slice(xmlSummaries, func(left, right int) bool { return xmlSummaries[left].StackID < xmlSummaries[right].StackID })

	writeCFNXMLResponse(w, XMLListStacksResponse{
		Xmlns: cfnXMLNS,
		Result: XMLListStacksResult{
			StackSummaries: XMLStackSummaries{Members: xmlSummaries},
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// UpdateStack handles the UpdateStack action.
//
//nolint:funlen,wsl // Request evaluation and executor orchestration form one boundary.
func (s *Service) UpdateStack(w http.ResponseWriter, r *http.Request) {
	var req UpdateStackRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, err.Error(), http.StatusBadRequest)

		return
	}

	if err := validateUpdateRequest(req); err != nil {
		handleCFNError(w, err)

		return
	}

	reader, err := stackReader(s.storage)
	if err != nil {
		handleCFNError(w, err)

		return
	}
	stack, err := reader.GetStack(r.Context(), req.StackName)
	if err != nil {
		handleCFNError(w, err)

		return
	}
	templateBody := req.TemplateBody
	if req.UsePreviousTemplate {
		templateBody = stack.TemplateBody
	}
	parsed, evaluationErr := template.Parse(templateBody)
	if evaluationErr != nil {
		handleCFNError(w, evaluationErr)

		return
	}
	parameters, evaluationErr := parsed.ResolveParameters(template.EvaluationInput{Parameters: requestParameterValues(req.parameters, req.Parameters, previousParameters(stack)), PseudoParameters: pseudoParametersForStack(stack), Imports: committedImportValuesForStack(r.Context(), s.storage, stack.StackID)})
	if evaluationErr != nil {
		handleCFNError(w, &Error{Code: errInvalidParameter, Message: evaluationErr.Error()})

		return
	}
	importNames, evaluationErr := parsed.ImportedExportNames(template.EvaluationInput{Parameters: parameters, PseudoParameters: pseudoParametersForStack(stack), Imports: committedImportValuesForStack(r.Context(), s.storage, stack.StackID)})
	if evaluationErr != nil {
		handleCFNError(w, &Error{Code: errInvalidParameter, Message: evaluationErr.Error()})

		return
	}
	imports := committedImportValuesForNames(r.Context(), s.storage, importNames)
	evaluation, evaluationErr := parsed.Evaluate(template.EvaluationInput{
		Parameters:         parameters,
		PseudoParameters:   pseudoParametersForStack(stack),
		Imports:            imports,
		ResourceAttributes: resourceAttributesFromStack(stack),
	})
	if evaluationErr != nil {
		handleCFNError(w, &Error{Code: errInvalidParameter, Message: evaluationErr.Error()})

		return
	}
	if err := s.executor().Update(r.Context(), LifecycleRequest{
		StackRef:        stack.StackID,
		Template:        parsed,
		Evaluation:      evaluation,
		Parameters:      parameters,
		Imports:         stackImports(importNames),
		BaseRevision:    stack.Revision,
		BaseRevisionSet: true,
		Options: ExecutionOptions{
			Capabilities:          req.Capabilities,
			ClientRequestToken:    req.ClientRequestToken,
			DisableRollback:       effectiveDisableRollback(req.DisableRollback, req.DeploymentConfig),
			ResourceTypes:         append([]string(nil), req.ResourceTypes...),
			RoleARN:               req.RoleARN,
			DeploymentConfig:      cloneDeploymentConfig(req.DeploymentConfig),
			RollbackConfiguration: cloneRollbackConfiguration(req.RollbackConfiguration),
			Tags:                  cloneTags(req.Tags),
			RetainExceptOnCreate:  req.RetainExceptOnCreate,
		},
		Metadata: &LifecycleMetadata{
			TemplateBody:                templateBody,
			Parameters:                  parameters,
			Tags:                        cloneTags(req.Tags),
			RoleARN:                     req.RoleARN,
			ResourceTypes:               append([]string(nil), req.ResourceTypes...),
			NotificationARNs:            append([]string(nil), req.NotificationARNs...),
			RollbackConfiguration:       cloneRollbackConfiguration(req.RollbackConfiguration),
			DeploymentConfig:            cloneDeploymentConfig(req.DeploymentConfig),
			StackPolicyBody:             req.StackPolicyBody,
			StackPolicyDuringUpdateBody: req.StackPolicyDuringUpdateBody,
			RetainExceptOnCreate:        req.RetainExceptOnCreate,
			DisableValidation:           req.DisableValidation,
			DisableRollback:             effectiveDisableRollback(req.DisableRollback, req.DeploymentConfig),
		},
	}); err != nil {
		handleCFNError(w, err)

		return
	}
	writeCFNXMLResponse(w, XMLUpdateStackResponse{
		Xmlns: cfnXMLNS,
		Result: XMLUpdateStackResult{
			StackID: stack.StackID,
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// DescribeStackResources handles the DescribeStackResources action.
//
//nolint:wsl // Filtering, ordering, and conversion form one response boundary.
func (s *Service) DescribeStackResources(w http.ResponseWriter, r *http.Request) {
	var req DescribeStackResourcesRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}

	if req.StackName == "" {
		writeCFNError(w, errInvalidParameter, msgStackNameRequired, http.StatusBadRequest)

		return
	}

	resources, err := s.storage.DescribeStackResources(r.Context(), req.StackName, req.LogicalResourceID)
	if err != nil {
		handleCFNError(w, err)

		return
	}
	if req.PhysicalResourceID != "" {
		filtered := resources[:0]
		for _, resource := range resources {
			if resource.PhysicalResourceID == req.PhysicalResourceID {
				filtered = append(filtered, resource)
			}
		}
		resources = filtered
	}

	xmlResources := make([]XMLStackResource, 0, len(resources))
	sort.Slice(resources, func(left, right int) bool {
		return resources[left].LogicalResourceID < resources[right].LogicalResourceID
	})

	for _, resource := range resources {
		xmlResources = append(xmlResources, convertToXMLStackResource(resource))
	}

	writeCFNXMLResponse(w, XMLDescribeStackResourcesResponse{
		Xmlns: cfnXMLNS,
		Result: XMLDescribeStackResourcesResult{
			StackResources: XMLStackResources{Members: xmlResources},
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// DescribeStackEvents handles the DescribeStackEvents action.
//
//nolint:wsl // Reading, ordering, and conversion form one response boundary.
func (s *Service) DescribeStackEvents(w http.ResponseWriter, r *http.Request) {
	var req DescribeStackEventsRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}
	if req.StackName == "" {
		writeCFNError(w, errInvalidParameter, msgStackNameRequired, http.StatusBadRequest)

		return
	}
	if req.NextToken != "" {
		writeCFNError(w, errInvalidParameter, "NextToken pagination is unsupported", http.StatusBadRequest)

		return
	}
	reader, err := eventStorage(s.storage)
	if err != nil {
		handleCFNError(w, err)

		return
	}
	events, err := reader.ListStackEvents(r.Context(), req.StackName, "")
	if err != nil {
		handleCFNError(w, err)

		return
	}
	xmlEvents := make([]XMLStackEvent, 0, len(events))
	for index := range events {
		xmlEvents = append(xmlEvents, convertToXMLStackEvent(&events[index]))
	}
	sort.SliceStable(xmlEvents, func(left, right int) bool {
		if xmlEvents[left].Timestamp == xmlEvents[right].Timestamp {
			return xmlEvents[left].EventID > xmlEvents[right].EventID
		}

		return xmlEvents[left].Timestamp > xmlEvents[right].Timestamp
	})
	writeCFNXMLResponse(w, XMLDescribeStackEventsResponse{
		Xmlns:            cfnXMLNS,
		Result:           XMLDescribeStackEventsResult{StackEvents: XMLStackEvents{Members: xmlEvents}},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// DescribeStackResource handles the DescribeStackResource action.
//
//nolint:wsl // Validation and conversion form one response boundary.
func (s *Service) DescribeStackResource(w http.ResponseWriter, r *http.Request) {
	var req DescribeStackResourceRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}
	if req.StackName == "" || req.LogicalResourceID == "" {
		writeCFNError(w, errInvalidParameter, "StackName and LogicalResourceId are required", http.StatusBadRequest)

		return
	}
	resources, err := s.storage.DescribeStackResources(r.Context(), req.StackName, req.LogicalResourceID)
	if err != nil {
		handleCFNError(w, err)

		return
	}
	if len(resources) == 0 {
		writeCFNError(w, errInvalidParameter, "Resource not found", http.StatusBadRequest)

		return
	}
	writeCFNXMLResponse(w, XMLDescribeStackResourceResponse{
		Xmlns:            cfnXMLNS,
		Result:           XMLDescribeStackResourceResult{StackResourceDetail: convertToXMLStackResourceDetail(resources[0])},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// ListExports handles the ListExports action.
//
//nolint:wsl // Reading, ordering, and conversion form one response boundary.
func (s *Service) ListExports(w http.ResponseWriter, r *http.Request) {
	var req ListExportsRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}
	if req.NextToken != "" {
		writeCFNError(w, errInvalidParameter, "NextToken pagination is unsupported", http.StatusBadRequest)

		return
	}
	reader, err := exportStorage(s.storage)
	if err != nil {
		handleCFNError(w, err)

		return
	}
	exports, err := reader.ListExports(r.Context())
	if err != nil {
		handleCFNError(w, err)

		return
	}
	xmlExports := make([]XMLExport, 0, len(exports))
	for _, export := range exports {
		xmlExports = append(xmlExports, XMLExport{ExportingStackID: export.ExporterStackARN, Name: export.Name, Value: export.Value})
	}
	sort.Slice(xmlExports, func(left, right int) bool {
		if xmlExports[left].Name == xmlExports[right].Name {
			return xmlExports[left].ExportingStackID < xmlExports[right].ExportingStackID
		}

		return xmlExports[left].Name < xmlExports[right].Name
	})
	writeCFNXMLResponse(w, XMLListExportsResponse{
		Xmlns:            cfnXMLNS,
		Result:           XMLListExportsResult{Exports: XMLExports{Members: xmlExports}},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// CreateChangeSet handles the review-only CreateChangeSet action.
//
//nolint:cyclop,funlen,gocognit,wsl // Request normalization, evaluation, and review persistence form one boundary.
func (s *Service) CreateChangeSet(w http.ResponseWriter, r *http.Request) {
	var req CreateChangeSetRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, err.Error(), http.StatusBadRequest)

		return
	}
	if err := validateCreateChangeSetRequest(&req); err != nil {
		handleCFNError(w, err)

		return
	}
	changeStorage, ok := s.storage.(changeSetStorage)
	if !ok {
		handleCFNError(w, errors.New("cloudformation storage does not support change sets"))

		return
	}
	lifecycleReader, err := stackReader(s.storage)
	if err != nil {
		handleCFNError(w, err)

		return
	}
	req.ClientToken = executionToken(req.ClientToken)
	stack, err := lifecycleReader.GetStack(r.Context(), req.StackName)
	if err != nil {
		var cfnErr *Error
		if !errors.As(err, &cfnErr) || cfnErr.Code != errStackNotFound || req.ChangeSetType != changeSetTypeCreate {
			handleCFNError(w, err)

			return
		}
		stack, err = changeStorage.CreateReviewStack(r.Context(), req.StackName)
		if err != nil {
			handleCFNError(w, err)

			return
		}
	}
	var changeSet *ChangeSet
	err = changeStorage.RunExclusiveStackOperation(r.Context(), stack.StackID, func(lockedCtx context.Context) error {
		current, err := lifecycleReader.GetStack(lockedCtx, stack.StackID)
		if err != nil {
			return fmt.Errorf("get stack for change-set creation: %w", err)
		}
		changeSetType := effectiveChangeSetType(req.ChangeSetType)
		if current.StackStatus == StackStatusReviewInProgress && changeSetType != changeSetTypeCreate {
			return &Error{Code: errInvalidParameter, Message: "UPDATE change sets require an existing stack, not a review stack"}
		}
		if current.StackStatus != StackStatusReviewInProgress && changeSetType == changeSetTypeCreate {
			return &Error{Code: errInvalidParameter, Message: "CREATE change sets require a review stack"}
		}
		templateBody := req.TemplateBody
		if req.UsePreviousTemplate {
			templateBody = current.TemplateBody
		}
		parameters := requestParameterValues(req.parameters, req.Parameters, previousParameters(current))
		parsed, err := template.Parse(templateBody)
		if err != nil {
			return fmt.Errorf("evaluate change-set template: %w", err)
		}
		resolvedParameters, err := parsed.ResolveParameters(template.EvaluationInput{Parameters: parameters, PseudoParameters: pseudoParametersForStack(current), Imports: committedImportValuesForStack(lockedCtx, s.storage, current.StackID)})
		if err != nil {
			return fmt.Errorf("evaluate change-set parameters: %w", err)
		}
		importNames, err := parsed.ImportedExportNames(template.EvaluationInput{Parameters: resolvedParameters, PseudoParameters: pseudoParametersForStack(current), Imports: committedImportValuesForStack(lockedCtx, s.storage, current.StackID)})
		if err != nil {
			return fmt.Errorf("evaluate change-set imports: %w", err)
		}
		evaluation, err := parsed.Evaluate(template.EvaluationInput{Parameters: resolvedParameters, PseudoParameters: pseudoParametersForStack(current), Imports: committedImportValuesForNames(lockedCtx, s.storage, importNames), ResourceAttributes: resourceAttributesFromStack(current)})
		if err != nil {
			return fmt.Errorf("evaluate change-set template: %w", err)
		}
		req.TemplateBody = templateBody
		req.imports = stackImports(importNames)
		req.baseRevision = current.Revision
		req.baseRevisionSet = true
		req.preflightError = ""
		if err := s.executor().preflight(evaluation, req.Capabilities, req.ResourceTypes); err != nil {
			req.preflightError = err.Error()
		}
		changeSet, err = changeStorage.CreateChangeSet(lockedCtx, &req, evaluation)
		if err != nil {
			return fmt.Errorf("create change set: %w", err)
		}

		return nil
	})
	if err != nil {
		handleCFNError(w, err)

		return
	}
	writeCFNXMLResponse(w, XMLCreateChangeSetResponse{
		Xmlns:            cfnXMLNS,
		Result:           XMLCreateChangeSetResult{ID: changeSet.ID, StackID: changeSet.StackID},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// DescribeChangeSet returns an immutable change-set review.
//
//nolint:wsl // Review lookup and XML conversion form one response boundary.
func (s *Service) DescribeChangeSet(w http.ResponseWriter, r *http.Request) {
	var req DescribeChangeSetRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, err.Error(), http.StatusBadRequest)

		return
	}
	if req.ChangeSetName == "" {
		writeCFNError(w, errInvalidParameter, "ChangeSetName is required", http.StatusBadRequest)

		return
	}
	if req.NextToken != "" {
		writeCFNError(w, errInvalidParameter, "NextToken pagination is unsupported", http.StatusBadRequest)

		return
	}
	reader, ok := s.storage.(changeSetStorage)
	if !ok {
		handleCFNError(w, errors.New("cloudformation storage does not support change sets"))

		return
	}
	changeSet, err := reader.GetChangeSet(r.Context(), req.StackName, req.ChangeSetName)
	if err != nil {
		handleCFNError(w, err)

		return
	}
	writeCFNXMLResponse(w, XMLDescribeChangeSetResponse{
		Xmlns: cfnXMLNS,
		Result: XMLDescribeChangeSetResult{
			ChangeSetName:   changeSet.Name,
			ChangeSetID:     changeSet.ID,
			StackID:         changeSet.StackID,
			StackName:       changeSet.StackName,
			Description:     changeSet.Description,
			ChangeSetType:   changeSet.ChangeSetType,
			Status:          changeSet.Status,
			StatusReason:    changeSet.StatusReason,
			ExecutionStatus: changeSet.ExecutionStatus,
			CreationTime:    formatCFNTime(changeSet.CreationTime),
			Parameters:      xmlParameters(changeSet.Parameters),
			ResourceChanges: XMLResourceChanges{Members: convertToXMLResourceChanges(changeSet.ResourceChanges)},
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// ExecuteChangeSet applies a reviewed change set through the lifecycle executor.
//
//nolint:cyclop,funlen,gocognit,gocyclo,nestif,wsl // Stale checks, executor dispatch, and terminal review state form one boundary.
func (s *Service) ExecuteChangeSet(w http.ResponseWriter, r *http.Request) {
	var req ExecuteChangeSetRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, err.Error(), http.StatusBadRequest)

		return
	}
	if req.ChangeSetName == "" {
		writeCFNError(w, errInvalidParameter, "ChangeSetName is required", http.StatusBadRequest)

		return
	}
	reader, ok := s.storage.(changeSetStorage)
	if !ok {
		handleCFNError(w, errors.New("cloudformation storage does not support change sets"))

		return
	}
	changeSet, err := reader.GetChangeSet(r.Context(), req.StackName, req.ChangeSetName)
	if err != nil {
		handleCFNError(w, err)

		return
	}
	token := executionToken(req.ClientRequestToken)
	if changeSet.ExecutionStatus == ChangeSetExecutionComplete {
		writeCFNXMLResponse(w, XMLExecuteChangeSetResponse{Xmlns: cfnXMLNS, ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()}})

		return
	}
	if changeSet.Status != ChangeSetStatusCreateComplete || changeSet.ExecutionStatus != ChangeSetExecutionAvailable {
		if executionReader, ok := s.storage.(executionRecordReader); ok {
			record, found, readErr := executionReader.GetExecution(r.Context(), changeSet.StackID, token)
			if readErr == nil && found && record.Completed {
				if record.Success {
					writeCFNXMLResponse(w, XMLExecuteChangeSetResponse{Xmlns: cfnXMLNS, ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()}})

					return
				}
				if record.ErrorCode != "" {
					writeCFNError(w, record.ErrorCode, record.Error, http.StatusBadRequest)

					return
				}
			}
		}
		writeCFNError(w, errInvalidParameter, "Change set is not available for execution", http.StatusBadRequest)

		return
	}
	lifecycleReader, err := stackReader(s.storage)
	if err != nil {
		handleCFNError(w, err)

		return
	}
	operationCtx := context.WithValue(r.Context(), executionTokenContextKey{}, token)
	execute := func(lockedCtx context.Context) (bool, error) {
		currentChangeSet, err := reader.GetChangeSet(lockedCtx, changeSet.StackID, changeSet.ID)
		if err != nil {
			return false, fmt.Errorf("get change set for execution: %w", err)
		}
		if currentChangeSet.ExecutionStatus == ChangeSetExecutionComplete {
			return true, nil
		}
		if currentChangeSet.Status != ChangeSetStatusCreateComplete || currentChangeSet.ExecutionStatus != ChangeSetExecutionAvailable {
			return false, &Error{Code: errInvalidParameter, Message: "Change set is not available for execution"}
		}
		stack, err := lifecycleReader.GetStack(lockedCtx, changeSet.StackID)
		if err != nil {
			return false, fmt.Errorf("get stack for change-set execution: %w", err)
		}
		if stack.Revision != currentChangeSet.BaseRevision {
			return false, &Error{Code: "StaleChangeSetException", Message: "Change set is stale because the stack revision changed"}
		}
		options := cloneExecutionOptions(&currentChangeSet.Options)
		options.ClientRequestToken = token
		options.DisableRollback = true
		if req.disableRollbackSet {
			options.DisableRollback = req.DisableRollback
		}
		options.RetainExceptOnCreate = req.RetainExceptOnCreate
		metadata := cloneLifecycleMetadata(currentChangeSet.Metadata)
		if metadata == nil {
			metadata = &LifecycleMetadata{}
		}
		metadata.DisableRollback = options.DisableRollback
		compensation := &lifecycleCompensation{}
		lifecycle := LifecycleRequest{StackRef: currentChangeSet.StackID, Evaluation: cloneEvaluation(currentChangeSet.Evaluation), Imports: cloneImports(currentChangeSet.Imports), Options: options, Metadata: metadata, compensation: compensation}
		if currentChangeSet.ChangeSetType == changeSetTypeCreate {
			parsed, parseErr := template.Parse(currentChangeSet.TemplateBody)
			if parseErr != nil {
				return false, fmt.Errorf("parse change-set template for execution: %w", parseErr)
			}
			lifecycle.Template = parsed
			lifecycle.Parameters = cloneStringMap(currentChangeSet.Parameters)
			lifecycle.Evaluation = nil
			err = s.executor().Create(lockedCtx, lifecycle)
		} else {
			parsed, parseErr := template.Parse(currentChangeSet.TemplateBody)
			if parseErr != nil {
				return false, fmt.Errorf("parse change-set template for execution: %w", parseErr)
			}
			lifecycle.Template = parsed
			lifecycle.Parameters = cloneStringMap(currentChangeSet.Parameters)
			err = s.executor().Update(lockedCtx, lifecycle)
		}
		if err != nil {
			markErr := reader.MarkChangeSetExecution(lockedCtx, currentChangeSet.StackID, currentChangeSet.ID, false, err.Error())
			if markErr != nil {
				return false, errors.Join(err, markErr)
			}

			return true, err
		}
		if err := reader.MarkChangeSetExecution(lockedCtx, currentChangeSet.StackID, currentChangeSet.ID, true, ""); err != nil {
			compensationErr := s.executor().compensate(lockedCtx, compensation)
			if compensationErr != nil {
				return true, errors.Join(fmt.Errorf("mark change set execution: %w", err), compensationErr)
			}

			return false, fmt.Errorf("mark change set execution: %w", err)
		}

		return true, nil
	}
	var operationErr error
	if atomicStorage, ok := s.storage.(atomicChangeSetExecutionStorage); ok {
		operationErr = atomicStorage.RunChangeSetExecution(operationCtx, changeSet.StackID, execute)
	} else {
		operationErr = reader.RunExclusiveStackOperation(operationCtx, changeSet.StackID, func(lockedCtx context.Context) error {
			_, err := execute(lockedCtx)

			return err
		})
	}
	if operationErr != nil {
		handleCFNError(w, operationErr)

		return
	}
	writeCFNXMLResponse(w, XMLExecuteChangeSetResponse{Xmlns: cfnXMLNS, ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()}})
}

// GetTemplateSummary summarizes supported template structure and capabilities.
//
//nolint:funlen,wsl // Summary parsing and deterministic XML conversion form one response boundary.
func (s *Service) GetTemplateSummary(w http.ResponseWriter, r *http.Request) {
	var req GetTemplateSummaryRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, err.Error(), http.StatusBadRequest)

		return
	}
	if req.TemplateURL != "" || req.StackSetName != "" {
		writeCFNError(w, errInvalidParameter, "TemplateURL and StackSetName are unsupported", http.StatusBadRequest)

		return
	}
	body := req.TemplateBody
	if req.StackName != "" {
		if body != "" {
			writeCFNError(w, errInvalidParameter, "StackName and TemplateBody cannot both be specified", http.StatusBadRequest)

			return
		}
		var err error
		body, err = s.storage.GetTemplate(r.Context(), req.StackName)
		if err != nil {
			handleCFNError(w, err)

			return
		}
	}
	if body == "" {
		writeCFNError(w, errInvalidParameter, "TemplateBody or StackName is required", http.StatusBadRequest)

		return
	}
	result, err := summarizeTemplate(body)
	if err != nil {
		handleCFNError(w, err)

		return
	}
	params := make([]XMLTemplateParameter, 0, len(result.Parameters))
	for index := range result.Parameters {
		parameter := &result.Parameters[index]
		params = append(params, XMLTemplateParameter{
			ParameterKey:          parameter.ParameterKey,
			DefaultValue:          parameter.DefaultValue,
			NoEcho:                parameter.NoEcho,
			Description:           parameter.Description,
			ConstraintDescription: parameter.ConstraintDescription,
			ParameterType:         parameter.ParameterType,
			AllowedValues:         XMLStringMembers{Members: append([]string(nil), parameter.AllowedValues...)},
			AllowedPattern:        parameter.AllowedPattern,
			MinLength:             parameter.MinLength,
			MaxLength:             parameter.MaxLength,
			MinValue:              parameter.MinValue,
			MaxValue:              parameter.MaxValue,
		})
	}
	capabilities := append([]string(nil), result.Capabilities...)
	resourceTypes := append([]string(nil), result.ResourceTypes...)
	sort.Strings(capabilities)
	sort.Strings(resourceTypes)
	writeCFNXMLResponse(w, XMLGetTemplateSummaryResponse{
		Xmlns: cfnXMLNS,
		Result: XMLGetTemplateSummaryResult{
			Parameters:    XMLTemplateParameters{Members: params},
			Description:   result.Description,
			Capabilities:  XMLCapabilities{Members: capabilities},
			ResourceTypes: XMLStringMembers{Members: resourceTypes},
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// GetTemplate handles the GetTemplate action.
//
//nolint:wsl // Validation branches intentionally stay next to the request boundary.
func (s *Service) GetTemplate(w http.ResponseWriter, r *http.Request) {
	var req GetTemplateRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}

	if req.StackName == "" {
		writeCFNError(w, errInvalidParameter, msgStackNameRequired, http.StatusBadRequest)

		return
	}
	if req.ChangeSetName != "" || req.TemplateStage != "" {
		writeCFNError(w, errInvalidParameter, "ChangeSetName and TemplateStage are unsupported", http.StatusBadRequest)

		return
	}

	template, err := s.storage.GetTemplate(r.Context(), req.StackName)
	if err != nil {
		handleCFNError(w, err)

		return
	}

	writeCFNXMLResponse(w, XMLGetTemplateResponse{
		Xmlns: cfnXMLNS,
		Result: XMLGetTemplateResult{
			TemplateBody: template,
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// ValidateTemplate handles the ValidateTemplate action.
//
//nolint:wsl,funlen // Validation and deterministic conversion form one response boundary.
func (s *Service) ValidateTemplate(w http.ResponseWriter, r *http.Request) {
	var req ValidateTemplateRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}

	if req.TemplateURL != "" {
		writeCFNError(w, errInvalidParameter, msgTemplateURLUnsupported, http.StatusBadRequest)

		return
	}
	if req.TemplateBody == "" {
		writeCFNError(w, errInvalidParameter, "TemplateBody is required", http.StatusBadRequest)

		return
	}

	if _, err := template.Parse(req.TemplateBody); err != nil {
		handleCFNError(w, &Error{Code: errInvalidParameter, Message: err.Error()})

		return
	}
	result, err := summarizeTemplate(req.TemplateBody)
	if err != nil {
		handleCFNError(w, err)

		return
	}

	xmlParams := make([]XMLTemplateParameter, 0, len(result.Parameters))

	for index := range result.Parameters {
		param := &result.Parameters[index]
		xmlParams = append(xmlParams, XMLTemplateParameter{
			ParameterKey:          param.ParameterKey,
			DefaultValue:          param.DefaultValue,
			NoEcho:                param.NoEcho,
			Description:           param.Description,
			ConstraintDescription: param.ConstraintDescription,
			ParameterType:         param.ParameterType,
			AllowedValues:         XMLStringMembers{Members: append([]string(nil), param.AllowedValues...)},
			AllowedPattern:        param.AllowedPattern,
			MinLength:             param.MinLength,
			MaxLength:             param.MaxLength,
			MinValue:              param.MinValue,
			MaxValue:              param.MaxValue,
		})
	}
	sort.Slice(xmlParams, func(left, right int) bool { return xmlParams[left].ParameterKey < xmlParams[right].ParameterKey })
	sort.Strings(result.Capabilities)

	writeCFNXMLResponse(w, XMLValidateTemplateResponse{
		Xmlns: cfnXMLNS,
		Result: XMLValidateTemplateResult{
			Parameters:   XMLTemplateParameters{Members: xmlParams},
			Description:  result.Description,
			Capabilities: XMLCapabilities{Members: result.Capabilities},
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// DispatchAction routes the request to the appropriate handler based on Action parameter.
func (s *Service) DispatchAction(w http.ResponseWriter, r *http.Request) {
	action := extractAction(r)
	handler := s.getActionHandler(action)

	if handler == nil {
		writeCFNError(w, errInvalidAction, fmt.Sprintf("The action '%s' is not valid", action), http.StatusBadRequest)

		return
	}

	handler(w, r)
}

// getActionHandler returns the handler function for the given action.
func (s *Service) getActionHandler(action string) func(http.ResponseWriter, *http.Request) {
	handlers := map[string]func(http.ResponseWriter, *http.Request){
		"CreateChangeSet":        s.CreateChangeSet,
		"CreateStack":            s.CreateStack,
		"DeleteStack":            s.DeleteStack,
		"DescribeChangeSet":      s.DescribeChangeSet,
		"DescribeStackEvents":    s.DescribeStackEvents,
		"DescribeStackResource":  s.DescribeStackResource,
		"DescribeStacks":         s.DescribeStacks,
		"ListStacks":             s.ListStacks,
		"ListExports":            s.ListExports,
		"UpdateStack":            s.UpdateStack,
		"DescribeStackResources": s.DescribeStackResources,
		"GetTemplate":            s.GetTemplate,
		"GetTemplateSummary":     s.GetTemplateSummary,
		"ExecuteChangeSet":       s.ExecuteChangeSet,
		"ValidateTemplate":       s.ValidateTemplate,
	}

	return handlers[action]
}

// Helper functions.

// convertToXMLStack converts a Stack to XMLStack.
//
//nolint:wsl,funlen // Stack conversion and deterministic parameter ordering form one boundary.
func convertToXMLStack(stack *Stack) XMLStack {
	params := make([]XMLParameter, 0, len(stack.Parameters))

	for key, value := range stack.Parameters {
		params = append(params, XMLParameter{
			ParameterKey:   key,
			ParameterValue: value,
		})
	}
	sort.Slice(params, func(left, right int) bool { return params[left].ParameterKey < params[right].ParameterKey })

	xmlStack := XMLStack{
		StackID:                     stack.StackID,
		StackName:                   stack.StackName,
		StackStatus:                 stack.StackStatus,
		StackStatusReason:           stack.StackStatusReason,
		CreationTime:                formatCFNTime(stack.CreationTime),
		Parameters:                  XMLParameters{Members: params},
		RoleARN:                     stack.RoleARN,
		DisableRollback:             stack.DisableRollback,
		TimeoutInMinutes:            stack.TimeoutInMinutes,
		EnableTerminationProtection: stack.EnableTerminationProtection,
		RetainExceptOnCreate:        stack.RetainExceptOnCreate,
		Tags: XMLTags{Members: func() []XMLTag {
			result := make([]XMLTag, 0, len(stack.Tags))
			for _, tag := range stack.Tags {
				result = append(result, XMLTag(tag))
			}
			sort.Slice(result, func(left, right int) bool { return result[left].Key < result[right].Key })

			return result
		}()},
		NotificationARNs: XMLNotificationARNs{Members: append([]string(nil), stack.NotificationARNs...)},
	}
	if stack.EvaluatedState != nil {
		outputs := make([]XMLOutput, 0, len(stack.EvaluatedState.Outputs))
		for name, output := range stack.EvaluatedState.Outputs {
			outputs = append(outputs, XMLOutput{OutputKey: name, OutputValue: fmt.Sprint(output.Value), ExportName: output.ExportName})
		}
		sort.Slice(outputs, func(left, right int) bool { return outputs[left].OutputKey < outputs[right].OutputKey })
		xmlStack.Outputs = XMLOutputs{Members: outputs}
	}
	if stack.RollbackConfiguration != nil {
		triggers := make([]XMLRollbackTrigger, 0, len(stack.RollbackConfiguration.RollbackTriggers))
		for _, trigger := range stack.RollbackConfiguration.RollbackTriggers {
			triggers = append(triggers, XMLRollbackTrigger(trigger))
		}
		xmlStack.RollbackConfiguration = &XMLRollbackConfiguration{
			RollbackTriggers:        XMLRollbackTriggers{Members: triggers},
			MonitoringTimeInMinutes: stack.RollbackConfiguration.MonitoringTimeInMinutes,
		}
	}
	if stack.DeploymentConfig != nil {
		xmlStack.DeploymentConfig = &XMLDeploymentConfig{Mode: stack.DeploymentConfig.Mode, DisableRollback: stack.DeploymentConfig.DisableRollback}
	}

	if !stack.LastUpdatedTime.IsZero() {
		xmlStack.LastUpdatedTime = formatCFNTime(stack.LastUpdatedTime)
	}

	if !stack.DeletionTime.IsZero() {
		xmlStack.DeletionTime = formatCFNTime(stack.DeletionTime)
	}

	return xmlStack
}

// convertToXMLStackSummary converts a Stack to XMLStackSummary.
func convertToXMLStackSummary(stack *Stack) XMLStackSummary {
	summary := XMLStackSummary{
		StackID:           stack.StackID,
		StackName:         stack.StackName,
		StackStatus:       stack.StackStatus,
		StackStatusReason: stack.StackStatusReason,
		CreationTime:      formatCFNTime(stack.CreationTime),
	}

	if !stack.LastUpdatedTime.IsZero() {
		summary.LastUpdatedTime = formatCFNTime(stack.LastUpdatedTime)
	}

	if !stack.DeletionTime.IsZero() {
		summary.DeletionTime = formatCFNTime(stack.DeletionTime)
	}

	return summary
}

// convertToXMLStackResource converts a StackResource to XMLStackResource.
func convertToXMLStackResource(resource *StackResource) XMLStackResource {
	return XMLStackResource{
		LogicalResourceID:  resource.LogicalResourceID,
		PhysicalResourceID: resource.PhysicalResourceID,
		ResourceType:       resource.ResourceType,
		ResourceStatus:     resource.ResourceStatus,
		Timestamp:          formatCFNTime(resource.Timestamp),
		StackID:            resource.StackID,
		StackName:          resource.StackName,
	}
}

func convertToXMLStackEvent(event *StackEvent) XMLStackEvent {
	return XMLStackEvent{
		EventID:              event.EventID,
		StackID:              event.StackID,
		StackName:            event.StackName,
		LogicalResourceID:    event.LogicalResourceID,
		PhysicalResourceID:   event.PhysicalResourceID,
		ResourceType:         event.ResourceType,
		Timestamp:            formatCFNTime(event.Timestamp),
		ResourceStatus:       event.ResourceStatus,
		ResourceStatusReason: event.ResourceStatusReason,
		ClientRequestToken:   event.ClientRequestToken,
	}
}

func convertToXMLStackResourceDetail(resource *StackResource) XMLStackResourceDetail {
	return XMLStackResourceDetail{
		LogicalResourceID:    resource.LogicalResourceID,
		PhysicalResourceID:   resource.PhysicalResourceID,
		ResourceType:         resource.ResourceType,
		ResourceStatus:       resource.ResourceStatus,
		LastUpdatedTimestamp: formatCFNTime(resource.Timestamp),
		StackID:              resource.StackID,
		StackName:            resource.StackName,
	}
}

func formatCFNTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func pseudoParametersForStack(stack *Stack) map[string]string {
	return map[string]string{
		pseudoAccountIDName: pseudoAccountID,
		pseudoRegionName:    pseudoRegion,
		pseudoStackIDName:   stack.StackID,
		pseudoStackNameName: stack.StackName,
		pseudoURLSuffixName: pseudoURLSuffix,
	}
}

//nolint:wsl,nlreturn // Import lookup is a linear storage compatibility path.
func committedImportValuesForStack(ctx context.Context, storage Storage, stackRef string) map[string]string {
	reader, ok := storage.(interface {
		GetImports(context.Context, string) ([]StackImport, error)
		ListExports(context.Context) ([]StackExport, error)
	})
	if !ok {
		return nil
	}
	imports, err := reader.GetImports(ctx, stackRef)
	if err != nil {
		return nil
	}
	exports, err := reader.ListExports(ctx)
	if err != nil {
		return nil
	}
	values := make(map[string]string, len(imports))
	for _, edge := range imports {
		for _, export := range exports {
			if edge.ExportName == export.Name {
				values[edge.ExportName] = export.Value
				break
			}
		}
	}

	return values
}

//nolint:wsl // Export lookup and desired-name filtering form one read boundary.
func committedImportValuesForNames(ctx context.Context, storage Storage, names []string) map[string]string {
	reader, ok := storage.(interface {
		ListExports(context.Context) ([]StackExport, error)
	})
	if !ok {
		return nil
	}
	exports, err := reader.ListExports(ctx)
	if err != nil {
		return nil
	}
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}
	values := make(map[string]string, len(wanted))
	for _, export := range exports {
		if _, ok := wanted[export.Name]; ok {
			values[export.Name] = export.Value
		}
	}

	return values
}

func stackImports(names []string) []StackImport {
	imports := make([]StackImport, 0, len(names))
	for _, name := range names {
		imports = append(imports, StackImport{ExportName: name})
	}

	return imports
}

//nolint:wsl // Parameter precedence is intentionally applied in one pass.
func requestParameterValues(parameters []Parameter, explicit, previous map[string]string) map[string]string {
	values := cloneStringMap(previous)
	if values == nil && parameters == nil && explicit == nil {
		return nil
	}
	if values == nil {
		values = make(map[string]string, len(parameters)+len(explicit))
	}
	for key, value := range explicit {
		values[key] = value
	}
	for _, parameter := range parameters {
		if parameter.ParameterKey == "" || parameter.UsePreviousValue {
			continue
		}
		values[parameter.ParameterKey] = parameter.ParameterValue
	}

	return values
}

func previousParameters(stack *Stack) map[string]string {
	if stack.EvaluatedState != nil {
		return stack.EvaluatedState.Parameters
	}

	return stack.Parameters
}

type stackLifecycleReader interface {
	GetStack(context.Context, string) (*Stack, error)
}

type createFailureWriter interface {
	MarkCreateFailed(context.Context, string, string, string, bool, string) error
}

type stackEventReader interface {
	ListStackEvents(context.Context, string, string) ([]StackEvent, error)
}

type stackExportReader interface {
	ListExports(context.Context) ([]StackExport, error)
}

func stackReader(storage Storage) (stackLifecycleReader, error) {
	reader, ok := storage.(stackLifecycleReader)
	if !ok {
		return nil, errors.New("cloudformation storage does not support stack reads")
	}

	return reader, nil
}

func eventStorage(storage Storage) (stackEventReader, error) {
	reader, ok := storage.(stackEventReader)
	if !ok {
		return nil, errors.New("cloudformation storage does not support stack event reads")
	}

	return reader, nil
}

func exportStorage(storage Storage) (stackExportReader, error) {
	reader, ok := storage.(stackExportReader)
	if !ok {
		return nil, errors.New("cloudformation storage does not support export reads")
	}

	return reader, nil
}

//nolint:wsl // Fingerprinting intentionally serializes the complete request value.
func createStackFingerprint(req *CreateStackRequest) (string, error) {
	request := *req
	request.ClientRequestToken = ""
	request.parameters = nil
	data, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("fingerprint CreateStack request: %w", err)
	}
	hash := sha256.Sum256(data)

	return hex.EncodeToString(hash[:]), nil
}

func writeCreateStackResponse(w http.ResponseWriter, stackID string) {
	writeCFNXMLResponse(w, XMLCreateStackResponse{
		Xmlns: cfnXMLNS,
		Result: XMLCreateStackResult{
			StackID: stackID,
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

//nolint:cyclop,gocritic,wsl // Request validation deliberately rejects every unsupported control.
func validateCreateRequest(req CreateStackRequest) error {
	if req.StackName == "" {
		return &Error{Code: errInvalidParameter, Message: msgStackNameRequired}
	}
	if req.TemplateURL != "" {
		return &Error{Code: errInvalidParameter, Message: msgTemplateURLUnsupported}
	}
	if req.TemplateBody == "" {
		return &Error{Code: errInvalidParameter, Message: "TemplateBody is required"}
	}
	if req.RollbackConfiguration != nil {
		return &Error{Code: errInvalidParameter, Message: "RollbackConfiguration is unsupported"}
	}
	if req.StackPolicyBody != "" || req.StackPolicyURL != "" {
		return &Error{Code: errInvalidParameter, Message: "StackPolicyBody and StackPolicyURL are unsupported"}
	}
	if req.DeploymentConfig != nil && req.DeploymentConfig.Mode != "" {
		return &Error{Code: errInvalidParameter, Message: msgDeploymentModeUnsupported}
	}
	if (req.DisableRollback || req.DeploymentConfig != nil && req.DeploymentConfig.DisableRollback) && req.OnFailure != "" {
		return &Error{Code: errInvalidParameter, Message: "DisableRollback and OnFailure cannot both be specified"}
	}
	if len(req.Capabilities) > 0 && len(req.ResourceTypes) > 0 {
		return &Error{Code: errInvalidParameter, Message: msgCapabilitiesResourceTypesConflict}
	}
	if req.DisableValidation {
		return &Error{Code: errInvalidParameter, Message: "DisableValidation is unsupported"}
	}
	if req.TimeoutInMinutes > 0 {
		return &Error{Code: errInvalidParameter, Message: "TimeoutInMinutes is unsupported"}
	}
	for _, parameter := range req.parameters {
		if parameter.UsePreviousValue {
			return &Error{Code: errInvalidParameter, Message: "UsePreviousValue is not valid for CreateStack"}
		}
	}
	if err := validateExecutionOptions(ExecutionOptions{OnFailure: FailurePolicy(req.OnFailure), DisableRollback: effectiveDisableRollback(req.DisableRollback, req.DeploymentConfig)}); err != nil {
		return &Error{Code: errInvalidParameter, Message: err.Error()}
	}

	return nil
}

//nolint:cyclop,gocritic,wsl // Request validation deliberately rejects every unsupported control.
func validateUpdateRequest(req UpdateStackRequest) error {
	if req.StackName == "" {
		return &Error{Code: errInvalidParameter, Message: msgStackNameRequired}
	}
	if req.TemplateURL != "" {
		return &Error{Code: errInvalidParameter, Message: msgTemplateURLUnsupported}
	}
	if req.UsePreviousTemplate && req.TemplateBody != "" {
		return &Error{Code: errInvalidParameter, Message: "UsePreviousTemplate cannot be combined with TemplateBody"}
	}
	if !req.UsePreviousTemplate && req.TemplateBody == "" {
		return &Error{Code: errInvalidParameter, Message: "TemplateBody is required unless UsePreviousTemplate is true"}
	}
	if req.StackPolicyURL != "" || req.StackPolicyDuringUpdateURL != "" {
		return &Error{Code: errInvalidParameter, Message: "stack policy URLs are unsupported"}
	}
	if req.StackPolicyBody != "" || req.StackPolicyDuringUpdateBody != "" {
		return &Error{Code: errInvalidParameter, Message: "stack policy bodies are unsupported"}
	}
	if req.RollbackConfiguration != nil {
		return &Error{Code: errInvalidParameter, Message: "RollbackConfiguration is unsupported"}
	}
	if req.DeploymentConfig != nil && req.DeploymentConfig.Mode != "" {
		return &Error{Code: errInvalidParameter, Message: msgDeploymentModeUnsupported}
	}
	if req.DisableValidation {
		return &Error{Code: errInvalidParameter, Message: "DisableValidation is unsupported"}
	}
	if len(req.Capabilities) > 0 && len(req.ResourceTypes) > 0 {
		return &Error{Code: errInvalidParameter, Message: msgCapabilitiesResourceTypesConflict}
	}
	if err := validateExecutionOptions(ExecutionOptions{DisableRollback: effectiveDisableRollback(req.DisableRollback, req.DeploymentConfig)}); err != nil {
		return &Error{Code: errInvalidParameter, Message: err.Error()}
	}

	return nil
}

//nolint:cyclop,wsl // Change-set validation is the explicit unsupported-input boundary.
func validateCreateChangeSetRequest(req *CreateChangeSetRequest) error {
	if req.StackName == "" {
		return &Error{Code: errInvalidParameter, Message: msgStackNameRequired}
	}
	if req.ChangeSetName == "" {
		return &Error{Code: errInvalidParameter, Message: "ChangeSetName is required"}
	}
	if req.TemplateURL != "" {
		return &Error{Code: errInvalidParameter, Message: msgTemplateURLUnsupported}
	}
	switch req.ChangeSetType {
	case "", changeSetTypeCreate, "UPDATE":
	default:
		return &Error{Code: errInvalidParameter, Message: fmt.Sprintf("unsupported ChangeSetType %q", req.ChangeSetType)}
	}
	if req.ChangeSetType == changeSetTypeCreate && req.UsePreviousTemplate {
		return &Error{Code: errInvalidParameter, Message: "UsePreviousTemplate is unsupported for CREATE change sets"}
	}
	if !req.UsePreviousTemplate && req.TemplateBody == "" {
		return &Error{Code: errInvalidParameter, Message: "TemplateBody is required unless UsePreviousTemplate is true"}
	}
	if req.IncludeNestedStacks {
		return &Error{Code: errInvalidParameter, Message: "IncludeNestedStacks is unsupported"}
	}
	if len(req.Capabilities) > 0 && len(req.ResourceTypes) > 0 {
		return &Error{Code: errInvalidParameter, Message: msgCapabilitiesResourceTypesConflict}
	}
	if req.ChangeSetType == changeSetTypeCreate {
		for _, parameter := range req.parameters {
			if parameter.UsePreviousValue {
				return &Error{Code: errInvalidParameter, Message: "UsePreviousValue is not valid for CREATE change sets"}
			}
		}
	}

	return nil
}

//nolint:gocritic,wsl // Request validation is the complete deletion-mode boundary.
func validateDeleteRequest(req DeleteStackRequest) error {
	switch req.DeletionMode {
	case "", deletionModeStandard, deletionModeForce:
	default:
		return &Error{Code: errInvalidParameter, Message: fmt.Sprintf("unknown DeletionMode %q", req.DeletionMode)}
	}
	if req.DeploymentConfig != nil && req.DeploymentConfig.Mode != "" {
		return &Error{Code: errInvalidParameter, Message: msgDeploymentModeUnsupported}
	}

	return nil
}

func effectiveDisableRollback(disable bool, config *DeploymentConfig) bool {
	return disable || config != nil && config.DisableRollback
}

//nolint:wsl // Parameter ordering is the final XML conversion step.
func xmlParameters(parameters map[string]string) XMLParameters {
	result := make([]XMLParameter, 0, len(parameters))
	for key, value := range parameters {
		result = append(result, XMLParameter{ParameterKey: key, ParameterValue: value})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ParameterKey < result[right].ParameterKey })

	return XMLParameters{Members: result}
}

//nolint:wsl // Resource change conversion preserves the deterministic review order.
func convertToXMLResourceChanges(changes []ResourceChange) []XMLResourceChangeMember {
	result := make([]XMLResourceChangeMember, 0, len(changes))
	for _, change := range changes {
		result = append(result, XMLResourceChangeMember{ResourceChange: XMLResourceChange(change)})
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].ResourceChange.LogicalResourceID < result[right].ResourceChange.LogicalResourceID
	})

	return result
}

//nolint:cyclop,funlen,gocognit,nestif,wsl // Summary extraction traverses one validated template tree.
func summarizeTemplate(body string) (*TemplateSummaryResult, error) {
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(body), &raw); err != nil {
		return nil, &Error{Code: errInvalidParameter, Message: fmt.Sprintf("parse template summary: %v", err)}
	}
	_, hasTransform := raw["Transform"]
	validationBody := body
	if hasTransform {
		delete(raw, "Transform")
		validated, err := yaml.Marshal(raw)
		if err != nil {
			return nil, &Error{Code: errInvalidParameter, Message: fmt.Sprintf("parse template summary: %v", err)}
		}
		validationBody = string(validated)
	}
	if _, err := template.Parse(validationBody); err != nil {
		return nil, &Error{Code: errInvalidParameter, Message: err.Error()}
	}
	result := &TemplateSummaryResult{}
	if description, ok := raw["Description"].(string); ok {
		result.Description = description
	}
	if parameters, ok := raw["Parameters"].(map[string]any); ok {
		for key, value := range parameters {
			parameterMap, ok := value.(map[string]any)
			if !ok {
				continue
			}
			parameter := TemplateParameter{ParameterKey: key}
			if typeName, ok := parameterMap["Type"].(string); ok {
				parameter.ParameterType = typeName
			}
			if defaultValue, ok := parameterMap["Default"]; ok {
				parameter.DefaultValue = fmt.Sprint(defaultValue)
			}
			if noEcho, ok := parameterMap["NoEcho"].(bool); ok {
				parameter.NoEcho = noEcho
			}
			if description, ok := parameterMap["Description"].(string); ok {
				parameter.Description = description
			}
			if constraintDescription, ok := parameterMap["ConstraintDescription"].(string); ok {
				parameter.ConstraintDescription = constraintDescription
			}
			if allowedValues, ok := parameterMap["AllowedValues"].([]any); ok {
				for _, value := range allowedValues {
					parameter.AllowedValues = append(parameter.AllowedValues, fmt.Sprint(value))
				}
			}
			if allowedPattern, ok := parameterMap["AllowedPattern"].(string); ok {
				parameter.AllowedPattern = allowedPattern
			}
			parameter.MinLength = summaryIntPointer(parameterMap["MinLength"])
			parameter.MaxLength = summaryIntPointer(parameterMap["MaxLength"])
			parameter.MinValue = summaryFloatPointer(parameterMap["MinValue"])
			parameter.MaxValue = summaryFloatPointer(parameterMap["MaxValue"])
			result.Parameters = append(result.Parameters, parameter)
		}
	}
	resourceTypes := make(map[string]struct{})
	capabilities := make(map[string]struct{})
	if resources, ok := raw["Resources"].(map[string]any); ok {
		for _, value := range resources {
			resourceMap, ok := value.(map[string]any)
			if !ok {
				continue
			}
			typeName, _ := resourceMap["Type"].(string)
			if typeName == "" {
				continue
			}
			resourceTypes[typeName] = struct{}{}
			if !strings.HasPrefix(typeName, "AWS::IAM::") {
				continue
			}
			properties, _ := resourceMap["Properties"].(map[string]any)
			named := false
			for property, propertyValue := range properties {
				if strings.HasSuffix(property, "Name") && fmt.Sprint(propertyValue) != "" {
					named = true
				}
			}
			if named {
				capabilities["CAPABILITY_NAMED_IAM"] = struct{}{}
			} else {
				capabilities["CAPABILITY_IAM"] = struct{}{}
			}
		}
	}
	if hasTransform {
		capabilities["CAPABILITY_AUTO_EXPAND"] = struct{}{}
	}
	for capability := range capabilities {
		result.Capabilities = append(result.Capabilities, capability)
	}
	for resourceType := range resourceTypes {
		result.ResourceTypes = append(result.ResourceTypes, resourceType)
	}
	sort.Slice(result.Parameters, func(left, right int) bool {
		return result.Parameters[left].ParameterKey < result.Parameters[right].ParameterKey
	})
	sort.Strings(result.Capabilities)
	sort.Strings(result.ResourceTypes)

	return result, nil
}

//nolint:wsl // Parsing and pointer construction form one conversion boundary.
func summaryIntPointer(value any) *int {
	if value == nil {
		return nil
	}
	parsed, err := strconv.Atoi(fmt.Sprint(value))
	if err != nil {
		return nil
	}

	return &parsed
}

//nolint:wsl // Parsing and pointer construction form one conversion boundary.
func summaryFloatPointer(value any) *float64 {
	if value == nil {
		return nil
	}
	parsed, err := strconv.ParseFloat(fmt.Sprint(value), 64)
	if err != nil {
		return nil
	}

	return &parsed
}

func effectiveOnFailure(value string) FailurePolicy {
	if value == "" {
		return FailurePolicyRollback
	}

	return FailurePolicy(value)
}

func markCreateFailure(ctx context.Context, storage Storage, stackRef string, cause error, req *CreateStackRequest) error {
	writer, ok := storage.(createFailureWriter)
	if !ok {
		return errors.New("cloudformation storage does not support create failure persistence")
	}

	if err := writer.MarkCreateFailed(ctx, stackRef, cause.Error(), errorCodeOf(cause), effectiveOnFailure(req.OnFailure) == FailurePolicyDelete, executionToken(req.ClientRequestToken)); err != nil {
		return fmt.Errorf("persist create failure: %w", err)
	}

	return nil
}

// readCFNJSONRequest reads and decodes JSON request body.
func readCFNJSONRequest(r *http.Request, v any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("failed to read request body: %w", err)
	}

	if len(body) == 0 {
		return nil
	}

	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	return nil
}

// extractAction extracts the action name from the request.
func extractAction(r *http.Request) string {
	// Try X-Amz-Target header (format: "CloudFormation.ActionName").
	target := r.Header.Get("X-Amz-Target")
	if target != "" {
		if idx := strings.LastIndex(target, "."); idx >= 0 {
			return target[idx+1:]
		}
	}

	// Fallback to URL query parameter.
	return r.URL.Query().Get("Action")
}

// writeCFNXMLResponse writes an XML response with HTTP 200 OK.
func writeCFNXMLResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("x-amzn-RequestId", uuid.New().String())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}

// writeCFNError writes a CloudFormation error response in XML format.
func writeCFNError(w http.ResponseWriter, code, message string, status int) {
	requestID := uuid.New().String()

	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("x-amzn-RequestId", requestID)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(XMLErrorResponse{
		Xmlns: cfnXMLNS,
		Error: XMLError{
			Type:    "Sender",
			Code:    code,
			Message: message,
		},
		RequestID: requestID,
	})
}

// handleCFNError handles CloudFormation errors and writes the appropriate response.
func handleCFNError(w http.ResponseWriter, err error) {
	var cfnErr *Error
	if errors.As(err, &cfnErr) {
		writeCFNError(w, cfnErr.Code, cfnErr.Message, http.StatusBadRequest)

		return
	}

	writeCFNError(w, errInternalError, "Internal server error", http.StatusInternalServerError)
}
