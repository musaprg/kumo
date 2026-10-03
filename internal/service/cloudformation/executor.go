package cloudformation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sivchari/kumo/internal/service/cloudformation/resource"
	"github.com/sivchari/kumo/internal/service/cloudformation/template"
)

const (
	journalBefore       = "BEFORE"
	journalAfter        = "AFTER"
	journalFailed       = "FAILED"
	operationCreate     = "CREATE"
	operationDelete     = "DELETE"
	operationUpdate     = "UPDATE"
	retainPolicy        = "Retain"
	snapshotPolicy      = "Snapshot"
	pseudoAccountID     = "123456789012"
	pseudoRegion        = "us-east-1"
	pseudoURLSuffix     = "amazonaws.com"
	pseudoAccountIDName = "AWS::AccountId"
	pseudoRegionName    = "AWS::Region"
	pseudoStackIDName   = "AWS::StackId"
	pseudoStackNameName = "AWS::StackName"
	pseudoURLSuffixName = "AWS::URLSuffix"
)

// FailurePolicy controls what happens to successfully created resources after a
// later create operation fails.
type FailurePolicy string

const (
	// FailurePolicyRollback removes resources created by the failed operation.
	FailurePolicyRollback FailurePolicy = "ROLLBACK"
	// FailurePolicyRetain leaves successfully created resources in place.
	FailurePolicyRetain FailurePolicy = "RETAIN"
	// FailurePolicyDoNothing is the CloudFormation-compatible name for retain.
	FailurePolicyDoNothing FailurePolicy = "DO_NOTHING"
	// FailurePolicyDelete removes the stack after a failed create.
	FailurePolicyDelete FailurePolicy = "DELETE"
)

// ExecutionOptions configures one synchronous lifecycle operation.
type ExecutionOptions struct {
	ClientRequestToken       string
	Capabilities             []string
	OnFailure                FailurePolicy
	DisableRollback          bool
	RetainResourcesOnFail    bool
	RetainResourcesOnFailure bool
	RetainResources          []string
	DeletionMode             string
	RoleARN                  string
	ResourceTypes            []string
	DeploymentConfig         *DeploymentConfig
	RollbackConfiguration    *RollbackConfiguration
	Tags                     []Tag
	RetainExceptOnCreate     bool
}

// LifecycleRequest supplies an evaluated stack state to the executor.
type LifecycleRequest struct {
	StackRef        string
	Evaluation      *template.Evaluation
	Template        *template.Template
	Parameters      map[string]string
	Imports         []StackImport
	Options         ExecutionOptions
	Metadata        *LifecycleMetadata
	BaseRevision    uint64
	BaseRevisionSet bool
	compensation    *lifecycleCompensation
}

type lifecycleCompensation struct {
	actions []compensationAction
}

type compensationAction struct {
	kind               string
	logicalID          string
	adapter            resource.Adapter
	updater            resource.UpdateCapable
	physicalID         string
	definition         template.Resource
	previousDefinition template.Resource
	capabilities       []string
	tags               map[string]string
}

const (
	compensateCreate = "CREATE"
	compensateUpdate = "UPDATE"
	compensateDelete = "DELETE"
)

// Executor synchronously applies evaluated CloudFormation resource state.
type Executor struct {
	storage  Storage
	registry *resource.Registry
}

type lifecycleStorage interface {
	Storage
	RunExclusiveStackOperation(context.Context, string, func(context.Context) error) error
	GetStack(context.Context, string) (*Stack, error)
	RunStackOperation(context.Context, string, func(context.Context, *Stack) error) error
	BeginExecution(context.Context, string, string, string, string) (ExecutionRecord, bool, error)
	CompleteExecution(context.Context, string, string, bool, string, string) error
	MarkCreateFailed(context.Context, string, string, string, bool, string) error
	AppendStackEvent(context.Context, string, StackEvent) error
	AppendStackJournal(context.Context, string, string, string, string) error
	StageExports(context.Context, string, []StackExport) (*ExportStage, error)
	CommitExports(context.Context, *ExportStage) error
	StageImports(context.Context, string, []StackImport) (*ImportStage, error)
	CommitImports(context.Context, *ImportStage) error
	GetImports(context.Context, string) ([]StackImport, error)
	ListExports(context.Context) ([]StackExport, error)
	FinalizeDeletedStack(context.Context, string, *template.Evaluation, []StackResource, *StackEvent, ...LifecycleMetadata) error
}

type stackDeletionValidator interface {
	ValidateStackDeletion(context.Context, string) error
}

type exportMutationValidator interface {
	ValidateExportMutation(context.Context, string, *template.Evaluation) error
}

type resourceAction struct {
	logicalID    string
	definition   template.Resource
	adapter      resource.Adapter
	physicalID   string
	capabilities []string
	attributes   map[string]any
	completed    bool
	failed       bool
}

type updateRollbackAction struct {
	logicalID         string
	oldDefinition     template.Resource
	newDefinition     template.Resource
	oldResource       StackResource
	resourceAdapter   resource.Adapter
	adapter           resource.UpdateCapable
	capabilities      []string
	oldAttributes     map[string]any
	sideEffectStarted bool
	completed         bool
}

type replacementPlan struct {
	logicalID     string
	oldDefinition template.Resource
	oldResource   StackResource
	oldAdapter    resource.Adapter
	definition    template.Resource
	adapter       resource.Adapter
}

// NewExecutor creates an executor using registry, or the production registry
// when registry is nil.
func NewExecutor(storage Storage, registry *resource.Registry) *Executor {
	if registry == nil {
		registry = resource.DefaultRegistry()
	}

	return &Executor{storage: storage, registry: registry}
}

// Create provisions all resources in dependency order.
//
//nolint:gocritic,wsl // LifecycleRequest is the public operation boundary.
func (e *Executor) Create(ctx context.Context, req LifecycleRequest) error {
	if err := validateExecutionOptions(req.Options); err != nil {
		return fmt.Errorf("validate create options: %w", err)
	}
	if err := validateLifecycleMetadata(req.Metadata); err != nil {
		return fmt.Errorf("validate create metadata: %w", err)
	}
	if err := e.runLifecycle(ctx, operationCreate, req, e.create); err != nil {
		return fmt.Errorf("run create stack operation: %w", err)
	}

	return nil
}

//nolint:gocritic,wsl // Lifecycle orchestration is intentionally explicit.
func (e *Executor) create(ctx context.Context, req LifecycleRequest) error {
	storage, err := e.storageForExecution()
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	if req.Template == nil {
		if err := validateEvaluation(req.Evaluation); err != nil {
			return err
		}
	}
	token := executionToken(req.Options.ClientRequestToken)
	stack, err := storage.GetStack(ctx, req.StackRef)
	if err != nil {
		return fmt.Errorf("get stack for create: %w", err)
	}
	if req.Template != nil {
		return e.createTemplate(ctx, storage, stack, req)
	}
	if err := e.preflight(req.Evaluation, req.Options.Capabilities, req.Options.ResourceTypes); err != nil {
		return err
	}
	order, err := evaluationOrder(req.Evaluation)
	if err != nil {
		return err
	}
	created := make([]resourceAction, 0, len(order))
	resources := make(map[string]StackResource, len(order))
	for _, logicalID := range order {
		definition := req.Evaluation.Resources[logicalID]
		adapter, _ := e.registry.Get(definition.Type)
		physicalID, callErr := e.createResource(ctx, storage, stack, token, logicalID, definition, adapter, req.Options.Capabilities, operationCreate)
		if physicalID != "" {
			created = append(created, resourceAction{logicalID: logicalID, definition: definition, adapter: adapter, physicalID: physicalID, capabilities: append([]string(nil), req.Options.Capabilities...)})
		}
		if callErr != nil {
			return e.failCreate(ctx, storage, stack, token, req.Evaluation, created, req.Options, callErr)
		}
		recordCompensation(req.compensation, &compensationAction{kind: compensateCreate, logicalID: logicalID, adapter: adapter, physicalID: physicalID})
		resources[logicalID] = stackResource(stack, logicalID, definition.Type, physicalID)
	}

	if err := e.commitFinalState(ctx, storage, stack, StackStatusCreateComplete, "", req.Evaluation, resources, token, req.Metadata, req.Imports); err != nil {
		return e.failCreate(ctx, storage, stack, token, req.Evaluation, created, req.Options, err)
	}

	return nil
}

//nolint:gocognit,cyclop,funlen,gocritic,goconst,wsl,nlreturn // Incremental evaluation and rollback form one lifecycle transaction.
func (e *Executor) createTemplate(ctx context.Context, storage lifecycleStorage, stack *Stack, req LifecycleRequest) error {
	token := executionToken(req.Options.ClientRequestToken)
	input := template.EvaluationInput{
		Parameters:         req.Parameters,
		PseudoParameters:   pseudoParameters(stack),
		Imports:            committedImportValues(ctx, storage, stack.StackID, req.Imports),
		ResourceAttributes: resourceAttributesFromStack(stack),
	}
	names, err := req.Template.ResourceNames(input)
	if err != nil {
		return fmt.Errorf("list template resources: %w", err)
	}
	remaining := make(map[string]struct{}, len(names))
	for _, name := range names {
		remaining[name] = struct{}{}
	}
	resources := make(map[string]StackResource, len(names))
	created := make([]resourceAction, 0, len(names))
	var lastEvaluationErr error
	for len(remaining) > 0 {
		progress := false
		for _, logicalID := range names {
			if _, ok := remaining[logicalID]; !ok {
				continue
			}
			partial, evalErr := req.Template.EvaluateResources(input, []string{logicalID})
			if evalErr != nil {
				lastEvaluationErr = evalErr
				continue
			}
			definition, active := partial.Resources[logicalID]
			if !active {
				delete(remaining, logicalID)
				progress = true
				continue
			}
			if !dependenciesReady(definition, input.ResourceAttributes) {
				continue
			}
			adapter, found := e.registry.Get(definition.Type)
			if !found {
				return e.failCreate(ctx, storage, stack, token, nil, created, req.Options, fmt.Errorf("resource type %q is not registered", definition.Type))
			}
			if err := e.preflight(&template.Evaluation{Resources: map[string]template.Resource{logicalID: definition}}, req.Options.Capabilities, req.Options.ResourceTypes); err != nil {
				return e.failCreate(ctx, storage, stack, token, nil, created, req.Options, err)
			}
			physicalID, createErr := e.createResource(ctx, storage, stack, token, logicalID, definition, adapter, req.Options.Capabilities, operationCreate)
			if physicalID != "" && req.Template != nil {
				created = append(created, resourceAction{logicalID: logicalID, definition: definition, adapter: adapter, physicalID: physicalID, capabilities: append([]string(nil), req.Options.Capabilities...)})
			}
			if createErr != nil {
				return e.failCreate(ctx, storage, stack, token, nil, created, req.Options, createErr)
			}
			recordCompensation(req.compensation, &compensationAction{kind: compensateCreate, logicalID: logicalID, adapter: adapter, physicalID: physicalID})
			if physicalID != "" && req.Template != nil {
				attributes := map[string]string{"Ref": physicalID}
				updatedStack, getStackErr := storage.GetStack(ctx, stack.StackID)
				if getStackErr != nil {
					return e.failCreate(ctx, storage, stack, token, nil, created, req.Options, fmt.Errorf("reload stack after creating resource %q: %w", logicalID, getStackErr))
				}
				for name, value := range latestResourceAttributes(updatedStack, logicalID) {
					attributes[name] = value
				}
				input.ResourceAttributes[logicalID] = attributes
			}
			resources[logicalID] = stackResource(stack, logicalID, definition.Type, physicalID)
			delete(remaining, logicalID)
			progress = true
		}
		if !progress {
			if lastEvaluationErr != nil {
				return e.failCreate(ctx, storage, stack, token, nil, created, req.Options, fmt.Errorf("evaluate resource dependencies: %w", lastEvaluationErr))
			}

			return e.failCreate(ctx, storage, stack, token, nil, created, req.Options, errors.New("unable to evaluate resource dependencies"))
		}
	}
	evaluation, err := req.Template.Evaluate(input)
	if err != nil {
		return e.failCreate(ctx, storage, stack, token, nil, created, req.Options, fmt.Errorf("evaluate completed template: %w", err))
	}
	if err := e.commitFinalState(ctx, storage, stack, StackStatusCreateComplete, "", evaluation, resources, token, req.Metadata, req.Imports); err != nil {
		return e.failCreate(ctx, storage, stack, token, evaluation, created, req.Options, err)
	}
	return nil
}

//nolint:gocritic,wsl // Failure handling intentionally preserves all rollback errors.
func (e *Executor) failCreate(ctx context.Context, storage lifecycleStorage, stack *Stack, token string, evaluation *template.Evaluation, created []resourceAction, options ExecutionOptions, cause error) error {
	var resources map[string]StackResource
	var rollbackErr error
	if createRetains(options) {
		resources = resourceEntries(stack, created)
	} else {
		resources, rollbackErr = e.rollbackCreated(ctx, storage, stack, token, created, options.RetainExceptOnCreate)
	}
	failureErr := e.commitFailure(ctx, storage, stack, StackStatusCreateFailed, cause.Error(), evaluation, resources, token, errorCodeOf(cause))

	return errors.Join(cause, rollbackErr, failureErr)
}

// Update applies the evaluated diff between the stored and requested states.
//
//nolint:gocritic,wsl // LifecycleRequest is the public operation boundary.
func (e *Executor) Update(ctx context.Context, req LifecycleRequest) error {
	if err := validateExecutionOptions(req.Options); err != nil {
		return fmt.Errorf("validate update options: %w", err)
	}
	if err := validateLifecycleMetadata(req.Metadata); err != nil {
		return fmt.Errorf("validate update metadata: %w", err)
	}
	if err := e.runLifecycle(ctx, operationUpdate, req, e.update); err != nil {
		return fmt.Errorf("run update stack operation: %w", err)
	}

	return nil
}

//
//nolint:cyclop,funlen,gocognit,gocyclo,gocritic,nlreturn,wsl,nestif // Diff planning and rollback are one lifecycle transaction.
func (e *Executor) update(ctx context.Context, req LifecycleRequest) error {
	storage, err := e.storageForExecution()
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	if req.Template == nil {
		if err := validateEvaluation(req.Evaluation); err != nil {
			return err
		}
	}
	token := executionToken(req.Options.ClientRequestToken)
	stack, err := storage.GetStack(ctx, req.StackRef)
	if err != nil {
		return fmt.Errorf("get stack for update: %w", err)
	}
	if validator, ok := storage.(exportMutationValidator); ok {
		if err := validator.ValidateExportMutation(ctx, req.StackRef, req.Evaluation); err != nil {
			return fmt.Errorf("validate export mutation: %w", err)
		}
	}
	oldEvaluation := stack.EvaluatedState
	if oldEvaluation == nil {
		return errors.New("stored evaluated state is required for update")
	}
	if err := e.preflight(req.Evaluation, req.Options.Capabilities, req.Options.ResourceTypes); err != nil {
		return err
	}
	if req.Metadata != nil && req.Metadata.Tags != nil {
		stack.Tags = cloneTags(req.Metadata.Tags)
	}
	order, err := evaluationOrder(req.Evaluation)
	if err != nil {
		return err
	}
	oldResources := stackResourceMap(stack.Resources)
	var updateInput *template.EvaluationInput
	if req.Template != nil {
		updateInput = &template.EvaluationInput{
			Parameters:         cloneStringMap(req.Parameters),
			PseudoParameters:   pseudoParameters(stack),
			Imports:            committedImportValues(ctx, storage, stack.StackID, req.Imports),
			ResourceAttributes: resourceAttributesFromStack(stack),
		}
	}
	if err := e.validateUpdatePolicies(req.Evaluation, oldEvaluation, oldResources); err != nil {
		return err
	}
	if err := e.preflightInPlaceUpdates(oldEvaluation, req.Evaluation, oldResources); err != nil {
		return err
	}
	newResources := make(map[string]StackResource, len(req.Evaluation.Resources))
	for logicalID, oldResource := range oldResources {
		if _, exists := req.Evaluation.Resources[logicalID]; exists {
			newResources[logicalID] = oldResource
		}
	}
	created := make([]resourceAction, 0)
	deleted := make([]resourceAction, 0)
	updated := make([]updateRollbackAction, 0)
	inPlacePlans := make(map[string]updateRollbackAction)
	replacements := make(map[string]replacementPlan)

	for logicalID, oldDefinition := range oldEvaluation.Resources {
		nextDefinition, exists := req.Evaluation.Resources[logicalID]
		if !exists {
			continue
		}
		oldResource, hasOldResource := oldResources[logicalID]
		if !hasOldResource {
			continue
		}
		adapter, found := e.registry.Get(nextDefinition.Type)
		if !found {
			return fmt.Errorf("resource type %q is not registered", nextDefinition.Type)
		}
		oldAdapter, found := e.registry.Get(oldDefinition.Type)
		if !found {
			return fmt.Errorf("resource type %q is not registered", oldDefinition.Type)
		}
		changed, replacement := oldDefinition.Type != nextDefinition.Type, oldDefinition.Type != nextDefinition.Type
		if oldDefinition.Type == nextDefinition.Type {
			changed, replacement = classifyChanges(oldDefinition, nextDefinition, adapter.Schema())
		}
		if !changed {
			continue
		}
		if !replacement {
			if hasNoInterruptionChange(oldDefinition, nextDefinition, adapter.Schema()) {
				updater, ok := adapter.(resource.UpdateCapable)
				if !ok {
					return fmt.Errorf("resource %q adapter does not support no-interruption updates", logicalID)
				}
				journal := journaledAction(stack, logicalID, oldResource.PhysicalResourceID, oldDefinition, adapter, req.Options.Capabilities)
				inPlacePlans[logicalID] = updateRollbackAction{logicalID: logicalID, oldDefinition: oldDefinition, newDefinition: nextDefinition, oldResource: oldResource, resourceAdapter: adapter, adapter: updater, capabilities: journal.capabilities, oldAttributes: journal.attributes}
			}
			continue
		}
		if fixedCustomName(oldDefinition, nextDefinition, adapter.Schema()) {
			return fmt.Errorf("resource %q replacement cannot keep a fixed custom name", logicalID)
		}
		replacements[logicalID] = replacementPlan{
			logicalID:     logicalID,
			oldDefinition: oldDefinition,
			oldResource:   oldResource,
			oldAdapter:    oldAdapter,
			definition:    nextDefinition,
			adapter:       adapter,
		}
	}

	for _, logicalID := range order {
		definition := req.Evaluation.Resources[logicalID]
		_, existed := oldEvaluation.Resources[logicalID]
		_, hasOldResource := oldResources[logicalID]
		adapter, _ := e.registry.Get(definition.Type)
		if plan, inPlace := inPlacePlans[logicalID]; inPlace {
			if req.Template != nil {
				definition, err = e.evaluateUpdateResource(&req, updateInput, logicalID, definition)
				if err != nil {
					return e.failUpdate(ctx, storage, stack, token, oldEvaluation, req.Evaluation, stack.Resources, created, deleted, updated, req.Options, err)
				}
			}
			plan.newDefinition = definition
			updated = append(updated, plan)
			updateIndex := len(updated) - 1
			if err := e.updateResource(ctx, storage, stack, token, logicalID, plan.oldResource, plan.oldDefinition, definition, plan.adapter, plan.capabilities, plan.oldAttributes, operationUpdate, func() { updated[updateIndex].sideEffectStarted = true }); err != nil {
				return e.failUpdate(ctx, storage, stack, token, oldEvaluation, req.Evaluation, stack.Resources, created, deleted, updated, req.Options, err)
			}
			updated[updateIndex].completed = true
			recordCompensation(req.compensation, &compensationAction{kind: compensateUpdate, logicalID: logicalID, adapter: plan.resourceAdapter, updater: plan.adapter, physicalID: plan.oldResource.PhysicalResourceID, definition: plan.oldDefinition, previousDefinition: definition, capabilities: plan.capabilities, tags: stackTagMap(stack.Tags)})
			oldResource := plan.oldResource
			oldResource.ResourceStatus = ResourceStatusUpdateComplete
			oldResource.Timestamp = time.Now()
			newResources[logicalID] = oldResource
			if req.Template != nil {
				if err := refreshUpdateResourceAttributes(ctx, storage, stack, logicalID, oldResource.PhysicalResourceID, updateInput.ResourceAttributes); err != nil {
					return e.failUpdate(ctx, storage, stack, token, oldEvaluation, req.Evaluation, stack.Resources, created, deleted, updated, req.Options, err)
				}
			}
			continue
		}
		if !existed || !hasOldResource {
			if req.Template != nil {
				definition, err = e.evaluateUpdateResource(&req, updateInput, logicalID, definition)
				if err != nil {
					return e.failUpdate(ctx, storage, stack, token, oldEvaluation, req.Evaluation, stack.Resources, created, deleted, updated, req.Options, err)
				}
				adapter, _ = e.registry.Get(definition.Type)
			}
			physicalID, callErr := e.createResource(ctx, storage, stack, token, logicalID, definition, adapter, req.Options.Capabilities, operationUpdate)
			if physicalID != "" {
				created = append(created, resourceAction{logicalID: logicalID, definition: definition, adapter: adapter, physicalID: physicalID, capabilities: append([]string(nil), req.Options.Capabilities...)})
			}
			if callErr != nil {
				if len(created) > 0 {
					created[len(created)-1].failed = true
				}
				return e.failUpdate(ctx, storage, stack, token, oldEvaluation, req.Evaluation, stack.Resources, created, deleted, updated, req.Options, callErr)
			}
			recordCompensation(req.compensation, &compensationAction{kind: compensateCreate, logicalID: logicalID, adapter: adapter, physicalID: physicalID})
			if len(created) > 0 {
				created[len(created)-1].completed = true
			}
			newResources[logicalID] = stackResource(stack, logicalID, definition.Type, physicalID)
			newResources[logicalID] = updateResourceStatus(newResources[logicalID], operationUpdate)
			if physicalID != "" && req.Template != nil {
				if refreshErr := refreshUpdateResourceAttributes(ctx, storage, stack, logicalID, physicalID, updateInput.ResourceAttributes); refreshErr != nil {
					return e.failUpdate(ctx, storage, stack, token, oldEvaluation, req.Evaluation, stack.Resources, created, deleted, updated, req.Options, refreshErr)
				}
			}
			continue
		}
		if plan, replacement := replacements[logicalID]; replacement {
			if req.Template != nil {
				plan.definition, err = e.evaluateUpdateResource(&req, updateInput, logicalID, plan.definition)
				if err != nil {
					return e.failUpdate(ctx, storage, stack, token, oldEvaluation, req.Evaluation, stack.Resources, created, deleted, updated, req.Options, err)
				}
			}
			physicalID, callErr := e.createResource(ctx, storage, stack, token, logicalID, plan.definition, plan.adapter, req.Options.Capabilities, operationUpdate)
			if physicalID != "" {
				created = append(created, resourceAction{logicalID: logicalID, definition: plan.definition, adapter: plan.adapter, physicalID: physicalID, capabilities: append([]string(nil), req.Options.Capabilities...)})
			}
			if callErr != nil {
				if len(created) > 0 {
					created[len(created)-1].failed = true
				}
				return e.failUpdate(ctx, storage, stack, token, oldEvaluation, req.Evaluation, stack.Resources, created, deleted, updated, req.Options, callErr)
			}
			recordCompensation(req.compensation, &compensationAction{kind: compensateCreate, logicalID: logicalID, adapter: plan.adapter, physicalID: physicalID})
			if len(created) > 0 {
				created[len(created)-1].completed = true
			}
			newResources[logicalID] = stackResource(stack, logicalID, plan.definition.Type, physicalID)
			newResources[logicalID] = updateResourceStatus(newResources[logicalID], operationUpdate)
			if physicalID != "" && req.Template != nil {
				if refreshErr := refreshUpdateResourceAttributes(ctx, storage, stack, logicalID, physicalID, updateInput.ResourceAttributes); refreshErr != nil {
					return e.failUpdate(ctx, storage, stack, token, oldEvaluation, req.Evaluation, stack.Resources, created, deleted, updated, req.Options, refreshErr)
				}
			}
		}
	}

	oldOrder, err := evaluationOrder(oldEvaluation)
	if err != nil {
		return err
	}
	for index := len(oldOrder) - 1; index >= 0; index-- {
		logicalID := oldOrder[index]
		plan, replacing := replacements[logicalID]
		if _, exists := req.Evaluation.Resources[logicalID]; exists && !replacing {
			continue
		}
		oldDefinition, exists := oldEvaluation.Resources[logicalID]
		oldResource, hasResource := oldResources[logicalID]
		if replacing {
			oldDefinition = plan.oldDefinition
			oldResource = plan.oldResource
			exists = true
			hasResource = true
		}
		if !exists || !hasResource {
			continue
		}
		adapter, found := e.registry.Get(oldDefinition.Type)
		if !found {
			return e.failUpdate(ctx, storage, stack, token, oldEvaluation, req.Evaluation, stack.Resources, created, deleted, updated, req.Options, fmt.Errorf("resource type %q is not registered", oldDefinition.Type))
		}
		if replacing {
			adapter = plan.oldAdapter
		}
		if replacing && deletionPolicy(plan.definition, true) == retainPolicy {
			continue
		}
		if !replacing && deletionPolicy(oldDefinition, false) == retainPolicy {
			continue
		}
		action := journaledAction(stack, logicalID, oldResource.PhysicalResourceID, oldDefinition, adapter, req.Options.Capabilities)
		deletedOK, deleteErr := e.deleteResource(ctx, storage, stack, token, action, operationUpdate)
		if deletedOK {
			deleted = append(deleted, action)
			recordCompensation(req.compensation, &compensationAction{kind: compensateDelete, logicalID: logicalID, adapter: action.adapter, physicalID: action.physicalID, definition: action.definition, capabilities: action.capabilities, tags: stackTagMap(stack.Tags)})
		}
		if deleteErr != nil {
			return e.failUpdate(ctx, storage, stack, token, oldEvaluation, req.Evaluation, stack.Resources, created, deleted, updated, req.Options, deleteErr)
		}
		if !replacing {
			delete(newResources, logicalID)
		}
	}

	finalEvaluation := req.Evaluation
	if req.Template != nil {
		finalEvaluation, err = req.Template.Evaluate(*updateInput)
		if err != nil {
			return e.failUpdate(ctx, storage, stack, token, oldEvaluation, req.Evaluation, stack.Resources, created, deleted, updated, req.Options, fmt.Errorf("evaluate final template: %w", err))
		}
	}
	if err := e.commitFinalState(ctx, storage, stack, StackStatusUpdateComplete, "", finalEvaluation, newResources, token, req.Metadata, req.Imports); err != nil {
		return e.failUpdate(ctx, storage, stack, token, oldEvaluation, req.Evaluation, stack.Resources, created, deleted, updated, req.Options, err)
	}
	return nil
}

//nolint:cyclop,wsl,nlreturn // The preflight keeps classification and capability checks together.
func (e *Executor) preflightInPlaceUpdates(oldEvaluation, nextEvaluation *template.Evaluation, oldResources map[string]StackResource) error {
	for logicalID, oldDefinition := range oldEvaluation.Resources {
		nextDefinition, exists := nextEvaluation.Resources[logicalID]
		if !exists || oldDefinition.Type != nextDefinition.Type {
			continue
		}
		if _, exists := oldResources[logicalID]; !exists {
			continue
		}
		adapter, found := e.registry.Get(nextDefinition.Type)
		if !found {
			return fmt.Errorf("resource type %q is not registered", nextDefinition.Type)
		}
		schema := adapter.Schema()
		keys := make(map[string]struct{}, len(oldDefinition.Properties)+len(nextDefinition.Properties))
		for property := range oldDefinition.Properties {
			keys[property] = struct{}{}
		}
		for property := range nextDefinition.Properties {
			keys[property] = struct{}{}
		}
		for property := range keys {
			if reflect.DeepEqual(oldDefinition.Properties[property], nextDefinition.Properties[property]) {
				continue
			}
			update, supported := schema.PropertyUpdate[property]
			if !supported || (update != resource.PropertyUpdate("NoInterruption") && update != resource.PropertyUpdate("Replacement")) {
				return fmt.Errorf("resource %q property %q has unsupported update behavior %q", logicalID, property, update)
			}
		}
		changed, replacement := classifyChanges(oldDefinition, nextDefinition, schema)
		if changed && !replacement && hasNoInterruptionChange(oldDefinition, nextDefinition, schema) {
			if _, ok := adapter.(resource.UpdateCapable); !ok {
				return fmt.Errorf("resource %q adapter does not support no-interruption updates", logicalID)
			}
		}
	}
	return nil
}

// Delete removes resources in reverse dependency order and commits a deleted stack state.
//
//nolint:gocritic,wsl // LifecycleRequest is the public operation boundary.
func (e *Executor) Delete(ctx context.Context, req LifecycleRequest) error {
	if err := validateExecutionOptions(req.Options); err != nil {
		return fmt.Errorf("validate delete options: %w", err)
	}
	if err := e.deletePreflight(ctx, req); err != nil {
		return fmt.Errorf("run delete stack operation: %w", err)
	}
	if err := e.runLifecycle(ctx, operationDelete, req, e.delete); err != nil {
		return fmt.Errorf("run delete stack operation: %w", err)
	}

	return nil
}

type lifecycleExecutorFunc func(context.Context, LifecycleRequest) error

//nolint:cyclop,funlen,gocognit,gocritic,wsl // Reservation, failure persistence, and completion are one operation boundary.
func (e *Executor) runLifecycle(ctx context.Context, operation string, req LifecycleRequest, execute lifecycleExecutorFunc) error {
	storage, err := e.storageForExecution()
	if err != nil {
		return err
	}
	token := executionToken(req.Options.ClientRequestToken)
	req.Options.DisableRollback = effectiveDisableRollback(req.Options.DisableRollback, req.Options.DeploymentConfig)
	fingerprint, err := lifecycleFingerprint(operation, req)
	if err != nil {
		return err
	}
	req.Options.ClientRequestToken = token
	ctx = context.WithValue(ctx, executionTokenContextKey{}, token)

	if err := storage.RunExclusiveStackOperation(ctx, req.StackRef, func(operationCtx context.Context) error {
		operationCtx = context.WithoutCancel(operationCtx)
		if req.BaseRevisionSet {
			current, revisionErr := storage.GetStack(operationCtx, req.StackRef)
			if revisionErr != nil {
				return fmt.Errorf("get stack for revision check: %w", revisionErr)
			}
			if current.Revision != req.BaseRevision {
				return &Error{Code: revisionConflictException, Message: fmt.Sprintf("stack revision %d is stale; current revision is %d", req.BaseRevision, current.Revision)}
			}
		}
		record, replayed, err := storage.BeginExecution(operationCtx, req.StackRef, operation, token, fingerprint)
		if err != nil {
			return fmt.Errorf("begin execution: %w", err)
		}
		if replayed {
			if record.Success {
				return nil
			}
			if record.Error == "" {
				return fmt.Errorf("previous %s operation failed", operation)
			}
			if record.ErrorCode != "" {
				return &Error{Code: record.ErrorCode, Message: record.Error}
			}

			return errors.New(record.Error)
		}

		operationErr := execute(operationCtx, req)
		if operationErr != nil {
			operationErr = errors.Join(operationErr, e.persistLifecycleFailure(operationCtx, storage, operation, req, operationErr))
		}
		errorCode := errorCodeOf(operationErr)
		errorReason := errorString(operationErr)
		if errorCode != "" {
			var cfnErr *Error
			if errors.As(operationErr, &cfnErr) {
				errorReason = cfnErr.Message
			}
		} else if operationErr != nil {
			errorCode = errInternalError
			errorReason = "Internal server error"
		}
		completionErr := storage.CompleteExecution(operationCtx, req.StackRef, token, operationErr == nil, errorCode, errorReason)

		return errors.Join(operationErr, completionErr)
	}); err != nil {
		return fmt.Errorf("run %s lifecycle operation: %w", operation, err)
	}

	return nil
}

//nolint:gocritic,wsl // The preflight callback matches the lifecycle executor seam.
func (e *Executor) deletePreflight(ctx context.Context, req LifecycleRequest) error {
	storage, err := e.storageForExecution()
	if err != nil {
		return err
	}
	stack, err := storage.GetStack(ctx, req.StackRef)
	if err != nil {
		return fmt.Errorf("get stack for delete preflight: %w", err)
	}
	if stack.EnableTerminationProtection {
		return &Error{Code: errInvalidParameter, Message: "Termination protection is enabled"}
	}
	if stack.StackStatus == StackStatusDeleteComplete {
		return nil
	}
	if validator, ok := storage.(stackDeletionValidator); ok {
		if err := validator.ValidateStackDeletion(ctx, req.StackRef); err != nil {
			return fmt.Errorf("validate stack deletion: %w", err)
		}
	}

	return nil
}

//nolint:gocritic,wsl // The fingerprint includes the complete immutable request value.
func lifecycleFingerprint(operation string, req LifecycleRequest) (string, error) {
	value := struct {
		Operation  string
		StackRef   string
		Evaluation *template.Evaluation
		Metadata   *LifecycleMetadata
		Options    struct {
			Capabilities             []string
			OnFailure                FailurePolicy
			DisableRollback          bool
			RetainResourcesOnFail    bool
			RetainResourcesOnFailure bool
			RetainResources          []string
			DeletionMode             string
			RoleARN                  string
			ResourceTypes            []string
			DeploymentConfig         *DeploymentConfig
			RollbackConfiguration    *RollbackConfiguration
			Tags                     []Tag
			RetainExceptOnCreate     bool
		}
	}{Operation: operation, StackRef: req.StackRef, Evaluation: cloneEvaluation(req.Evaluation), Metadata: cloneLifecycleMetadata(req.Metadata)}
	if value.Evaluation != nil {
		for logicalID, definition := range value.Evaluation.Resources {
			definition.DependsOn = append([]string(nil), definition.DependsOn...)
			definition.Dependencies = append([]string(nil), definition.Dependencies...)
			sort.Strings(definition.DependsOn)
			sort.Strings(definition.Dependencies)
			value.Evaluation.Resources[logicalID] = definition
		}
	}
	value.Options.Capabilities = req.Options.Capabilities
	value.Options.OnFailure = req.Options.OnFailure
	value.Options.DisableRollback = req.Options.DisableRollback
	value.Options.RetainResourcesOnFail = req.Options.RetainResourcesOnFail
	value.Options.RetainResourcesOnFailure = req.Options.RetainResourcesOnFailure
	value.Options.RetainResources = append([]string(nil), req.Options.RetainResources...)
	value.Options.DeletionMode = req.Options.DeletionMode
	value.Options.RoleARN = req.Options.RoleARN
	value.Options.ResourceTypes = append([]string(nil), req.Options.ResourceTypes...)
	value.Options.DeploymentConfig = cloneDeploymentConfig(req.Options.DeploymentConfig)
	value.Options.RollbackConfiguration = cloneRollbackConfiguration(req.Options.RollbackConfiguration)
	value.Options.Tags = cloneTags(req.Options.Tags)
	value.Options.RetainExceptOnCreate = req.Options.RetainExceptOnCreate
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("fingerprint lifecycle request: %w", err)
	}

	hash := sha256.Sum256(data)

	return hex.EncodeToString(hash[:]), nil
}

//nolint:gocritic,wsl // Failure persistence needs the original request and current stack snapshot.
func (e *Executor) persistLifecycleFailure(ctx context.Context, storage lifecycleStorage, operation string, req LifecycleRequest, cause error) error {
	stack, err := storage.GetStack(ctx, req.StackRef)
	if err != nil {
		return fmt.Errorf("get stack for failure persistence: %w", err)
	}
	status := map[string]string{operationCreate: StackStatusCreateFailed, operationUpdate: StackStatusUpdateFailed, operationDelete: StackStatusDeleteFailed}[operation]
	evaluation := stack.EvaluatedState
	if evaluation == nil {
		evaluation = req.Evaluation
	}
	if operation == operationCreate && req.Options.OnFailure == FailurePolicyDelete {
		if stack.StackStatus != StackStatusCreateFailed || !hasLifecycleEvent(stack.Events, req.Options.ClientRequestToken, StackStatusCreateFailed) {
			if err := e.commitFailure(ctx, storage, stack, status, cause.Error(), evaluation, stackResourceMap(stack.Resources), req.Options.ClientRequestToken, errorCodeOf(cause)); err != nil {
				return err
			}
		}

		return fmt.Errorf("persist create deletion failure: %w", storage.MarkCreateFailed(ctx, stack.StackID, cause.Error(), errorCodeOf(cause), true, req.Options.ClientRequestToken))
	}
	if stack.StackStatus == status && hasLifecycleEvent(stack.Events, req.Options.ClientRequestToken, status) {
		return nil
	}

	return e.commitFailure(ctx, storage, stack, status, cause.Error(), evaluation, stackResourceMap(stack.Resources), req.Options.ClientRequestToken, errorCodeOf(cause))
}

func hasLifecycleEvent(events []StackEvent, token, status string) bool {
	for index := range events {
		if events[index].ClientRequestToken == token && events[index].ResourceStatus == status {
			return true
		}
	}

	return false
}

func errorString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

func errorCodeOf(err error) string {
	var cfnErr *Error
	if errors.As(err, &cfnErr) {
		return cfnErr.Code
	}

	return ""
}

//
//nolint:cyclop,funlen,gocritic,nlreturn,wsl // Deletion policy checks and reverse cleanup are one transaction.
func (e *Executor) delete(ctx context.Context, req LifecycleRequest) error {
	storage, err := e.storageForExecution()
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	token := executionToken(req.Options.ClientRequestToken)
	stack, err := storage.GetStack(ctx, req.StackRef)
	if err != nil {
		return fmt.Errorf("get stack for delete: %w", err)
	}
	evaluation := stack.EvaluatedState
	if evaluation == nil {
		evaluation = evaluationFromStack(stack)
	}
	oldOrder, err := evaluationOrder(evaluation)
	if err != nil {
		return err
	}
	resources := stackResourceMap(stack.Resources)
	if err := e.validateDeletePolicies(evaluation, resources); err != nil {
		return err
	}
	for index := len(oldOrder) - 1; index >= 0; index-- {
		logicalID := oldOrder[index]
		definition, exists := evaluation.Resources[logicalID]
		stored, hasResource := resources[logicalID]
		if !exists || !hasResource || deletionPolicy(definition, false) == retainPolicy {
			continue
		}
		if slices.Contains(req.Options.RetainResources, logicalID) {
			continue
		}
		adapter, found := e.registry.Get(definition.Type)
		if !found {
			return e.failDelete(ctx, storage, stack, token, resources, fmt.Errorf("resource type %q is not registered", definition.Type))
		}
		action := journaledAction(stack, logicalID, stored.PhysicalResourceID, definition, adapter, nil)
		deletedOK, deleteErr := e.deleteResource(ctx, storage, stack, token, action, operationDelete)
		if deletedOK {
			stored.ResourceStatus = ResourceStatusDeleteComplete
			resources[logicalID] = stored
			recordCompensation(req.compensation, &compensationAction{kind: compensateDelete, logicalID: logicalID, adapter: adapter, physicalID: stored.PhysicalResourceID, definition: definition, tags: stackTagMap(stack.Tags)})
		}
		if deleteErr != nil {
			stored.ResourceStatus = ResourceStatusDeleteFailed
			resources[logicalID] = stored
			if req.Options.DeletionMode != deletionModeForce {
				return e.failDelete(ctx, storage, stack, token, resources, deleteErr)
			}
		}
	}
	event := &StackEvent{EventID: uuid.NewString(), StackID: stack.StackID, StackName: stack.StackName, ResourceStatus: StackStatusDeleteComplete, ClientRequestToken: token, Timestamp: time.Now()}
	if err := storage.FinalizeDeletedStack(ctx, stack.StackID, evaluation, sortedStackResources(resources), event, lifecycleMetadataForDelete(&req.Options)); err != nil {
		failureErr := e.commitFailure(ctx, storage, stack, StackStatusDeleteFailed, err.Error(), evaluation, resources, token, errorCodeOf(err))
		return errors.Join(err, failureErr)
	}
	return nil
}

//nolint:gocritic,wsl // Journal boundaries deliberately surround one adapter call.
func (e *Executor) createResource(ctx context.Context, storage lifecycleStorage, stack *Stack, token, logicalID string, definition template.Resource, adapter resource.Adapter, capabilities []string, phase string) (string, error) {
	entry := StackJournalEntry{LogicalResourceID: logicalID, ResourceType: definition.Type, Properties: definition.Properties, Capabilities: append([]string(nil), capabilities...)}
	inProgressStatus, failedStatus, completeStatus := sideEffectStatuses(phase, false)
	if err := e.persistResourceStatus(ctx, storage, stack, logicalID, definition.Type, "", inProgressStatus, inProgressStatus); err != nil {
		return "", err
	}
	if err := e.recordSideEffect(ctx, storage, stack, token, "CreateResource", journalBefore, inProgressStatus, entry); err != nil {
		return "", err
	}
	result, err := adapter.Create(context.WithoutCancel(ctx), resource.CreateRequest{Properties: cloneAnyMap(definition.Properties), Capabilities: append([]string(nil), capabilities...), Tags: stackTagMap(stack.Tags)})
	if err != nil {
		entry.Reason = err.Error()
		journalErr := e.recordSideEffect(ctx, storage, stack, token, "CreateResource", journalFailed, failedStatus, entry)

		return "", errors.Join(fmt.Errorf("create resource %q: %w", logicalID, err), journalErr)
	}
	physicalID := result.Identifier
	if physicalID == "" {
		physicalID = logicalID + "-" + uuid.NewString()
	}
	entry.Identifier = physicalID
	entry.PhysicalResourceID = physicalID
	entry.Attributes = cloneAnyMap(result.Attributes)
	if err := e.recordSideEffect(ctx, storage, stack, token, "CreateResource", journalAfter, completeStatus, entry); err != nil {
		return physicalID, err
	}

	return physicalID, nil
}

//nolint:gocritic,wsl,nlreturn // The adapter update journal is one transactional operation.
func (e *Executor) updateResource(ctx context.Context, storage lifecycleStorage, stack *Stack, token, logicalID string, oldResource StackResource, oldDefinition, definition template.Resource, adapter resource.UpdateCapable, capabilities []string, fallbackAttributes map[string]any, phase string, beforeCall func()) error {
	entry := StackJournalEntry{LogicalResourceID: logicalID, ResourceType: definition.Type, PhysicalResourceID: oldResource.PhysicalResourceID, Identifier: oldResource.PhysicalResourceID, Properties: definition.Properties, Capabilities: append([]string(nil), capabilities...)}
	inProgressStatus, failedStatus, completeStatus := sideEffectStatuses(phase, false)
	if err := e.persistResourceStatus(ctx, storage, stack, logicalID, definition.Type, oldResource.PhysicalResourceID, inProgressStatus, inProgressStatus); err != nil {
		return err
	}
	if err := e.recordSideEffect(ctx, storage, stack, token, "UpdateResource", journalBefore, inProgressStatus, entry); err != nil {
		return err
	}
	if beforeCall != nil {
		beforeCall()
	}
	result, err := adapter.Update(context.WithoutCancel(ctx), resource.UpdateRequest{
		Identifier:         oldResource.PhysicalResourceID,
		Properties:         cloneAnyMap(definition.Properties),
		PreviousProperties: cloneAnyMap(oldDefinition.Properties),
		Capabilities:       append([]string(nil), capabilities...),
		Tags:               stackTagMap(stack.Tags),
	})
	if err != nil {
		entry.Reason = err.Error()
		statusErr := e.persistResourceStatus(ctx, storage, stack, logicalID, definition.Type, oldResource.PhysicalResourceID, failedStatus, failedStatus)
		return errors.Join(fmt.Errorf("update resource %q: %w", logicalID, err), statusErr, e.recordSideEffect(ctx, storage, stack, token, "UpdateResource", journalFailed, failedStatus, entry))
	}
	entry.Attributes = cloneAnyMap(fallbackAttributes)
	for name, value := range result.Attributes {
		if entry.Attributes == nil {
			entry.Attributes = make(map[string]any)
		}
		entry.Attributes[name] = cloneAny(value)
	}
	if err := e.recordSideEffect(ctx, storage, stack, token, "UpdateResource", journalAfter, completeStatus, entry); err != nil {
		entry.Reason = err.Error()

		return errors.Join(err, e.recordSideEffect(ctx, storage, stack, token, "UpdateResource", journalFailed, failedStatus, entry))
	}

	return nil
}

//nolint:gocritic,wsl // Journal boundaries deliberately surround one adapter call.
func (e *Executor) deleteResource(ctx context.Context, storage lifecycleStorage, stack *Stack, token string, action resourceAction, phase string) (bool, error) {
	entry := StackJournalEntry{LogicalResourceID: action.logicalID, PhysicalResourceID: action.physicalID, ResourceType: action.definition.Type, Identifier: action.physicalID, Properties: action.definition.Properties, Capabilities: append([]string(nil), action.capabilities...)}
	inProgressStatus, failedStatus, completeStatus := sideEffectStatuses(phase, true)
	if err := e.persistResourceStatus(ctx, storage, stack, action.logicalID, action.definition.Type, action.physicalID, inProgressStatus, inProgressStatus); err != nil {
		return false, err
	}
	if err := e.recordSideEffect(ctx, storage, stack, token, "DeleteResource", journalBefore, inProgressStatus, entry); err != nil {
		return false, err
	}
	err := action.adapter.Delete(context.WithoutCancel(ctx), resource.DeleteRequest{Identifier: action.physicalID})
	if err != nil {
		entry.Reason = err.Error()
		journalErr := e.recordSideEffect(ctx, storage, stack, token, "DeleteResource", journalFailed, failedStatus, entry)
		statusErr := e.persistResourceStatus(ctx, storage, stack, action.logicalID, action.definition.Type, action.physicalID, failedStatus, failedStatus)

		return false, errors.Join(fmt.Errorf("delete resource %q: %w", action.logicalID, err), statusErr, journalErr)
	}

	return true, e.recordSideEffect(ctx, storage, stack, token, "DeleteResource", journalAfter, completeStatus, entry)
}

//nolint:wsl // Rollback order is intentionally a compact reverse traversal.
func (e *Executor) rollbackCreated(ctx context.Context, storage lifecycleStorage, stack *Stack, token string, created []resourceAction, retainExceptOnCreate bool) (map[string]StackResource, error) {
	var rollbackErr error
	resources := make(map[string]StackResource)
	for index := len(created) - 1; index >= 0; index-- {
		if !retainExceptOnCreate && deletionPolicy(created[index].definition, false) == retainPolicy {
			resources[created[index].logicalID] = stackResource(stack, created[index].logicalID, created[index].definition.Type, created[index].physicalID)

			continue
		}
		deleted, err := e.deleteResource(ctx, storage, stack, token, created[index], "ROLLBACK")
		if err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
			if !deleted {
				stored := stackResource(stack, created[index].logicalID, created[index].definition.Type, created[index].physicalID)
				stored.ResourceStatus = ResourceStatusDeleteFailed
				resources[created[index].logicalID] = stored
			}
		}
	}

	return resources, rollbackErr
}

//nolint:gocritic,wsl // Failure handling keeps compensation and durable failure state together.
//nolint:wsl // Rollback steps deliberately keep their failure handling together.
func (e *Executor) failUpdate(ctx context.Context, storage lifecycleStorage, stack *Stack, token string, oldEvaluation, requestedEvaluation *template.Evaluation, oldResources []StackResource, created, deleted []resourceAction, updated []updateRollbackAction, options ExecutionOptions, cause error) error {
	if options.DisableRollback {
		resourceState := stack
		if current, getErr := storage.GetStack(ctx, stack.StackID); getErr == nil {
			resourceState = current
		}
		resources := failedUpdateResources(resourceState, created, deleted, updated)
		evaluation := failedUpdateEvaluation(oldEvaluation, requestedEvaluation, oldResources, deleted)
		failure := e.commitFailure(ctx, storage, stack, StackStatusUpdateFailed, cause.Error(), evaluation, resources, token)

		return errors.Join(cause, failure)
	}
	rollbackResources, rollbackErr := e.rollbackCreated(ctx, storage, stack, token, created, true)
	restoredResources := stackResourceMap(oldResources)
	for index := len(deleted) - 1; index >= 0; index-- {
		action := deleted[index]
		delete(restoredResources, action.logicalID)
		physicalID, err := e.createResource(ctx, storage, stack, token, action.logicalID, action.definition, action.adapter, action.capabilities, "ROLLBACK")
		if err != nil {
			if physicalID != "" {
				_, compensationErr := e.deleteResource(ctx, storage, stack, token, resourceAction{
					logicalID:    action.logicalID,
					definition:   action.definition,
					adapter:      action.adapter,
					physicalID:   physicalID,
					capabilities: action.capabilities,
				}, "ROLLBACK")
				rollbackErr = errors.Join(rollbackErr, compensationErr)
			}
			if physicalID == "" {
				physicalID = action.physicalID
			}
			rollbackErr = errors.Join(rollbackErr, err)
			failed := stackResource(stack, action.logicalID, action.definition.Type, physicalID)
			failed.ResourceStatus = ResourceStatusCreateFailed
			restoredResources[action.logicalID] = failed

			continue
		}
		stored := stackResource(stack, action.logicalID, action.definition.Type, physicalID)
		restoredResources[action.logicalID] = stored
	}
	for logicalID, resource := range rollbackResources {
		restoredResources[logicalID] = resource
	}
	failedUpdates, updateErr := e.rollbackUpdated(ctx, storage, stack, token, updated)
	for logicalID, resource := range failedUpdates {
		restoredResources[logicalID] = resource
	}
	rollbackErr = errors.Join(rollbackErr, updateErr)
	failure := errors.Join(cause, rollbackErr)
	failure = errors.Join(failure, e.commitFailure(ctx, storage, stack, StackStatusUpdateFailed, failure.Error(), oldEvaluation, restoredResources, token))

	return failure
}

//nolint:wsl // The projection keeps each side-effect class visibly separate.
func failedUpdateResources(stack *Stack, created, deleted []resourceAction, updated []updateRollbackAction) map[string]StackResource {
	resources := stackResourceMap(stack.Resources)
	for index := range created {
		action := &created[index]
		if action.physicalID == "" {
			continue
		}
		stored := stackResource(stack, action.logicalID, action.definition.Type, action.physicalID)
		stored.ResourceStatus = ResourceStatusUpdateComplete
		if action.failed {
			stored.ResourceStatus = ResourceStatusUpdateFailed
		}
		resources[action.logicalID] = stored
	}
	for index := range updated {
		action := &updated[index]
		stored, exists := resources[action.logicalID]
		if !exists {
			stored = action.oldResource
		}
		if action.completed {
			stored.ResourceStatus = ResourceStatusUpdateComplete
		} else if action.sideEffectStarted {
			stored.ResourceStatus = ResourceStatusUpdateFailed
		}
		resources[action.logicalID] = stored
	}
	for index := range deleted {
		action := &deleted[index]
		delete(resources, action.logicalID)
	}

	return resources
}

//nolint:wsl // The evaluation projection keeps requested and retained resources distinct.
func failedUpdateEvaluation(oldEvaluation, requestedEvaluation *template.Evaluation, oldResources []StackResource, deleted []resourceAction) *template.Evaluation {
	evaluation := cloneEvaluation(requestedEvaluation)
	if evaluation == nil {
		evaluation = cloneEvaluation(oldEvaluation)
	}
	if evaluation == nil {
		evaluation = &template.Evaluation{Resources: make(map[string]template.Resource)}
	}
	deletedIDs := make(map[string]struct{}, len(deleted))
	for index := range deleted {
		action := &deleted[index]
		deletedIDs[action.logicalID] = struct{}{}
	}
	for logicalID, definition := range oldEvaluation.Resources {
		if _, requested := evaluation.Resources[logicalID]; requested {
			continue
		}
		if _, removed := deletedIDs[logicalID]; removed {
			continue
		}
		for index := range oldResources {
			stored := &oldResources[index]
			if stored.LogicalResourceID == logicalID {
				evaluation.Resources[logicalID] = definition

				break
			}
		}
	}

	return evaluation
}

//nolint:wsl // Compensation snapshots are copied before appending one action.
func recordCompensation(compensation *lifecycleCompensation, action *compensationAction) {
	if compensation == nil {
		return
	}
	action.capabilities = append([]string(nil), action.capabilities...)
	action.tags = cloneStringMap(action.tags)
	compensation.actions = append(compensation.actions, *action)
}

//nolint:wsl // Inverse action dispatch deliberately keeps each adapter operation visible.
func (e *Executor) compensate(ctx context.Context, compensation *lifecycleCompensation) error {
	if compensation == nil {
		return nil
	}
	var compensationErr error
	for index := len(compensation.actions) - 1; index >= 0; index-- {
		action := compensation.actions[index]
		var err error
		switch action.kind {
		case compensateCreate:
			err = action.adapter.Delete(context.WithoutCancel(ctx), resource.DeleteRequest{Identifier: action.physicalID})
		case compensateUpdate:
			if action.updater == nil {
				err = fmt.Errorf("resource %q does not support update compensation", action.logicalID)
			} else {
				_, err = action.updater.Update(context.WithoutCancel(ctx), resource.UpdateRequest{
					Identifier:         action.physicalID,
					Properties:         cloneAnyMap(action.definition.Properties),
					PreviousProperties: cloneAnyMap(action.previousDefinition.Properties),
					Capabilities:       append([]string(nil), action.capabilities...),
					Tags:               cloneStringMap(action.tags),
				})
			}
		case compensateDelete:
			var result resource.CreateResult
			result, err = action.adapter.Create(context.WithoutCancel(ctx), resource.CreateRequest{Properties: cloneAnyMap(action.definition.Properties), Capabilities: append([]string(nil), action.capabilities...), Tags: cloneStringMap(action.tags)})
			if err == nil && result.Identifier != action.physicalID {
				if result.Identifier != "" {
					err = errors.Join(fmt.Errorf("resource %q compensation returned identifier %q, want %q", action.logicalID, result.Identifier, action.physicalID), action.adapter.Delete(context.WithoutCancel(ctx), resource.DeleteRequest{Identifier: result.Identifier}))
				} else {
					err = fmt.Errorf("resource %q compensation returned no identifier, want %q", action.logicalID, action.physicalID)
				}
			}
		default:
			err = fmt.Errorf("resource %q has unknown compensation action %q", action.logicalID, action.kind)
		}
		if err != nil {
			compensationErr = errors.Join(compensationErr, fmt.Errorf("compensate resource %q: %w", action.logicalID, err))
		}
	}

	return compensationErr
}

//nolint:wsl // Compensation deliberately attempts every successful update.
func (e *Executor) rollbackUpdated(ctx context.Context, storage lifecycleStorage, stack *Stack, token string, updated []updateRollbackAction) (map[string]StackResource, error) {
	failedResources := make(map[string]StackResource)
	var rollbackErr error
	for index := len(updated) - 1; index >= 0; index-- {
		action := updated[index]
		if !action.sideEffectStarted {
			continue
		}
		err := e.updateResource(ctx, storage, stack, token, action.logicalID, action.oldResource, action.newDefinition, action.oldDefinition, action.adapter, action.capabilities, action.oldAttributes, "ROLLBACK", nil)
		if err == nil {
			continue
		}
		rollbackErr = errors.Join(rollbackErr, err)
		failed := action.oldResource
		failed.ResourceStatus = ResourceStatusUpdateFailed
		failed.Timestamp = time.Now()
		failedResources[action.logicalID] = failed
	}

	return failedResources, rollbackErr
}

func (e *Executor) failDelete(ctx context.Context, storage lifecycleStorage, stack *Stack, token string, resources map[string]StackResource, cause error) error {
	failure := e.commitFailure(ctx, storage, stack, StackStatusDeleteFailed, cause.Error(), stack.EvaluatedState, resources, token)

	return errors.Join(cause, failure)
}

//nolint:gocritic,wsl // Journal lookup returns one immutable restoration action.
func journaledAction(stack *Stack, logicalID, physicalID string, definition template.Resource, adapter resource.Adapter, capabilities []string) resourceAction {
	action := resourceAction{
		logicalID:    logicalID,
		definition:   definition,
		adapter:      adapter,
		physicalID:   physicalID,
		capabilities: append([]string(nil), capabilities...),
	}
	for index := len(stack.Journal) - 1; index >= 0; index-- {
		entry := stack.Journal[index]
		if entry.LogicalResourceID != logicalID || entry.PhysicalResourceID != physicalID && entry.Identifier != physicalID {
			continue
		}
		if entry.Properties != nil {
			action.definition.Properties = cloneAnyMap(entry.Properties)
		}
		if entry.CapabilitiesPresent || len(entry.Capabilities) > 0 {
			action.capabilities = make([]string, len(entry.Capabilities))
			copy(action.capabilities, entry.Capabilities)
		}
		action.attributes = cloneAnyMap(entry.Attributes)

		break
	}

	return action
}

//nolint:wsl // Staging and commit form one export boundary.
func (e *Executor) commitExports(ctx context.Context, storage lifecycleStorage, stack *Stack, evaluation *template.Evaluation) error {
	exports := make([]StackExport, 0)
	for _, output := range evaluation.Outputs {
		if output.ExportName == "" {
			continue
		}
		exports = append(exports, StackExport{Name: output.ExportName, Value: fmt.Sprint(output.Value)})
	}
	slices.SortFunc(exports, func(left, right StackExport) int { return strings.Compare(left.Name, right.Name) })
	stage, err := storage.StageExports(ctx, stack.StackID, exports)
	if err != nil {
		return fmt.Errorf("stage exports: %w", err)
	}
	if err := storage.CommitExports(ctx, stage); err != nil {
		return fmt.Errorf("commit exports: %w", err)
	}

	return nil
}

//nolint:nlreturn,wsl // Export and state commits are one compensatable boundary.
func (e *Executor) commitFinalState(ctx context.Context, storage lifecycleStorage, stack *Stack, status, reason string, evaluation *template.Evaluation, resources map[string]StackResource, token string, metadata ...any) error {
	previousExports := cloneExports(stack.Exports)
	previousImports, err := storage.GetImports(ctx, stack.StackID)
	if err != nil {
		return fmt.Errorf("read previous imports: %w", err)
	}
	if err := e.commitExports(ctx, storage, stack, evaluation); err != nil {
		return err
	}
	imports := []StackImport(nil)
	var lifecycleMetadata *LifecycleMetadata
	for _, value := range metadata {
		switch value := value.(type) {
		case *LifecycleMetadata:
			lifecycleMetadata = value
		case []StackImport:
			imports = value
		}
	}
	if err := e.commitImports(ctx, storage, stack, imports); err != nil {
		compensationErr := e.restoreExports(ctx, storage, stack, previousExports)
		return errors.Join(err, compensationErr)
	}
	if err := e.commitState(ctx, storage, stack, status, reason, evaluation, resources, token, lifecycleMetadata); err != nil {
		compensationErr := e.restoreExports(ctx, storage, stack, previousExports)
		importCompensationErr := e.commitImports(ctx, storage, stack, previousImports)
		return errors.Join(err, compensationErr, importCompensationErr)
	}

	return nil
}

//nolint:wsl,nlreturn // Import staging and commit form one storage transaction.
func (e *Executor) commitImports(ctx context.Context, storage lifecycleStorage, stack *Stack, imports []StackImport) error {
	stage, err := storage.StageImports(ctx, stack.StackID, imports)
	if err != nil {
		return fmt.Errorf("stage imports: %w", err)
	}
	if err := storage.CommitImports(ctx, stage); err != nil {
		return fmt.Errorf("commit imports: %w", err)
	}
	return nil
}

//nolint:wsl // Export compensation has one stage/commit pair.
func (e *Executor) restoreExports(ctx context.Context, storage lifecycleStorage, stack *Stack, exports []StackExport) error {
	stage, err := storage.StageExports(ctx, stack.StackID, exports)
	if err != nil {
		return fmt.Errorf("stage export compensation: %w", err)
	}
	if err := storage.CommitExports(ctx, stage); err != nil {
		return fmt.Errorf("commit export compensation: %w", err)
	}

	return nil
}

func (e *Executor) commitState(ctx context.Context, storage lifecycleStorage, stack *Stack, status, reason string, evaluation *template.Evaluation, resources map[string]StackResource, token string, metadata ...*LifecycleMetadata) error {
	if err := storage.RunStackOperation(ctx, stack.StackID, func(_ context.Context, working *Stack) error {
		working.StackStatus = status
		working.StackStatusReason = reason
		working.EvaluatedState = evaluation
		working.Resources = sortedStackResources(resources)
		if len(metadata) > 0 && metadata[0] != nil {
			applyLifecycleMetadata(working, *metadata[0])
		}
		working.LastUpdatedTime = time.Now()
		if status == StackStatusDeleteComplete {
			working.DeletionTime = time.Now()
		}
		appendEventLocked(working, status, token, StackEvent{EventID: uuid.NewString(), StackID: working.StackID, StackName: working.StackName, ResourceStatus: status, ResourceStatusReason: reason, ClientRequestToken: token, Timestamp: time.Now()})

		return nil
	}); err != nil {
		return fmt.Errorf("commit stack state: %w", err)
	}

	return nil
}

//nolint:wsl // Failure state and event persistence are independent attempts.
func (e *Executor) commitFailure(ctx context.Context, storage lifecycleStorage, stack *Stack, status, reason string, evaluation *template.Evaluation, resources map[string]StackResource, token string, errorCodes ...string) error {
	stateErr := storage.RunStackOperation(ctx, stack.StackID, func(_ context.Context, working *Stack) error {
		working.StackStatus = status
		working.StackStatusReason = reason
		working.EvaluatedState = evaluation
		working.Resources = sortedStackResources(resources)
		working.LastUpdatedTime = time.Now()

		return nil
	})
	event := StackEvent{
		EventID:              uuid.NewString(),
		StackID:              stack.StackID,
		StackName:            stack.StackName,
		ResourceStatus:       status,
		ResourceStatusReason: reason,
		ClientRequestToken:   token,
		Timestamp:            time.Now(),
	}
	if len(errorCodes) > 0 {
		event.ErrorCode = errorCodes[0]
	}
	eventErr := storage.AppendStackEvent(ctx, stack.StackID, event)
	if stateErr != nil {
		stateErr = fmt.Errorf("failure state persistence: %w", stateErr)
	}
	if eventErr != nil {
		eventErr = fmt.Errorf("failure event persistence: %w", eventErr)
	}

	return errors.Join(stateErr, eventErr)
}

//nolint:gocritic,wsl // Side-effect intent and completion are one visible pair.
func (e *Executor) recordSideEffect(ctx context.Context, storage lifecycleStorage, stack *Stack, token, operation, journalStatus, status string, entry StackJournalEntry) error {
	entry.Operation = operation
	entry.Status = journalStatus
	entry.CapabilitiesPresent = true
	if entry.Reason == "" {
		entry.Reason = status
	}
	journalStorage, detailed := storage.(interface {
		AppendStackJournalEntry(context.Context, string, StackJournalEntry) error
	})
	var err error
	if detailed {
		err = journalStorage.AppendStackJournalEntry(ctx, stack.StackID, entry)
	} else {
		err = storage.AppendStackJournal(ctx, stack.StackID, operation, journalStatus, status)
	}
	if err != nil {
		return fmt.Errorf("append side-effect journal: %w", err)
	}

	if err := storage.AppendStackEvent(ctx, stack.StackID, StackEvent{EventID: uuid.NewString(), StackID: stack.StackID, StackName: stack.StackName, LogicalResourceID: entry.LogicalResourceID, PhysicalResourceID: entry.PhysicalResourceID, ResourceType: entry.ResourceType, ResourceStatus: status, ResourceStatusReason: entry.Reason, ClientRequestToken: token, Timestamp: time.Now()}); err != nil {
		return fmt.Errorf("append side-effect event: %w", err)
	}

	return nil
}

func (e *Executor) persistResourceStatus(ctx context.Context, storage lifecycleStorage, stack *Stack, logicalID, typeName, physicalID, stackStatus, resourceStatus string) error {
	if err := storage.RunStackOperation(ctx, stack.StackID, func(_ context.Context, working *Stack) error {
		working.StackStatus = stackStatus
		working.LastUpdatedTime = time.Now()
		for index := range working.Resources {
			if working.Resources[index].LogicalResourceID != logicalID {
				continue
			}
			if physicalID != "" {
				working.Resources[index].PhysicalResourceID = physicalID
			}
			working.Resources[index].ResourceType = typeName
			working.Resources[index].ResourceStatus = resourceStatus

			return nil
		}
		working.Resources = append(working.Resources, StackResource{LogicalResourceID: logicalID, PhysicalResourceID: physicalID, ResourceType: typeName, ResourceStatus: resourceStatus, StackID: working.StackID, StackName: working.StackName, Timestamp: time.Now()})

		return nil
	}); err != nil {
		return fmt.Errorf("persist resource in-progress state: %w", err)
	}

	return nil
}

func sideEffectStatuses(phase string, deleting bool) (string, string, string) {
	if phase == operationUpdate || phase == "ROLLBACK" && !deleting {
		return StackStatusUpdateInProgress, StackStatusUpdateFailed, StackStatusUpdateComplete
	}

	if deleting && phase != operationUpdate {
		return StackStatusDeleteInProgress, StackStatusDeleteFailed, StackStatusDeleteComplete
	}

	return StackStatusCreateInProgress, StackStatusCreateFailed, StackStatusCreateComplete
}

//nolint:wsl // Capability checks are intentionally kept adjacent to registry lookup.
func (e *Executor) preflight(evaluation *template.Evaluation, capabilities, resourceTypes []string) error {
	for logicalID, definition := range evaluation.Resources {
		if len(resourceTypes) > 0 && !slices.Contains(resourceTypes, definition.Type) {
			return fmt.Errorf("resource %q uses disallowed resource type %q", logicalID, definition.Type)
		}
		adapter, found := e.registry.Get(definition.Type)
		if !found {
			return fmt.Errorf("resource %q uses unregistered type %q", logicalID, definition.Type)
		}
		for _, required := range resource.CapabilitiesFor(adapter, definition.Properties) {
			if !capabilityProvided(required, capabilities) {
				return &Error{Code: "InsufficientCapabilities", Message: fmt.Sprintf("resource %q requires %s", logicalID, required)}
			}
		}
	}

	return nil
}

//nolint:wsl // Policy validation is a single traversal of the old evaluated state.
func (e *Executor) validateUpdatePolicies(evaluation, old *template.Evaluation, oldResources map[string]StackResource) error {
	if old == nil {
		return nil
	}
	for logicalID, definition := range old.Resources {
		if _, hasResource := oldResources[logicalID]; !hasResource {
			continue
		}
		next, exists := evaluation.Resources[logicalID]
		if !exists {
			if deletionPolicy(definition, false) == snapshotPolicy {
				return fmt.Errorf("resource %q uses unsupported Snapshot deletion policy", logicalID)
			}

			continue
		}
		if definition.Type != next.Type {
			if deletionPolicy(next, true) == snapshotPolicy {
				return fmt.Errorf("resource %q uses unsupported Snapshot update replacement policy", logicalID)
			}

			continue
		}
		adapter, found := e.registry.Get(definition.Type)
		if !found {
			continue
		}
		changed, replacement := classifyChanges(definition, next, adapter.Schema())
		if changed && replacement && deletionPolicy(next, true) == snapshotPolicy {
			return fmt.Errorf("resource %q uses unsupported Snapshot update replacement policy", logicalID)
		}
	}

	return nil
}

func (e *Executor) validateDeletePolicies(evaluation *template.Evaluation, resources map[string]StackResource) error {
	for logicalID, definition := range evaluation.Resources {
		if _, exists := resources[logicalID]; exists && deletionPolicy(definition, false) == snapshotPolicy {
			return fmt.Errorf("resource %q uses unsupported Snapshot deletion policy", logicalID)
		}
	}

	return nil
}

func (e *Executor) storageForExecution() (lifecycleStorage, error) {
	storage, ok := e.storage.(lifecycleStorage)
	if !ok {
		return nil, errors.New("cloudformation storage does not support lifecycle execution")
	}

	return storage, nil
}

//nolint:gocritic,wsl // Evaluated resources are small immutable value objects.
func classifyChanges(old, next template.Resource, schema resource.Schema) (bool, bool) {
	keys := make(map[string]struct{}, len(old.Properties)+len(next.Properties))
	for property := range old.Properties {
		keys[property] = struct{}{}
	}
	for property := range next.Properties {
		keys[property] = struct{}{}
	}
	changed := false
	replacement := false
	for property := range keys {
		if reflect.DeepEqual(old.Properties[property], next.Properties[property]) {
			continue
		}
		changed = true
		if schema.PropertyUpdate[property] == resource.PropertyUpdate("Replacement") {
			replacement = true
		}
	}

	return changed, replacement
}

//nolint:gocritic,wsl,nlreturn // Property classification intentionally compares complete resource definitions.
func hasNoInterruptionChange(old, next template.Resource, schema resource.Schema) bool {
	keys := make(map[string]struct{}, len(old.Properties)+len(next.Properties))
	for property := range old.Properties {
		keys[property] = struct{}{}
	}
	for property := range next.Properties {
		keys[property] = struct{}{}
	}
	for property := range keys {
		if !reflect.DeepEqual(old.Properties[property], next.Properties[property]) && schema.PropertyUpdate[property] == resource.PropertyUpdate("NoInterruption") {
			return true
		}
	}
	return false
}

//nolint:wsl // Topological sorting is intentionally explicit and deterministic.
func evaluationOrder(evaluation *template.Evaluation) ([]string, error) {
	remaining := make(map[string]map[string]struct{}, len(evaluation.Resources))
	for logicalID, definition := range evaluation.Resources {
		dependencies := append([]string(nil), definition.DependsOn...)
		implicit := append([]string(nil), definition.Dependencies...)
		collectPropertyDependencies(definition.Properties, &implicit, evaluation.Conditions)
		for _, dependency := range implicit {
			if _, exists := evaluation.Resources[dependency]; exists {
				dependencies = append(dependencies, dependency)
			}
		}
		remaining[logicalID] = make(map[string]struct{}, len(dependencies))
		for _, dependency := range dependencies {
			if dependency == logicalID {
				return nil, fmt.Errorf("resource %q depends on itself", logicalID)
			}
			if _, exists := evaluation.Resources[dependency]; !exists {
				return nil, fmt.Errorf("resource %q depends on unresolved resource %q", logicalID, dependency)
			}
			remaining[logicalID][dependency] = struct{}{}
		}
	}
	order := make([]string, 0, len(remaining))
	for len(remaining) > 0 {
		ready := make([]string, 0)
		for logicalID, dependencies := range remaining {
			if len(dependencies) == 0 {
				ready = append(ready, logicalID)
			}
		}
		if len(ready) == 0 {
			return nil, errors.New("resource dependency cycle detected")
		}
		sort.Strings(ready)
		for _, logicalID := range ready {
			order = append(order, logicalID)
			delete(remaining, logicalID)
		}
		for _, dependencies := range remaining {
			for _, logicalID := range ready {
				delete(dependencies, logicalID)
			}
		}
	}

	return order, nil
}

//nolint:gocognit,nestif,wsl // Intrinsic dependency collection traverses one value tree.
func collectPropertyDependencies(value any, dependencies *[]string, conditions map[string]bool) {
	seen := make(map[string]struct{}, len(*dependencies))
	for _, dependency := range *dependencies {
		seen[dependency] = struct{}{}
	}
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if len(value) == 1 {
				if argument, ok := value["Fn::If"]; ok {
					if selected, ok := selectedExecutorIfBranch(argument, conditions); ok {
						visit(selected)

						return
					}
				}
				if name, ok := value["Ref"].(string); ok {
					seen[name] = struct{}{}
				}
				if argument, ok := value["Fn::GetAtt"]; ok {
					if parts, ok := graphGetAttParts(argument); ok {
						seen[parts[0]] = struct{}{}
					}
				}
				if argument, ok := value["Fn::Sub"]; ok {
					collectSubDependencies(argument, seen, conditions)
				}
			}
			for _, child := range value {
				visit(child)
			}
		case []any:
			for _, child := range value {
				visit(child)
			}
		}
	}
	visit(value)
	*dependencies = (*dependencies)[:0]
	for dependency := range seen {
		*dependencies = append(*dependencies, dependency)
	}
}

//nolint:wsl // Fn::Sub has two accepted argument shapes.
func collectSubDependencies(argument any, dependencies map[string]struct{}, conditions map[string]bool) {
	var source string
	variables := map[string]any{}
	switch argument := argument.(type) {
	case string:
		source = argument
	case []any:
		if len(argument) != 2 {
			return
		}
		source, _ = argument[0].(string)
		variables, _ = argument[1].(map[string]any)
		for _, value := range variables {
			implicit := make([]string, 0)
			collectPropertyDependencies(value, &implicit, conditions)
			for _, dependency := range implicit {
				dependencies[dependency] = struct{}{}
			}
		}
	}
	for _, match := range graphSubReferences(source) {
		name := strings.TrimPrefix(match, "!")
		if strings.HasPrefix(match, "!") {
			continue
		}
		if _, ok := variables[name]; ok {
			continue
		}
		dependencies[strings.SplitN(name, ".", 2)[0]] = struct{}{}
	}
}

//nolint:wsl // Branch selection keeps argument validation adjacent to the chosen value.
func selectedExecutorIfBranch(argument any, conditions map[string]bool) (any, bool) {
	arguments, ok := argument.([]any)
	if !ok || len(arguments) != 3 {
		return nil, false
	}
	conditionName, ok := arguments[0].(string)
	if !ok {
		return nil, false
	}
	condition, ok := conditions[conditionName]
	if !ok {
		return nil, false
	}
	if condition {
		return arguments[1], true
	}

	return arguments[2], true
}

//nolint:wsl // Conversion preserves action order while constructing state.
func resourceEntries(stack *Stack, actions []resourceAction) map[string]StackResource {
	resources := make(map[string]StackResource, len(actions))
	for index := range actions {
		action := &actions[index]
		resources[action.logicalID] = stackResource(stack, action.logicalID, action.definition.Type, action.physicalID)
	}

	return resources
}

//nolint:gocritic,wsl // Evaluated resources are small immutable value objects.
func fixedCustomName(old, next template.Resource, schema resource.Schema) bool {
	for property, update := range schema.PropertyUpdate {
		if update != resource.PropertyUpdate("Replacement") || !strings.HasSuffix(property, "Name") {
			continue
		}
		oldName, oldOK := old.Properties[property].(string)
		nextName, nextOK := next.Properties[property].(string)
		if oldOK && nextOK && oldName != "" && oldName == nextName {
			return true
		}
	}

	return false
}

func stackResource(stack *Stack, logicalID, typeName, physicalID string) StackResource {
	return StackResource{LogicalResourceID: logicalID, PhysicalResourceID: physicalID, ResourceType: typeName, ResourceStatus: ResourceStatusCreateComplete, StackID: stack.StackID, StackName: stack.StackName, Timestamp: time.Now()}
}

//nolint:gocritic,wsl,nlreturn // Status projection accepts a value to preserve the stored snapshot.
func updateResourceStatus(stored StackResource, operation string) StackResource {
	if operation == operationUpdate {
		stored.ResourceStatus = ResourceStatusUpdateComplete
		stored.Timestamp = time.Now()
	}
	return stored
}

func stackResourceMap(resources []StackResource) map[string]StackResource {
	result := make(map[string]StackResource, len(resources))
	for _, resource := range resources {
		result[resource.LogicalResourceID] = resource
	}

	return result
}

//nolint:wsl // Sorting is the final deterministic state conversion step.
func sortedStackResources(resources map[string]StackResource) []StackResource {
	logicalIDs := make([]string, 0, len(resources))
	for logicalID := range resources {
		logicalIDs = append(logicalIDs, logicalID)
	}
	sort.Strings(logicalIDs)
	result := make([]StackResource, 0, len(logicalIDs))
	for _, logicalID := range logicalIDs {
		result = append(result, resources[logicalID])
	}

	return result
}

func evaluationFromStack(stack *Stack) *template.Evaluation {
	evaluation := &template.Evaluation{Resources: make(map[string]template.Resource)}
	for _, resource := range stack.Resources {
		evaluation.Resources[resource.LogicalResourceID] = template.Resource{Type: resource.ResourceType}
	}

	return evaluation
}

func pseudoParameters(stack *Stack) map[string]string {
	return map[string]string{
		pseudoAccountIDName: pseudoAccountID,
		pseudoRegionName:    pseudoRegion,
		pseudoStackIDName:   stack.StackID,
		pseudoStackNameName: stack.StackName,
		pseudoURLSuffixName: pseudoURLSuffix,
	}
}

//nolint:wsl // Journal projection intentionally walks current resources before historical attributes.
func resourceAttributesFromStack(stack *Stack) map[string]map[string]string {
	attributes := make(map[string]map[string]string, len(stack.Resources))
	for _, stored := range stack.Resources {
		attributes[stored.LogicalResourceID] = map[string]string{"Ref": stored.PhysicalResourceID}
	}
	for index := len(stack.Journal) - 1; index >= 0; index-- {
		entry := stack.Journal[index]
		if entry.LogicalResourceID == "" || len(entry.Attributes) == 0 {
			continue
		}
		if _, exists := attributes[entry.LogicalResourceID]; !exists {
			attributes[entry.LogicalResourceID] = map[string]string{}
		}
		for name, value := range entry.Attributes {
			if _, exists := attributes[entry.LogicalResourceID][name]; !exists {
				attributes[entry.LogicalResourceID][name] = fmt.Sprint(value)
			}
		}
	}

	return attributes
}

//nolint:wsl,nlreturn // The latest journal entry is selected by reverse traversal.
func latestResourceAttributes(stack *Stack, logicalID string) map[string]string {
	attributes := map[string]string{}
	for index := len(stack.Journal) - 1; index >= 0; index-- {
		entry := stack.Journal[index]
		if entry.LogicalResourceID != logicalID || len(entry.Attributes) == 0 {
			continue
		}
		for name, value := range entry.Attributes {
			attributes[name] = fmt.Sprint(value)
		}
		break
	}

	return attributes
}

//nolint:gocritic,wsl // Evaluation errors and the inactive-resource check are one boundary.
func (e *Executor) evaluateUpdateResource(req *LifecycleRequest, input *template.EvaluationInput, logicalID string, fallback template.Resource) (template.Resource, error) {
	if req.Template == nil || input == nil {
		return fallback, nil
	}
	evaluation, err := req.Template.EvaluateResources(*input, []string{logicalID})
	if err != nil {
		return template.Resource{}, fmt.Errorf("evaluate updated resource %q: %w", logicalID, err)
	}
	definition, ok := evaluation.Resources[logicalID]
	if !ok {
		return template.Resource{}, fmt.Errorf("updated resource %q is inactive", logicalID)
	}

	return definition, nil
}

//nolint:wsl // Reloading and projecting the latest attributes are one compensation boundary.
func refreshUpdateResourceAttributes(ctx context.Context, storage lifecycleStorage, stack *Stack, logicalID, physicalID string, attributes map[string]map[string]string) error {
	updatedStack, err := storage.GetStack(ctx, stack.StackID)
	if err != nil {
		return fmt.Errorf("reload stack after updating resource %q: %w", logicalID, err)
	}
	current := map[string]string{"Ref": physicalID}
	for name, value := range latestResourceAttributes(updatedStack, logicalID) {
		current[name] = value
	}
	attributes[logicalID] = current

	return nil
}

//nolint:wsl,nlreturn // Import lookup combines committed and requested lifecycle edges.
func committedImportValues(ctx context.Context, storage lifecycleStorage, stackRef string, requested ...[]StackImport) map[string]string {
	values := make(map[string]string)
	imports, err := storage.GetImports(ctx, stackRef)
	if err != nil {
		return values
	}
	for _, edges := range requested {
		imports = append(imports, edges...)
	}
	exports, err := storage.ListExports(ctx)
	if err != nil {
		return values
	}
	for _, edge := range imports {
		for _, export := range exports {
			if export.Name == edge.ExportName {
				values[edge.ExportName] = export.Value
				break
			}
		}
	}
	return values
}

//nolint:gocritic // Dependency readiness is a compact graph predicate.
func dependenciesReady(definition template.Resource, attributes map[string]map[string]string) bool {
	dependencies := append(append([]string(nil), definition.DependsOn...), definition.Dependencies...)
	for _, dependency := range dependencies {
		if _, ok := attributes[dependency]; !ok {
			return false
		}
	}

	return true
}

//nolint:gocritic,nlreturn,wsl // Evaluated resources are small immutable value objects.
func deletionPolicy(definition template.Resource, replacement bool) string {
	if replacement && definition.UpdateReplacePolicy != "" {
		return definition.UpdateReplacePolicy
	}
	return definition.DeletionPolicy
}

//nolint:gocritic // ExecutionOptions is the lifecycle policy boundary.
func createRetains(options ExecutionOptions) bool {
	return options.DisableRollback || options.RetainResourcesOnFail || options.RetainResourcesOnFailure || options.OnFailure == FailurePolicyRetain || options.OnFailure == FailurePolicyDoNothing
}

//nolint:gocritic,wsl // ExecutionOptions validation is intentionally fail-closed.
func validateExecutionOptions(options ExecutionOptions) error {
	switch options.OnFailure {
	case "", FailurePolicyRollback, FailurePolicyRetain, FailurePolicyDoNothing, FailurePolicyDelete:
	default:
		return fmt.Errorf("unknown failure policy %q", options.OnFailure)
	}
	if options.DisableRollback && options.OnFailure != "" {
		return errors.New("DisableRollback and OnFailure cannot both be specified")
	}
	switch options.DeletionMode {
	case "", deletionModeStandard, deletionModeForce:
	default:
		return fmt.Errorf("unknown deletion mode %q", options.DeletionMode)
	}
	if options.DeploymentConfig != nil {
		if options.DeploymentConfig.Mode != "" {
			return errors.New("DeploymentConfig.Mode is unsupported")
		}
	}
	if options.RollbackConfiguration != nil {
		return errors.New("RollbackConfiguration is unsupported")
	}

	return nil
}

//nolint:wsl // Metadata validation intentionally checks each unsupported field in order.
func validateLifecycleMetadata(metadata *LifecycleMetadata) error {
	if metadata == nil {
		return nil
	}
	if metadata.RollbackConfiguration != nil {
		return errors.New("RollbackConfiguration is unsupported")
	}
	if metadata.StackPolicyBody != "" || metadata.StackPolicyDuringUpdateBody != "" {
		return errors.New("stack policy bodies are unsupported")
	}
	if metadata.DeploymentConfig != nil && metadata.DeploymentConfig.Mode != "" {
		return errors.New(msgDeploymentModeUnsupported)
	}
	if metadata.DisableValidation {
		return errors.New("DisableValidation is unsupported")
	}
	if metadata.TimeoutInMinutes > 0 {
		return errors.New("TimeoutInMinutes is unsupported")
	}

	return nil
}

//nolint:nlreturn,wsl // Capability aliases are a single predicate.
func capabilityProvided(required string, provided []string) bool {
	if slices.Contains(provided, required) {
		return true
	}
	return required == "CAPABILITY_IAM" && slices.Contains(provided, "CAPABILITY_NAMED_IAM")
}

func executionToken(token string) string {
	if token == "" {
		return uuid.NewString()
	}

	return token
}

//nolint:wsl // Evaluation normalization is intentionally a short validation boundary.
func validateEvaluation(evaluation *template.Evaluation) error {
	if evaluation == nil {
		return errors.New("evaluated template is required")
	}
	if evaluation.Resources == nil {
		evaluation.Resources = make(map[string]template.Resource)
	}

	return nil
}

//nolint:wsl // Tag conversion keeps the nil-preserving map construction together.
func stackTagMap(tags []Tag) map[string]string {
	if tags == nil {
		return nil
	}
	result := make(map[string]string, len(tags))
	for _, tag := range tags {
		result[tag.Key] = tag.Value
	}

	return result
}

func lifecycleMetadataForDelete(options *ExecutionOptions) LifecycleMetadata {
	return LifecycleMetadata{
		RoleARN:               options.RoleARN,
		DeploymentConfig:      cloneDeploymentConfig(options.DeploymentConfig),
		RollbackConfiguration: cloneRollbackConfiguration(options.RollbackConfiguration),
	}
}
