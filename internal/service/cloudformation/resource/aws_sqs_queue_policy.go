//nolint:wsl // CloudFormation property translation is intentionally explicit.
package resource

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sivchari/kumo/internal/service/sqs"
)

// sqsQueuePolicyAdapter implements AWS::SQS::QueuePolicy.
type sqsQueuePolicyAdapter struct{}

var sqsQueuePolicySchema = Schema{
	TypeName: "AWS::SQS::QueuePolicy",
	PropertyUpdate: map[string]PropertyUpdate{
		"PolicyDocument": propertyUpdateNoInterruption,
		"Queues":         propertyUpdateReplacement,
	},
	Attributes: []string{"Id"},
}

func (*sqsQueuePolicyAdapter) Schema() Schema { return cloneSchema(sqsQueuePolicySchema) }

func (*sqsQueuePolicyAdapter) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := validateProperties(sqsQueuePolicySchema, req.Properties); err != nil {
		return CreateResult{}, err
	}
	policy, err := jsonDocument("PolicyDocument", req.Properties["PolicyDocument"])
	if err != nil {
		return CreateResult{}, err
	}
	queues, err := stringListProperty(req.Properties, "Queues")
	if err != nil {
		return CreateResult{}, err
	}
	if len(queues) == 0 {
		return CreateResult{}, fmt.Errorf("queues must contain at least one queue")
	}

	storage, err := lookupStorage[sqs.Storage]("sqs")
	if err != nil {
		return CreateResult{}, err
	}
	identifier, err := createSQSQueuePolicy(ctx, storage, queues, policy)
	if err != nil {
		return CreateResult{}, err
	}

	return CreateResult{Identifier: identifier, Attributes: map[string]any{"Id": identifier}}, nil
}

func (*sqsQueuePolicyAdapter) Update(ctx context.Context, req UpdateRequest) (UpdateResult, error) {
	if err := validateProperties(sqsQueuePolicySchema, req.Properties); err != nil {
		return UpdateResult{}, err
	}
	policy, err := jsonDocument("PolicyDocument", req.Properties["PolicyDocument"])
	if err != nil {
		return UpdateResult{}, err
	}
	queues, err := stringListProperty(req.Properties, "Queues")
	if err != nil {
		return UpdateResult{}, err
	}
	oldQueues, err := stringListProperty(req.PreviousProperties, "Queues")
	if err != nil {
		return UpdateResult{}, err
	}
	storage, err := lookupStorage[sqs.Storage]("sqs")
	if err != nil {
		return UpdateResult{}, err
	}
	for _, queue := range queues {
		if err := storage.SetQueueAttributes(ctx, queue, map[string]string{policyProperty: policy}); err != nil {
			return UpdateResult{}, fmt.Errorf("update SQS queue policy: %w", err)
		}
	}
	current := make(map[string]struct{}, len(queues))
	for _, queue := range queues {
		current[queue] = struct{}{}
	}
	for _, queue := range oldQueues {
		if _, exists := current[queue]; !exists {
			if err := storage.SetQueueAttributes(ctx, queue, map[string]string{policyProperty: ""}); err != nil {
				return UpdateResult{}, fmt.Errorf("clear removed SQS queue policy: %w", err)
			}
		}
	}

	return UpdateResult{}, nil
}

func (*sqsQueuePolicyAdapter) Delete(ctx context.Context, req DeleteRequest) error {
	var queues []string
	if err := json.Unmarshal([]byte(req.Identifier), &queues); err != nil || len(queues) == 0 {
		return fmt.Errorf("invalid SQS queue policy identifier")
	}
	storage, err := lookupStorage[sqs.Storage]("sqs")
	if err != nil {
		return err
	}
	for _, queueURL := range queues {
		if err := storage.SetQueueAttributes(ctx, queueURL, map[string]string{policyProperty: ""}); err != nil {
			return fmt.Errorf("clear SQS queue policy: %w", err)
		}
	}

	return nil
}
