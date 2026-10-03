//nolint:goconst,wsl // CloudFormation property translation is intentionally explicit.
package resource

import (
	"context"
	"fmt"
	"strconv"

	"github.com/sivchari/kumo/internal/service/sns"
)

// snsSubscriptionAdapter implements AWS::SNS::Subscription.
type snsSubscriptionAdapter struct{}

var snsSubscriptionSchema = Schema{
	TypeName: "AWS::SNS::Subscription",
	PropertyUpdate: map[string]PropertyUpdate{
		"DeliveryPolicy":      propertyUpdateNoInterruption,
		"Endpoint":            propertyUpdateReplacement,
		"FilterPolicy":        propertyUpdateNoInterruption,
		"FilterPolicyScope":   propertyUpdateNoInterruption,
		"Protocol":            propertyUpdateReplacement,
		"RawMessageDelivery":  propertyUpdateNoInterruption,
		"RedrivePolicy":       propertyUpdateNoInterruption,
		"Region":              propertyUpdateReplacement,
		"ReplayPolicy":        propertyUpdateNoInterruption,
		"SubscriptionRoleArn": propertyUpdateNoInterruption,
		"TopicArn":            propertyUpdateReplacement,
	},
	Attributes: []string{"Arn"},
}

func (*snsSubscriptionAdapter) Schema() Schema { return cloneSchema(snsSubscriptionSchema) }

func (*snsSubscriptionAdapter) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := validateProperties(snsSubscriptionSchema, req.Properties); err != nil {
		return CreateResult{}, err
	}
	topicARN, err := requiredString(req.Properties, "TopicArn")
	if err != nil {
		return CreateResult{}, err
	}
	protocol, err := requiredString(req.Properties, "Protocol")
	if err != nil {
		return CreateResult{}, err
	}
	endpoint, err := optionalString(req.Properties, "Endpoint")
	if err != nil {
		return CreateResult{}, err
	}
	attributes := make(map[string]string)
	for _, property := range []string{"DeliveryPolicy", "FilterPolicy", "RedrivePolicy", "ReplayPolicy"} {
		if value, ok := req.Properties[property]; ok {
			encoded, encodeErr := jsonDocument(property, value)
			if encodeErr != nil {
				return CreateResult{}, encodeErr
			}
			attributes[property] = encoded
		}
	}
	for _, property := range []string{"FilterPolicyScope", "Region", "SubscriptionRoleArn"} {
		if value, ok := req.Properties[property]; ok {
			text, textOK := value.(string)
			if !textOK {
				return CreateResult{}, fmt.Errorf("%s must be a string", property)
			}
			attributes[property] = text
		}
	}
	if value, ok := req.Properties["RawMessageDelivery"]; ok {
		raw, rawOK := value.(bool)
		if !rawOK {
			return CreateResult{}, fmt.Errorf("RawMessageDelivery must be a boolean")
		}
		attributes["RawMessageDelivery"] = strconv.FormatBool(raw)
	}

	storage, err := lookupStorage[sns.Storage]("sns")
	if err != nil {
		return CreateResult{}, err
	}
	subscription, err := storage.Subscribe(ctx, topicARN, protocol, endpoint, attributes)
	if err != nil {
		return CreateResult{}, fmt.Errorf("create SNS subscription: %w", err)
	}

	return CreateResult{Identifier: subscription.ARN, Attributes: map[string]any{"Arn": subscription.ARN}}, nil
}

func (*snsSubscriptionAdapter) Update(ctx context.Context, req UpdateRequest) (UpdateResult, error) {
	if err := validateProperties(snsSubscriptionSchema, req.Properties); err != nil {
		return UpdateResult{}, err
	}
	storage, err := lookupStorage[sns.Storage]("sns")
	if err != nil {
		return UpdateResult{}, err
	}
	attributes, err := snsSubscriptionAttributes(req.Properties)
	if err != nil {
		return UpdateResult{}, err
	}
	for name, value := range attributes {
		if err := storage.SetSubscriptionAttribute(ctx, req.Identifier, name, value); err != nil {
			return UpdateResult{}, fmt.Errorf("update SNS subscription %s: %w", name, err)
		}
	}

	return UpdateResult{}, nil
}

func snsSubscriptionAttributes(properties map[string]any) (map[string]string, error) {
	attributes := map[string]string{
		"DeliveryPolicy":      "",
		"FilterPolicy":        "",
		"FilterPolicyScope":   "",
		"RawMessageDelivery":  "false",
		"RedrivePolicy":       "",
		"ReplayPolicy":        "",
		"SubscriptionRoleArn": "",
	}
	for _, property := range []string{"DeliveryPolicy", "FilterPolicy", "RedrivePolicy", "ReplayPolicy"} {
		if value, ok := properties[property]; ok {
			encoded, err := jsonDocument(property, value)
			if err != nil {
				return nil, err
			}
			attributes[property] = encoded
		}
	}
	for _, property := range []string{"FilterPolicyScope", "SubscriptionRoleArn"} {
		if value, ok := properties[property]; ok {
			text, valid := value.(string)
			if !valid {
				return nil, fmt.Errorf("%s must be a string", property)
			}
			attributes[property] = text
		}
	}
	if value, ok := properties["RawMessageDelivery"]; ok {
		raw, valid := value.(bool)
		if !valid {
			return nil, fmt.Errorf("RawMessageDelivery must be a boolean")
		}
		attributes["RawMessageDelivery"] = strconv.FormatBool(raw)
	}

	return attributes, nil
}

func (*snsSubscriptionAdapter) Delete(ctx context.Context, req DeleteRequest) error {
	storage, err := lookupStorage[sns.Storage]("sns")
	if err != nil {
		return err
	}
	if err := storage.Unsubscribe(ctx, req.Identifier); err != nil {
		return fmt.Errorf("delete SNS subscription: %w", err)
	}

	return nil
}
