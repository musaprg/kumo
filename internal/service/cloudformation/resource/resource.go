// Package resource adapts CloudFormation resource types to kumo services.
package resource

import (
	"context"
	"strconv"

	"github.com/google/uuid"
)

func generatedPhysicalName(prefix string) string {
	return prefix + "-" + uuid.NewString()
}

// PropertyUpdate describes whether a property supports updates. Resource
// execution is added separately; adapters currently use this schema to reject
// undeclared properties before provisioning.
type PropertyUpdate string

const (
	propertyUpdateNoInterruption   PropertyUpdate = "NoInterruption"
	propertyUpdateReplacement      PropertyUpdate = "Replacement"
	propertyUpdateSomeInterruption PropertyUpdate = "SomeInterruption"
	attributeARN                                  = "Arn"
	attributeDefinitionsProperty                  = "AttributeDefinitions"
	attributeTypeProperty                         = "AttributeType"
	billingModeProperty                           = "BillingMode"
	dynamoDBTableTypeName                         = "AWS::DynamoDB::Table"
	hashKeyType                                   = "HASH"
	keySchemaProperty                             = "KeySchema"
	policyProperty                                = "Policy"
	queueNameProperty                             = "QueueName"
	sqsRedrivePolicyProperty                      = "RedrivePolicy"
	tableNameProperty                             = "TableName"
	tagsProperty                                  = "Tags"
	topicARNAttribute                             = "TopicArn"
	topicNameProperty                             = "TopicName"
)

// Schema declares an adapter's CloudFormation resource contract.
type Schema struct {
	TypeName             string
	PropertyUpdate       map[string]PropertyUpdate
	Attributes           []string
	RequiredCapabilities []string
}

func cloneSchema(schema Schema) Schema {
	propertyUpdate := make(map[string]PropertyUpdate, len(schema.PropertyUpdate))
	for property, update := range schema.PropertyUpdate {
		propertyUpdate[property] = update
	}

	return Schema{
		TypeName:             schema.TypeName,
		PropertyUpdate:       propertyUpdate,
		Attributes:           append([]string(nil), schema.Attributes...),
		RequiredCapabilities: append([]string(nil), schema.RequiredCapabilities...),
	}
}

func strictIntegerString(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}

	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
	}

	integer, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}

	return integer, true
}

// CreateRequest supplies evaluated CloudFormation properties to an adapter.
type CreateRequest struct {
	Properties   map[string]any
	Capabilities []string
	Tags         map[string]string
}

// CreateResult identifies a provisioned resource and exposes its declared
// CloudFormation attributes.
type CreateResult struct {
	Identifier string
	Attributes map[string]any
}

// UpdateRequest supplies an existing resource identifier and the complete
// evaluated property set for an in-place update.
type UpdateRequest struct {
	Identifier         string
	Properties         map[string]any
	PreviousProperties map[string]any
	Capabilities       []string
	Tags               map[string]string
}

// UpdateResult exposes refreshed CloudFormation attributes after an update.
type UpdateResult struct {
	Attributes map[string]any
}

// DeleteRequest identifies the resource an adapter must remove.
type DeleteRequest struct {
	Identifier string
}

// Adapter provisions and removes one CloudFormation resource type.
type Adapter interface {
	Schema() Schema
	Create(context.Context, CreateRequest) (CreateResult, error)
	Delete(context.Context, DeleteRequest) error
}

// UpdateCapable is implemented by adapters that can mutate backend state
// without replacing the physical resource.
type UpdateCapable interface {
	Update(context.Context, UpdateRequest) (UpdateResult, error)
}

// CapabilityProvider optionally exposes capabilities required for evaluated
// resource properties. Keeping this separate from Adapter preserves the
// contract for adapters that do not need capabilities.
type CapabilityProvider interface {
	RequiredCapabilities(map[string]any) []string
}

// CapabilitiesFor returns the capabilities required by an adapter and its
// evaluated properties.
func CapabilitiesFor(adapter Adapter, properties map[string]any) []string {
	if provider, ok := adapter.(CapabilityProvider); ok {
		return append([]string(nil), provider.RequiredCapabilities(properties)...)
	}

	return append([]string(nil), adapter.Schema().RequiredCapabilities...)
}
