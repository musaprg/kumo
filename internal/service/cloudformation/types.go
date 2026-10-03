// Package cloudformation provides CloudFormation service emulation for kumo.
package cloudformation

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"time"

	"github.com/sivchari/kumo/internal/service/cloudformation/template"
)

const (
	cfnXMLNS                  = "http://cloudformation.amazonaws.com/doc/2010-05-15/"
	revisionConflictException = "RevisionConflictException"
)

// Stack status constants.
const (
	StackStatusCreateInProgress = "CREATE_IN_PROGRESS"
	StackStatusCreateComplete   = "CREATE_COMPLETE"
	StackStatusCreateFailed     = "CREATE_FAILED"
	StackStatusReviewInProgress = "REVIEW_IN_PROGRESS"
	StackStatusUpdateInProgress = "UPDATE_IN_PROGRESS"
	StackStatusUpdateComplete   = "UPDATE_COMPLETE"
	StackStatusUpdateFailed     = "UPDATE_FAILED"
	StackStatusDeleteInProgress = "DELETE_IN_PROGRESS"
	StackStatusDeleteComplete   = "DELETE_COMPLETE"
	StackStatusDeleteFailed     = "DELETE_FAILED"
)

// Change set status constants.
const (
	changeSetTypeCreate = operationCreate

	ChangeSetStatusCreatePending    = "CREATE_PENDING"
	ChangeSetStatusCreateInProgress = "CREATE_IN_PROGRESS"
	ChangeSetStatusCreateComplete   = "CREATE_COMPLETE"
	ChangeSetStatusFailed           = "FAILED"
	ChangeSetStatusObsolete         = "OBSOLETE"
)

// Change set execution status constants.
const (
	ChangeSetExecutionAvailable   = "AVAILABLE"
	ChangeSetExecutionUnavailable = "UNAVAILABLE"
	ChangeSetExecutionInProgress  = "EXECUTE_IN_PROGRESS"
	ChangeSetExecutionComplete    = "EXECUTE_COMPLETE"
)

// Change set resource action constants.
const (
	ChangeActionAdd    = "Add"
	ChangeActionModify = "Modify"
	ChangeActionRemove = "Remove"
)

// Resource replacement classification constants.
const (
	ReplacementTrue        = "True"
	ReplacementFalse       = "False"
	ReplacementConditional = "Conditional"
)

// Resource status constants.
const (
	ResourceStatusCreateInProgress = "CREATE_IN_PROGRESS"
	ResourceStatusCreateComplete   = "CREATE_COMPLETE"
	ResourceStatusCreateFailed     = "CREATE_FAILED"
	ResourceStatusUpdateInProgress = "UPDATE_IN_PROGRESS"
	ResourceStatusUpdateComplete   = "UPDATE_COMPLETE"
	ResourceStatusUpdateFailed     = "UPDATE_FAILED"
	ResourceStatusDeleteInProgress = "DELETE_IN_PROGRESS"
	ResourceStatusDeleteComplete   = "DELETE_COMPLETE"
	ResourceStatusDeleteFailed     = "DELETE_FAILED"
)

// Stack represents a CloudFormation stack.
type Stack struct {
	StackID                     string
	StackName                   string
	TemplateBody                string
	Parameters                  map[string]string
	StackStatus                 string
	StackStatusReason           string
	CreationTime                time.Time
	LastUpdatedTime             time.Time
	DeletionTime                time.Time
	Resources                   []StackResource
	Revision                    uint64
	EvaluatedState              *template.Evaluation
	Journal                     []StackJournalEntry
	Events                      []StackEvent
	OperationLock               *StackLock
	Executions                  map[string]ExecutionRecord
	Exports                     []StackExport
	Imports                     []StackImport
	Tags                        []Tag
	RoleARN                     string
	ResourceTypes               []string
	NotificationARNs            []string
	RollbackConfiguration       *RollbackConfiguration
	DeploymentConfig            *DeploymentConfig
	StackPolicyBody             string
	StackPolicyDuringUpdateBody string
	EnableTerminationProtection bool
	RetainExceptOnCreate        bool
	TimeoutInMinutes            int32
	DisableValidation           bool
	DisableRollback             bool
	OnFailure                   FailurePolicy
}

// ChangeSet is an immutable review of a proposed stack lifecycle operation.
type ChangeSet struct {
	ID              string
	Name            string
	StackID         string
	StackName       string
	Description     string
	ChangeSetType   string
	Status          string
	StatusReason    string
	ExecutionStatus string
	CreationTime    time.Time
	ClientToken     string
	Fingerprint     string
	BaseRevision    uint64
	TemplateBody    string
	Parameters      map[string]string
	Imports         []StackImport
	Evaluation      *template.Evaluation
	ResourceChanges []ResourceChange
	Options         ExecutionOptions
	Metadata        *LifecycleMetadata
}

// ResourceChange describes one resource delta in a change set.
type ResourceChange struct {
	Action             string
	LogicalResourceID  string
	PhysicalResourceID string
	ResourceType       string
	Replacement        string
}

// StackJournalEntry records a durable stack operation transition.
type StackJournalEntry struct {
	Revision            uint64
	Operation           string
	Status              string
	Reason              string
	LogicalResourceID   string
	PhysicalResourceID  string
	ResourceType        string
	Identifier          string
	Properties          map[string]any
	Capabilities        []string
	CapabilitiesPresent bool
	Attributes          map[string]any
	Timestamp           time.Time
}

// StackLock records the persisted state of a stack operation lock.
type StackLock struct {
	Token          string
	Operation      string
	StartedAt      time.Time
	ExecutionToken string
}

// ExecutionRecord stores the durable outcome of one client-request token.
type ExecutionRecord struct {
	Operation   string
	Fingerprint string
	Completed   bool
	Success     bool
	ErrorCode   string
	Error       string
	StartedAt   time.Time
	CompletedAt time.Time
}

// CreateExecutionRecord stores the durable outcome of one CreateStack request.
type CreateExecutionRecord struct {
	Fingerprint string

	StackName string
	StackID   string

	Completed bool
	Success   bool
	ErrorCode string
	Error     string

	StartedAt   time.Time
	CompletedAt time.Time
}

// StackExport is a committed CloudFormation export.
type StackExport struct {
	Name             string
	Value            string
	ExporterStackARN string
}

// StackImport is a committed edge from an importer to an export.
type StackImport struct {
	ExportName       string
	ExporterStackARN string
	ImporterStackARN string
}

// ExportStage is an immutable, revision-bound export replacement.
type ExportStage struct {
	ID           string
	StackARN     string
	BaseRevision uint64
	Exports      []StackExport
}

// ImportStage is an immutable, revision-bound import replacement.
type ImportStage struct {
	ID           string
	StackARN     string
	BaseRevision uint64
	Imports      []StackImport
}

// StackResource represents a resource in a stack.
type StackResource struct {
	LogicalResourceID  string
	PhysicalResourceID string
	ResourceType       string
	ResourceStatus     string
	Timestamp          time.Time
	StackID            string
	StackName          string
}

// TemplateValidationResult represents the result of template validation.
type TemplateValidationResult struct {
	Parameters   []TemplateParameter
	Description  string
	Capabilities []string
}

// TemplateSummaryResult represents the structural summary of a template.
type TemplateSummaryResult struct {
	Parameters    []TemplateParameter
	Description   string
	Capabilities  []string
	ResourceTypes []string
}

// TemplateParameter represents a parameter defined in a template.
type TemplateParameter struct {
	ParameterKey          string
	DefaultValue          string
	NoEcho                bool
	Description           string
	ConstraintDescription string
	ParameterType         string
	AllowedValues         []string
	AllowedPattern        string
	MinLength             *int
	MaxLength             *int
	MinValue              *float64
	MaxValue              *float64
}

// Parameter represents a parameter supplied to a stack operation.
type Parameter struct {
	ParameterKey     string `json:"ParameterKey"`
	ParameterValue   string `json:"ParameterValue,omitempty"`
	UsePreviousValue bool   `json:"UsePreviousValue,omitempty"`
}

// Tag represents a tag supplied to a stack operation.
type Tag struct {
	Key   string `json:"Key"`
	Value string `json:"Value"`
}

// RollbackTrigger represents a CloudWatch alarm monitored during a stack operation.
type RollbackTrigger struct {
	Arn  string `json:"Arn"`
	Type string `json:"Type"`
}

// RollbackConfiguration represents rollback monitoring settings.
type RollbackConfiguration struct {
	RollbackTriggers        []RollbackTrigger `json:"RollbackTriggers,omitempty"`
	MonitoringTimeInMinutes int32             `json:"MonitoringTimeInMinutes,omitempty"`
}

// DeploymentConfig represents deployment behavior for a stack operation.
type DeploymentConfig struct {
	Mode            string `json:"Mode,omitempty"`
	DisableRollback bool   `json:"DisableRollback,omitempty"`
}

// LifecycleMetadata contains stack metadata committed with a lifecycle result.
type LifecycleMetadata struct {
	TemplateBody                string
	Parameters                  map[string]string
	Tags                        []Tag
	RoleARN                     string
	ResourceTypes               []string
	NotificationARNs            []string
	RollbackConfiguration       *RollbackConfiguration
	DeploymentConfig            *DeploymentConfig
	StackPolicyBody             string
	StackPolicyDuringUpdateBody string
	EnableTerminationProtection bool
	RetainExceptOnCreate        bool
	TimeoutInMinutes            int32
	DisableValidation           bool
	DisableRollback             bool
	OnFailure                   FailurePolicy
}

// Request types.

// CreateStackRequest represents a CreateStack request.
type CreateStackRequest struct {
	StackName                   string                 `json:"StackName"`
	TemplateBody                string                 `json:"TemplateBody,omitempty"`
	TemplateURL                 string                 `json:"TemplateURL,omitempty"`
	Parameters                  map[string]string      `json:"Parameters,omitempty"`
	Capabilities                []string               `json:"Capabilities,omitempty"`
	ClientRequestToken          string                 `json:"ClientRequestToken,omitempty"`
	DeploymentConfig            *DeploymentConfig      `json:"DeploymentConfig,omitempty"`
	DisableRollback             bool                   `json:"DisableRollback,omitempty"`
	DisableValidation           bool                   `json:"DisableValidation,omitempty"`
	EnableTerminationProtection bool                   `json:"EnableTerminationProtection,omitempty"`
	NotificationARNs            []string               `json:"NotificationARNs,omitempty"`
	OnFailure                   string                 `json:"OnFailure,omitempty"`
	ResourceTypes               []string               `json:"ResourceTypes,omitempty"`
	RetainExceptOnCreate        bool                   `json:"RetainExceptOnCreate,omitempty"`
	RoleARN                     string                 `json:"RoleARN,omitempty"`
	RollbackConfiguration       *RollbackConfiguration `json:"RollbackConfiguration,omitempty"`
	StackPolicyBody             string                 `json:"StackPolicyBody,omitempty"`
	StackPolicyURL              string                 `json:"StackPolicyURL,omitempty"`
	Tags                        []Tag                  `json:"Tags,omitempty"`
	TimeoutInMinutes            int32                  `json:"TimeoutInMinutes,omitempty"`
	parameters                  []Parameter
}

// UnmarshalJSON accepts both map-shaped internal requests and AWS Query
// parameter member lists.
//
//nolint:goconst // Wire field names intentionally match CloudFormation's protocol.
func (r *CreateStackRequest) UnmarshalJSON(data []byte) error {
	if err := rejectUnsupportedRequestFields(data, map[string]struct{}{
		"StackName": {}, "TemplateBody": {}, "TemplateURL": {}, "Parameters": {}, "Capabilities": {}, "ClientRequestToken": {},
		"DeploymentConfig": {}, "DisableRollback": {}, "DisableValidation": {}, "EnableTerminationProtection": {}, "NotificationARNs": {},
		"OnFailure": {}, "ResourceTypes": {}, "RetainExceptOnCreate": {}, "RoleARN": {}, "RollbackConfiguration": {}, "StackPolicyBody": {},
		"StackPolicyURL": {}, "Tags": {}, "TimeoutInMinutes": {},
	}); err != nil {
		return err
	}

	return unmarshalStackRequest(data, func(fields stackRequestFields, values map[string]string, parameters []Parameter) {
		r.StackName = fields.StackName
		r.TemplateBody = fields.TemplateBody
		r.TemplateURL = fields.TemplateURL
		r.Parameters = values
		r.parameters = parameters
		r.Capabilities = fields.Capabilities
		r.ClientRequestToken = fields.ClientRequestToken
		r.DeploymentConfig = fields.DeploymentConfig
		r.DisableRollback = fields.DisableRollback
		r.DisableValidation = fields.DisableValidation
		r.EnableTerminationProtection = fields.EnableTerminationProtection
		r.NotificationARNs = fields.NotificationARNs
		r.OnFailure = fields.OnFailure
		r.ResourceTypes = fields.ResourceTypes
		r.RetainExceptOnCreate = fields.RetainExceptOnCreate
		r.RoleARN = fields.RoleARN
		r.RollbackConfiguration = fields.RollbackConfiguration
		r.StackPolicyBody = fields.StackPolicyBody
		r.StackPolicyURL = fields.StackPolicyURL
		r.Tags = fields.Tags
		r.TimeoutInMinutes = fields.TimeoutInMinutes
	})
}

// DeleteStackRequest represents a DeleteStack request.
type DeleteStackRequest struct {
	StackName          string            `json:"StackName"`
	ClientRequestToken string            `json:"ClientRequestToken,omitempty"`
	DeletionMode       string            `json:"DeletionMode,omitempty"`
	DeploymentConfig   *DeploymentConfig `json:"DeploymentConfig,omitempty"`
	RetainResources    []string          `json:"RetainResources,omitempty"`
	RoleARN            string            `json:"RoleARN,omitempty"`
}

// DescribeStacksRequest represents a DescribeStacks request.
type DescribeStacksRequest struct {
	StackName string `json:"StackName,omitempty"`
	NextToken string `json:"NextToken,omitempty"`
}

// ListStacksRequest represents a ListStacks request.
type ListStacksRequest struct {
	StackStatusFilter []string `json:"StackStatusFilter,omitempty"`
	NextToken         string   `json:"NextToken,omitempty"`
}

// UpdateStackRequest represents an UpdateStack request.
type UpdateStackRequest struct {
	StackName                   string                 `json:"StackName"`
	TemplateBody                string                 `json:"TemplateBody,omitempty"`
	TemplateURL                 string                 `json:"TemplateURL,omitempty"`
	UsePreviousTemplate         bool                   `json:"UsePreviousTemplate,omitempty"`
	Parameters                  map[string]string      `json:"Parameters,omitempty"`
	Capabilities                []string               `json:"Capabilities,omitempty"`
	ClientRequestToken          string                 `json:"ClientRequestToken,omitempty"`
	DeploymentConfig            *DeploymentConfig      `json:"DeploymentConfig,omitempty"`
	DisableRollback             bool                   `json:"DisableRollback,omitempty"`
	DisableValidation           bool                   `json:"DisableValidation,omitempty"`
	NotificationARNs            []string               `json:"NotificationARNs,omitempty"`
	ResourceTypes               []string               `json:"ResourceTypes,omitempty"`
	RetainExceptOnCreate        bool                   `json:"RetainExceptOnCreate,omitempty"`
	RoleARN                     string                 `json:"RoleARN,omitempty"`
	RollbackConfiguration       *RollbackConfiguration `json:"RollbackConfiguration,omitempty"`
	StackPolicyBody             string                 `json:"StackPolicyBody,omitempty"`
	StackPolicyDuringUpdateBody string                 `json:"StackPolicyDuringUpdateBody,omitempty"`
	StackPolicyDuringUpdateURL  string                 `json:"StackPolicyDuringUpdateURL,omitempty"`
	StackPolicyURL              string                 `json:"StackPolicyURL,omitempty"`
	Tags                        []Tag                  `json:"Tags,omitempty"`
	parameters                  []Parameter
}

// CreateChangeSetRequest represents a CreateChangeSet request.
type CreateChangeSetRequest struct {
	StackName           string            `json:"StackName"`
	ChangeSetName       string            `json:"ChangeSetName"`
	ChangeSetType       string            `json:"ChangeSetType,omitempty"`
	ClientToken         string            `json:"ClientToken,omitempty"`
	Description         string            `json:"Description,omitempty"`
	TemplateBody        string            `json:"TemplateBody,omitempty"`
	TemplateURL         string            `json:"TemplateURL,omitempty"`
	UsePreviousTemplate bool              `json:"UsePreviousTemplate,omitempty"`
	Parameters          map[string]string `json:"Parameters,omitempty"`
	Capabilities        []string          `json:"Capabilities,omitempty"`
	IncludeNestedStacks bool              `json:"IncludeNestedStacks,omitempty"`
	NotificationARNs    []string          `json:"NotificationARNs,omitempty"`
	ResourceTypes       []string          `json:"ResourceTypes,omitempty"`
	RoleARN             string            `json:"RoleARN,omitempty"`
	Tags                []Tag             `json:"Tags,omitempty"`
	parameters          []Parameter
	imports             []StackImport
	baseRevision        uint64
	baseRevisionSet     bool
	preflightError      string
}

// UnmarshalJSON accepts both map-shaped internal requests and AWS Query member lists.
//
//nolint:funlen,wsl // Change-set wire decoding keeps supported fields explicit.
func (r *CreateChangeSetRequest) UnmarshalJSON(data []byte) error {
	if err := rejectUnsupportedRequestFields(data, map[string]struct{}{
		"StackName": {}, "ChangeSetName": {}, "ChangeSetType": {}, "ClientToken": {}, "Description": {}, "TemplateBody": {}, "TemplateURL": {},
		"UsePreviousTemplate": {}, "Parameters": {}, "Capabilities": {}, "IncludeNestedStacks": {}, "NotificationARNs": {}, "ResourceTypes": {}, "RoleARN": {}, "Tags": {},
	}); err != nil {
		return err
	}
	var raw struct {
		StackName           string          `json:"StackName"`
		ChangeSetName       string          `json:"ChangeSetName"`
		ChangeSetType       string          `json:"ChangeSetType"`
		ClientToken         string          `json:"ClientToken"`
		Description         string          `json:"Description"`
		TemplateBody        string          `json:"TemplateBody"`
		TemplateURL         string          `json:"TemplateURL"`
		UsePreviousTemplate bool            `json:"UsePreviousTemplate"`
		Parameters          json.RawMessage `json:"Parameters"`
		Capabilities        []string        `json:"Capabilities"`
		IncludeNestedStacks bool            `json:"IncludeNestedStacks"`
		NotificationARNs    []string        `json:"NotificationARNs"`
		ResourceTypes       []string        `json:"ResourceTypes"`
		RoleARN             string          `json:"RoleARN"`
		Tags                []Tag           `json:"Tags"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("unmarshal change set request: %w", err)
	}
	r.StackName = raw.StackName
	r.ChangeSetName = raw.ChangeSetName
	r.ChangeSetType = raw.ChangeSetType
	r.ClientToken = raw.ClientToken
	r.Description = raw.Description
	r.TemplateBody = raw.TemplateBody
	r.TemplateURL = raw.TemplateURL
	r.UsePreviousTemplate = raw.UsePreviousTemplate
	r.Capabilities = raw.Capabilities
	r.IncludeNestedStacks = raw.IncludeNestedStacks
	r.NotificationARNs = raw.NotificationARNs
	r.ResourceTypes = raw.ResourceTypes
	r.RoleARN = raw.RoleARN
	r.Tags = raw.Tags
	if len(bytes.TrimSpace(raw.Parameters)) == 0 || bytes.Equal(bytes.TrimSpace(raw.Parameters), []byte("null")) {
		return nil
	}
	if bytes.HasPrefix(bytes.TrimSpace(raw.Parameters), []byte("[")) {
		if err := json.Unmarshal(raw.Parameters, &r.parameters); err != nil {
			return fmt.Errorf("unmarshal change set parameters: %w", err)
		}
		r.Parameters = make(map[string]string, len(r.parameters))
		for _, parameter := range r.parameters {
			if parameter.ParameterKey != "" && !parameter.UsePreviousValue {
				r.Parameters[parameter.ParameterKey] = parameter.ParameterValue
			}
		}

		return nil
	}
	if err := json.Unmarshal(raw.Parameters, &r.Parameters); err != nil {
		return fmt.Errorf("unmarshal change set parameter map: %w", err)
	}

	return nil
}

// ExecuteChangeSetRequest represents an ExecuteChangeSet request.
type ExecuteChangeSetRequest struct {
	ChangeSetName        string `json:"ChangeSetName"`
	StackName            string `json:"StackName,omitempty"`
	ClientRequestToken   string `json:"ClientRequestToken,omitempty"`
	DisableRollback      bool   `json:"DisableRollback,omitempty"`
	RetainExceptOnCreate bool   `json:"RetainExceptOnCreate,omitempty"`
	disableRollbackSet   bool
}

// UnmarshalJSON preserves whether DisableRollback was explicitly supplied.
//
//nolint:wsl // Request normalization keeps presence tracking beside field assignment.
func (r *ExecuteChangeSetRequest) UnmarshalJSON(data []byte) error {
	var raw struct {
		ChangeSetName        string `json:"ChangeSetName"`
		StackName            string `json:"StackName,omitempty"`
		ClientRequestToken   string `json:"ClientRequestToken,omitempty"`
		DisableRollback      *bool  `json:"DisableRollback"`
		RetainExceptOnCreate bool   `json:"RetainExceptOnCreate,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("unmarshal execute change set request: %w", err)
	}
	r.ChangeSetName = raw.ChangeSetName
	r.StackName = raw.StackName
	r.ClientRequestToken = raw.ClientRequestToken
	r.RetainExceptOnCreate = raw.RetainExceptOnCreate
	if raw.DisableRollback != nil {
		r.DisableRollback = *raw.DisableRollback
		r.disableRollbackSet = true
	}

	return nil
}

// DescribeChangeSetRequest represents a DescribeChangeSet request.
type DescribeChangeSetRequest struct {
	ChangeSetName string `json:"ChangeSetName"`
	StackName     string `json:"StackName,omitempty"`
	NextToken     string `json:"NextToken,omitempty"`
}

// GetTemplateSummaryRequest represents a GetTemplateSummary request.
type GetTemplateSummaryRequest struct {
	StackName    string `json:"StackName,omitempty"`
	StackSetName string `json:"StackSetName,omitempty"`
	TemplateBody string `json:"TemplateBody,omitempty"`
	TemplateURL  string `json:"TemplateURL,omitempty"`
}

// UnmarshalJSON accepts both map-shaped internal requests and AWS Query
// parameter member lists.
func (r *UpdateStackRequest) UnmarshalJSON(data []byte) error {
	if err := rejectUnsupportedRequestFields(data, map[string]struct{}{
		"StackName": {}, "TemplateBody": {}, "TemplateURL": {}, "UsePreviousTemplate": {}, "Parameters": {}, "Capabilities": {}, "ClientRequestToken": {},
		"DeploymentConfig": {}, "DisableRollback": {}, "DisableValidation": {}, "NotificationARNs": {}, "ResourceTypes": {}, "RetainExceptOnCreate": {},
		"RoleARN": {}, "RollbackConfiguration": {}, "StackPolicyBody": {}, "StackPolicyDuringUpdateBody": {}, "StackPolicyDuringUpdateURL": {},
		"StackPolicyURL": {}, "Tags": {},
	}); err != nil {
		return err
	}

	return unmarshalStackRequest(data, func(fields stackRequestFields, values map[string]string, parameters []Parameter) {
		r.StackName = fields.StackName
		r.TemplateBody = fields.TemplateBody
		r.TemplateURL = fields.TemplateURL
		r.UsePreviousTemplate = fields.UsePreviousTemplate
		r.Parameters = values
		r.parameters = parameters
		r.Capabilities = fields.Capabilities
		r.ClientRequestToken = fields.ClientRequestToken
		r.DeploymentConfig = fields.DeploymentConfig
		r.DisableRollback = fields.DisableRollback
		r.DisableValidation = fields.DisableValidation
		r.NotificationARNs = fields.NotificationARNs
		r.ResourceTypes = fields.ResourceTypes
		r.RetainExceptOnCreate = fields.RetainExceptOnCreate
		r.RoleARN = fields.RoleARN
		r.RollbackConfiguration = fields.RollbackConfiguration
		r.StackPolicyBody = fields.StackPolicyBody
		r.StackPolicyDuringUpdateBody = fields.StackPolicyDuringUpdateBody
		r.StackPolicyDuringUpdateURL = fields.StackPolicyDuringUpdateURL
		r.StackPolicyURL = fields.StackPolicyURL
		r.Tags = fields.Tags
	})
}

//nolint:wsl // Field validation keeps decoding and rejection in one boundary.
func rejectUnsupportedRequestFields(data []byte, allowed map[string]struct{}) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("unmarshal stack request: %w", err)
	}
	for field := range raw {
		if _, ok := allowed[field]; !ok {
			return fmt.Errorf("unsupported stack request field %q", field)
		}
	}

	return nil
}

type stackRequestFields struct {
	StackName                   string                 `json:"StackName"`
	TemplateBody                string                 `json:"TemplateBody,omitempty"`
	TemplateURL                 string                 `json:"TemplateURL,omitempty"`
	UsePreviousTemplate         bool                   `json:"UsePreviousTemplate,omitempty"`
	Capabilities                []string               `json:"Capabilities,omitempty"`
	ClientRequestToken          string                 `json:"ClientRequestToken,omitempty"`
	DeploymentConfig            *DeploymentConfig      `json:"DeploymentConfig,omitempty"`
	DisableRollback             bool                   `json:"DisableRollback,omitempty"`
	DisableValidation           bool                   `json:"DisableValidation,omitempty"`
	EnableTerminationProtection bool                   `json:"EnableTerminationProtection,omitempty"`
	NotificationARNs            []string               `json:"NotificationARNs,omitempty"`
	OnFailure                   string                 `json:"OnFailure,omitempty"`
	ResourceTypes               []string               `json:"ResourceTypes,omitempty"`
	RetainExceptOnCreate        bool                   `json:"RetainExceptOnCreate,omitempty"`
	RoleARN                     string                 `json:"RoleARN,omitempty"`
	RollbackConfiguration       *RollbackConfiguration `json:"RollbackConfiguration,omitempty"`
	StackPolicyBody             string                 `json:"StackPolicyBody,omitempty"`
	StackPolicyDuringUpdateBody string                 `json:"StackPolicyDuringUpdateBody,omitempty"`
	StackPolicyDuringUpdateURL  string                 `json:"StackPolicyDuringUpdateURL,omitempty"`
	StackPolicyURL              string                 `json:"StackPolicyURL,omitempty"`
	Tags                        []Tag                  `json:"Tags,omitempty"`
	TimeoutInMinutes            int32                  `json:"TimeoutInMinutes,omitempty"`
}

//nolint:wsl // Request normalization keeps map and list forms together.
func unmarshalStackRequest(data []byte, assign func(stackRequestFields, map[string]string, []Parameter)) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("unmarshal stack request: %w", err)
	}
	parameterData := raw["Parameters"]
	delete(raw, "Parameters")

	clean, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal stack request fields: %w", err)
	}

	var fields stackRequestFields
	if err := json.Unmarshal(clean, &fields); err != nil {
		return fmt.Errorf("unmarshal stack request fields: %w", err)
	}
	if len(bytes.TrimSpace(parameterData)) == 0 || bytes.Equal(bytes.TrimSpace(parameterData), []byte("null")) {
		assign(fields, nil, nil)

		return nil
	}
	if bytes.HasPrefix(bytes.TrimSpace(parameterData), []byte("[")) {
		var parameters []Parameter
		if err := json.Unmarshal(parameterData, &parameters); err != nil {
			return fmt.Errorf("unmarshal stack request parameters: %w", err)
		}
		values := make(map[string]string, len(parameters))
		for _, parameter := range parameters {
			if parameter.ParameterKey != "" && !parameter.UsePreviousValue {
				values[parameter.ParameterKey] = parameter.ParameterValue
			}
		}
		assign(fields, values, parameters)

		return nil
	}

	var values map[string]string
	if err := json.Unmarshal(parameterData, &values); err != nil {
		return fmt.Errorf("unmarshal stack request parameter map: %w", err)
	}
	assign(fields, values, nil)

	return nil
}

// DescribeStackResourcesRequest represents a DescribeStackResources request.
type DescribeStackResourcesRequest struct {
	StackName          string `json:"StackName,omitempty"`
	LogicalResourceID  string `json:"LogicalResourceId,omitempty"`
	PhysicalResourceID string `json:"PhysicalResourceId,omitempty"`
}

// DescribeStackEventsRequest represents a DescribeStackEvents request.
type DescribeStackEventsRequest struct {
	StackName string `json:"StackName"`
	NextToken string `json:"NextToken,omitempty"`
}

// DescribeStackResourceRequest represents a DescribeStackResource request.
type DescribeStackResourceRequest struct {
	StackName         string `json:"StackName"`
	LogicalResourceID string `json:"LogicalResourceId"`
}

// ListExportsRequest represents a ListExports request.
type ListExportsRequest struct {
	NextToken string `json:"NextToken,omitempty"`
}

// GetTemplateRequest represents a GetTemplate request.
type GetTemplateRequest struct {
	ChangeSetName string `json:"ChangeSetName,omitempty"`
	StackName     string `json:"StackName,omitempty"`
	TemplateStage string `json:"TemplateStage,omitempty"`
}

// ValidateTemplateRequest represents a ValidateTemplate request.
type ValidateTemplateRequest struct {
	TemplateBody string `json:"TemplateBody,omitempty"`
	TemplateURL  string `json:"TemplateURL,omitempty"`
}

// XML Response types.

// XMLCreateStackResponse is the XML response for CreateStack.
type XMLCreateStackResponse struct {
	XMLName          xml.Name             `xml:"CreateStackResponse"`
	Xmlns            string               `xml:"xmlns,attr"`
	Result           XMLCreateStackResult `xml:"CreateStackResult"`
	ResponseMetadata XMLResponseMetadata  `xml:"ResponseMetadata"`
}

// XMLCreateStackResult contains the result of CreateStack.
type XMLCreateStackResult struct {
	StackID string `xml:"StackId"`
}

// XMLDeleteStackResponse is the XML response for DeleteStack.
type XMLDeleteStackResponse struct {
	XMLName          xml.Name             `xml:"DeleteStackResponse"`
	Xmlns            string               `xml:"xmlns,attr"`
	Result           XMLDeleteStackResult `xml:"DeleteStackResult"`
	ResponseMetadata XMLResponseMetadata  `xml:"ResponseMetadata"`
}

// XMLDeleteStackResult is an empty result for DeleteStack.
type XMLDeleteStackResult struct{}

// XMLDescribeStacksResponse is the XML response for DescribeStacks.
type XMLDescribeStacksResponse struct {
	XMLName          xml.Name                `xml:"DescribeStacksResponse"`
	Xmlns            string                  `xml:"xmlns,attr"`
	Result           XMLDescribeStacksResult `xml:"DescribeStacksResult"`
	ResponseMetadata XMLResponseMetadata     `xml:"ResponseMetadata"`
}

// XMLDescribeStacksResult contains the result of DescribeStacks.
type XMLDescribeStacksResult struct {
	Stacks XMLStacks `xml:"Stacks"`
}

// XMLStacks contains a list of stacks.
type XMLStacks struct {
	Members []XMLStack `xml:"member"`
}

// XMLStack represents a stack in XML format.
type XMLStack struct {
	StackID                     string                    `xml:"StackId"`
	StackName                   string                    `xml:"StackName"`
	StackStatus                 string                    `xml:"StackStatus"`
	StackStatusReason           string                    `xml:"StackStatusReason,omitempty"`
	CreationTime                string                    `xml:"CreationTime"`
	LastUpdatedTime             string                    `xml:"LastUpdatedTime,omitempty"`
	DeletionTime                string                    `xml:"DeletionTime,omitempty"`
	Parameters                  XMLParameters             `xml:"Parameters,omitempty"`
	RollbackConfiguration       *XMLRollbackConfiguration `xml:"RollbackConfiguration,omitempty"`
	DisableRollback             bool                      `xml:"DisableRollback,omitempty"`
	NotificationARNs            XMLNotificationARNs       `xml:"NotificationARNs,omitempty"`
	TimeoutInMinutes            int32                     `xml:"TimeoutInMinutes,omitempty"`
	RoleARN                     string                    `xml:"RoleARN,omitempty"`
	Tags                        XMLTags                   `xml:"Tags,omitempty"`
	EnableTerminationProtection bool                      `xml:"EnableTerminationProtection,omitempty"`
	RetainExceptOnCreate        bool                      `xml:"RetainExceptOnCreate,omitempty"`
	DeploymentConfig            *XMLDeploymentConfig      `xml:"DeploymentConfig,omitempty"`
	Outputs                     XMLOutputs                `xml:"Outputs,omitempty"`
}

// XMLOutputs contains evaluated stack outputs.
type XMLOutputs struct {
	Members []XMLOutput `xml:"member"`
}

// XMLOutput represents one evaluated stack output.
type XMLOutput struct {
	OutputKey   string `xml:"OutputKey"`
	OutputValue string `xml:"OutputValue"`
	ExportName  string `xml:"ExportName,omitempty"`
}

// XMLParameters contains a list of parameters.
type XMLParameters struct {
	Members []XMLParameter `xml:"member"`
}

// XMLParameter represents a parameter in XML format.
type XMLParameter struct {
	ParameterKey   string `xml:"ParameterKey"`
	ParameterValue string `xml:"ParameterValue"`
}

// XMLTags contains stack tags.
type XMLTags struct {
	Members []XMLTag `xml:"member"`
}

// XMLTag represents a stack tag in XML format.
type XMLTag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// XMLNotificationARNs contains stack notification ARNs.
type XMLNotificationARNs struct {
	Members []string `xml:"member"`
}

// XMLRollbackConfiguration represents persisted rollback settings.
type XMLRollbackConfiguration struct {
	RollbackTriggers        XMLRollbackTriggers `xml:"RollbackTriggers,omitempty"`
	MonitoringTimeInMinutes int32               `xml:"MonitoringTimeInMinutes,omitempty"`
}

// XMLRollbackTriggers contains rollback triggers.
type XMLRollbackTriggers struct {
	Members []XMLRollbackTrigger `xml:"member"`
}

// XMLRollbackTrigger represents a rollback trigger in XML format.
type XMLRollbackTrigger struct {
	Arn  string `xml:"Arn"`
	Type string `xml:"Type"`
}

// XMLDeploymentConfig represents supported deployment settings.
type XMLDeploymentConfig struct {
	Mode            string `xml:"Mode,omitempty"`
	DisableRollback bool   `xml:"DisableRollback,omitempty"`
}

// XMLListStacksResponse is the XML response for ListStacks.
type XMLListStacksResponse struct {
	XMLName          xml.Name            `xml:"ListStacksResponse"`
	Xmlns            string              `xml:"xmlns,attr"`
	Result           XMLListStacksResult `xml:"ListStacksResult"`
	ResponseMetadata XMLResponseMetadata `xml:"ResponseMetadata"`
}

// XMLListStacksResult contains the result of ListStacks.
type XMLListStacksResult struct {
	StackSummaries XMLStackSummaries `xml:"StackSummaries"`
	NextToken      string            `xml:"NextToken,omitempty"`
}

// XMLStackSummaries contains a list of stack summaries.
type XMLStackSummaries struct {
	Members []XMLStackSummary `xml:"member"`
}

// XMLStackSummary represents a stack summary in XML format.
type XMLStackSummary struct {
	StackID           string `xml:"StackId"`
	StackName         string `xml:"StackName"`
	StackStatus       string `xml:"StackStatus"`
	StackStatusReason string `xml:"StackStatusReason,omitempty"`
	CreationTime      string `xml:"CreationTime"`
	LastUpdatedTime   string `xml:"LastUpdatedTime,omitempty"`
	DeletionTime      string `xml:"DeletionTime,omitempty"`
}

// XMLUpdateStackResponse is the XML response for UpdateStack.
type XMLUpdateStackResponse struct {
	XMLName          xml.Name             `xml:"UpdateStackResponse"`
	Xmlns            string               `xml:"xmlns,attr"`
	Result           XMLUpdateStackResult `xml:"UpdateStackResult"`
	ResponseMetadata XMLResponseMetadata  `xml:"ResponseMetadata"`
}

// XMLUpdateStackResult contains the result of UpdateStack.
type XMLUpdateStackResult struct {
	StackID string `xml:"StackId"`
}

// XMLDescribeStackResourcesResponse is the XML response for DescribeStackResources.
type XMLDescribeStackResourcesResponse struct {
	XMLName          xml.Name                        `xml:"DescribeStackResourcesResponse"`
	Xmlns            string                          `xml:"xmlns,attr"`
	Result           XMLDescribeStackResourcesResult `xml:"DescribeStackResourcesResult"`
	ResponseMetadata XMLResponseMetadata             `xml:"ResponseMetadata"`
}

// XMLDescribeStackResourcesResult contains the result of DescribeStackResources.
type XMLDescribeStackResourcesResult struct {
	StackResources XMLStackResources `xml:"StackResources"`
}

// XMLStackResources contains a list of stack resources.
type XMLStackResources struct {
	Members []XMLStackResource `xml:"member"`
}

// XMLStackResource represents a stack resource in XML format.
type XMLStackResource struct {
	LogicalResourceID  string `xml:"LogicalResourceId"`
	PhysicalResourceID string `xml:"PhysicalResourceId"`
	ResourceType       string `xml:"ResourceType"`
	ResourceStatus     string `xml:"ResourceStatus"`
	Timestamp          string `xml:"Timestamp"`
	StackID            string `xml:"StackId"`
	StackName          string `xml:"StackName"`
}

// XMLDescribeStackEventsResponse is the XML response for DescribeStackEvents.
type XMLDescribeStackEventsResponse struct {
	XMLName          xml.Name                     `xml:"DescribeStackEventsResponse"`
	Xmlns            string                       `xml:"xmlns,attr"`
	Result           XMLDescribeStackEventsResult `xml:"DescribeStackEventsResult"`
	ResponseMetadata XMLResponseMetadata          `xml:"ResponseMetadata"`
}

// XMLDescribeStackEventsResult contains the DescribeStackEvents result.
type XMLDescribeStackEventsResult struct {
	StackEvents XMLStackEvents `xml:"StackEvents"`
}

// XMLStackEvents contains a list of stack events.
type XMLStackEvents struct {
	Members []XMLStackEvent `xml:"member"`
}

// XMLStackEvent represents a stack event in XML format.
type XMLStackEvent struct {
	EventID              string `xml:"EventId"`
	StackID              string `xml:"StackId"`
	StackName            string `xml:"StackName"`
	LogicalResourceID    string `xml:"LogicalResourceId,omitempty"`
	PhysicalResourceID   string `xml:"PhysicalResourceId,omitempty"`
	ResourceType         string `xml:"ResourceType,omitempty"`
	Timestamp            string `xml:"Timestamp"`
	ResourceStatus       string `xml:"ResourceStatus"`
	ResourceStatusReason string `xml:"ResourceStatusReason,omitempty"`
	ClientRequestToken   string `xml:"ClientRequestToken,omitempty"`
}

// XMLDescribeStackResourceResponse is the XML response for DescribeStackResource.
type XMLDescribeStackResourceResponse struct {
	XMLName          xml.Name                       `xml:"DescribeStackResourceResponse"`
	Xmlns            string                         `xml:"xmlns,attr"`
	Result           XMLDescribeStackResourceResult `xml:"DescribeStackResourceResult"`
	ResponseMetadata XMLResponseMetadata            `xml:"ResponseMetadata"`
}

// XMLDescribeStackResourceResult contains the DescribeStackResource result.
type XMLDescribeStackResourceResult struct {
	StackResourceDetail XMLStackResourceDetail `xml:"StackResourceDetail"`
}

// XMLStackResourceDetail represents detailed resource information in XML format.
type XMLStackResourceDetail struct {
	LogicalResourceID    string `xml:"LogicalResourceId"`
	PhysicalResourceID   string `xml:"PhysicalResourceId,omitempty"`
	ResourceType         string `xml:"ResourceType"`
	ResourceStatus       string `xml:"ResourceStatus"`
	ResourceStatusReason string `xml:"ResourceStatusReason,omitempty"`
	LastUpdatedTimestamp string `xml:"LastUpdatedTimestamp"`
	StackID              string `xml:"StackId"`
	StackName            string `xml:"StackName"`
}

// XMLListExportsResponse is the XML response for ListExports.
type XMLListExportsResponse struct {
	XMLName          xml.Name             `xml:"ListExportsResponse"`
	Xmlns            string               `xml:"xmlns,attr"`
	Result           XMLListExportsResult `xml:"ListExportsResult"`
	ResponseMetadata XMLResponseMetadata  `xml:"ResponseMetadata"`
}

// XMLListExportsResult contains the ListExports result.
type XMLListExportsResult struct {
	Exports XMLExports `xml:"Exports"`
}

// XMLCreateChangeSetResponse is the XML response for CreateChangeSet.
type XMLCreateChangeSetResponse struct {
	XMLName          xml.Name                 `xml:"CreateChangeSetResponse"`
	Xmlns            string                   `xml:"xmlns,attr"`
	Result           XMLCreateChangeSetResult `xml:"CreateChangeSetResult"`
	ResponseMetadata XMLResponseMetadata      `xml:"ResponseMetadata"`
}

// XMLCreateChangeSetResult contains the created change-set identifiers.
type XMLCreateChangeSetResult struct {
	ID      string `xml:"Id"`
	StackID string `xml:"StackId"`
}

// XMLDescribeChangeSetResponse is the XML response for DescribeChangeSet.
type XMLDescribeChangeSetResponse struct {
	XMLName          xml.Name                   `xml:"DescribeChangeSetResponse"`
	Xmlns            string                     `xml:"xmlns,attr"`
	Result           XMLDescribeChangeSetResult `xml:"DescribeChangeSetResult"`
	ResponseMetadata XMLResponseMetadata        `xml:"ResponseMetadata"`
}

// XMLDescribeChangeSetResult contains a change-set review.
type XMLDescribeChangeSetResult struct {
	ChangeSetName   string             `xml:"ChangeSetName"`
	ChangeSetID     string             `xml:"ChangeSetId"`
	StackID         string             `xml:"StackId"`
	StackName       string             `xml:"StackName"`
	Description     string             `xml:"Description,omitempty"`
	ChangeSetType   string             `xml:"ChangeSetType"`
	Status          string             `xml:"Status"`
	StatusReason    string             `xml:"StatusReason,omitempty"`
	ExecutionStatus string             `xml:"ExecutionStatus"`
	CreationTime    string             `xml:"CreationTime"`
	Parameters      XMLParameters      `xml:"Parameters,omitempty"`
	ResourceChanges XMLResourceChanges `xml:"Changes,omitempty"`
	NextToken       string             `xml:"NextToken,omitempty"`
}

// XMLResourceChanges contains change-set resource changes.
type XMLResourceChanges struct {
	Members []XMLResourceChangeMember `xml:"member"`
}

// XMLResourceChangeMember wraps a resource change in Query XML.
type XMLResourceChangeMember struct {
	ResourceChange XMLResourceChange `xml:"ResourceChange"`
}

// XMLResourceChange represents one resource change in XML format.
type XMLResourceChange struct {
	Action             string `xml:"Action"`
	LogicalResourceID  string `xml:"LogicalResourceId"`
	PhysicalResourceID string `xml:"PhysicalResourceId,omitempty"`
	ResourceType       string `xml:"ResourceType"`
	Replacement        string `xml:"Replacement,omitempty"`
}

// XMLExecuteChangeSetResponse is the XML response for ExecuteChangeSet.
type XMLExecuteChangeSetResponse struct {
	XMLName          xml.Name                  `xml:"ExecuteChangeSetResponse"`
	Xmlns            string                    `xml:"xmlns,attr"`
	Result           XMLExecuteChangeSetResult `xml:"ExecuteChangeSetResult"`
	ResponseMetadata XMLResponseMetadata       `xml:"ResponseMetadata"`
}

// XMLExecuteChangeSetResult is an empty ExecuteChangeSet result.
type XMLExecuteChangeSetResult struct{}

// XMLGetTemplateSummaryResponse is the XML response for GetTemplateSummary.
type XMLGetTemplateSummaryResponse struct {
	XMLName          xml.Name                    `xml:"GetTemplateSummaryResponse"`
	Xmlns            string                      `xml:"xmlns,attr"`
	Result           XMLGetTemplateSummaryResult `xml:"GetTemplateSummaryResult"`
	ResponseMetadata XMLResponseMetadata         `xml:"ResponseMetadata"`
}

// XMLGetTemplateSummaryResult contains template summary fields.
type XMLGetTemplateSummaryResult struct {
	Parameters    XMLTemplateParameters `xml:"Parameters,omitempty"`
	Description   string                `xml:"Description,omitempty"`
	Capabilities  XMLCapabilities       `xml:"Capabilities,omitempty"`
	ResourceTypes XMLStringMembers      `xml:"ResourceTypes,omitempty"`
}

// XMLStringMembers contains a list of strings.
type XMLStringMembers struct {
	Members []string `xml:"member"`
}

// XMLExports contains a list of exports.
type XMLExports struct {
	Members []XMLExport `xml:"member"`
}

// XMLExport represents an export in XML format.
type XMLExport struct {
	ExportingStackID string `xml:"ExportingStackId"`
	Name             string `xml:"Name"`
	Value            string `xml:"Value"`
}

// XMLGetTemplateResponse is the XML response for GetTemplate.
type XMLGetTemplateResponse struct {
	XMLName          xml.Name             `xml:"GetTemplateResponse"`
	Xmlns            string               `xml:"xmlns,attr"`
	Result           XMLGetTemplateResult `xml:"GetTemplateResult"`
	ResponseMetadata XMLResponseMetadata  `xml:"ResponseMetadata"`
}

// XMLGetTemplateResult contains the result of GetTemplate.
type XMLGetTemplateResult struct {
	TemplateBody string `xml:"TemplateBody"`
}

// XMLValidateTemplateResponse is the XML response for ValidateTemplate.
type XMLValidateTemplateResponse struct {
	XMLName          xml.Name                  `xml:"ValidateTemplateResponse"`
	Xmlns            string                    `xml:"xmlns,attr"`
	Result           XMLValidateTemplateResult `xml:"ValidateTemplateResult"`
	ResponseMetadata XMLResponseMetadata       `xml:"ResponseMetadata"`
}

// XMLValidateTemplateResult contains the result of ValidateTemplate.
type XMLValidateTemplateResult struct {
	Parameters   XMLTemplateParameters `xml:"Parameters,omitempty"`
	Description  string                `xml:"Description,omitempty"`
	Capabilities XMLCapabilities       `xml:"Capabilities,omitempty"`
}

// XMLTemplateParameters contains a list of template parameters.
type XMLTemplateParameters struct {
	Members []XMLTemplateParameter `xml:"member"`
}

// XMLTemplateParameter represents a template parameter in XML format.
type XMLTemplateParameter struct {
	ParameterKey          string           `xml:"ParameterKey"`
	DefaultValue          string           `xml:"DefaultValue,omitempty"`
	NoEcho                bool             `xml:"NoEcho"`
	Description           string           `xml:"Description,omitempty"`
	ConstraintDescription string           `xml:"ConstraintDescription,omitempty"`
	ParameterType         string           `xml:"ParameterType,omitempty"`
	AllowedValues         XMLStringMembers `xml:"AllowedValues,omitempty"`
	AllowedPattern        string           `xml:"AllowedPattern,omitempty"`
	MinLength             *int             `xml:"MinLength,omitempty"`
	MaxLength             *int             `xml:"MaxLength,omitempty"`
	MinValue              *float64         `xml:"MinValue,omitempty"`
	MaxValue              *float64         `xml:"MaxValue,omitempty"`
}

// XMLCapabilities contains a list of capabilities.
type XMLCapabilities struct {
	Members []string `xml:"member"`
}

// XMLResponseMetadata contains response metadata.
type XMLResponseMetadata struct {
	RequestID string `xml:"RequestId"`
}

// XMLErrorResponse is the XML error response.
type XMLErrorResponse struct {
	XMLName   xml.Name `xml:"ErrorResponse"`
	Xmlns     string   `xml:"xmlns,attr"`
	Error     XMLError `xml:"Error"`
	RequestID string   `xml:"RequestId"`
}

// XMLError represents an error in XML format.
type XMLError struct {
	Type    string `xml:"Type"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// Error represents a CloudFormation error.
type Error struct {
	Code    string
	Message string
}

// Error implements the error interface.
func (e *Error) Error() string {
	return e.Code + ": " + e.Message
}
