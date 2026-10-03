package resource

import (
	"context"
	"fmt"

	"github.com/sivchari/kumo/internal/service/sns"
)

// snsTopicAdapter implements AWS::SNS::Topic.
// Ref and Fn::GetAtt: https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-sns-topic.html
type snsTopicAdapter struct{}

var snsTopicSchema = Schema{
	TypeName: "AWS::SNS::Topic",
	PropertyUpdate: map[string]PropertyUpdate{
		"DisplayName":     propertyUpdateNoInterruption,
		tagsProperty:      propertyUpdateNoInterruption,
		topicNameProperty: propertyUpdateReplacement,
	},
	Attributes: []string{topicARNAttribute, "TopicName"},
}

func (*snsTopicAdapter) Schema() Schema { return cloneSchema(snsTopicSchema) }

//nolint:cyclop,funlen // Property validation is kept adjacent to provisioning.
func (*snsTopicAdapter) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := validateProperties(snsTopicSchema, req.Properties); err != nil {
		return CreateResult{}, err
	}

	nameValue, provided := req.Properties[topicNameProperty]

	name, ok := nameValue.(string)
	if provided && !ok {
		return CreateResult{}, fmt.Errorf("TopicName must be a string")
	}

	if provided && name == "" {
		return CreateResult{}, fmt.Errorf("TopicName must not be empty")
	}

	if name == "" {
		name = generatedPhysicalName("cfn-sns")
	}

	attributes := make(map[string]string)

	if displayNameValue, provided := req.Properties["DisplayName"]; provided {
		displayName, ok := displayNameValue.(string)
		if !ok {
			return CreateResult{}, fmt.Errorf("DisplayName must be a string")
		}

		attributes["DisplayName"] = displayName
	}

	var tags []sns.Tag

	if tagsValue, provided := req.Properties[tagsProperty]; provided {
		rawTags, ok := tagsValue.([]any)
		if !ok {
			return CreateResult{}, fmt.Errorf("tags must be a list of tags")
		}

		for _, rawTag := range rawTags {
			tag, ok := rawTag.(map[string]any)
			if !ok {
				return CreateResult{}, fmt.Errorf("tags contains an invalid tag")
			}

			key, keyOK := tag["Key"].(string)
			value, valueOK := tag["Value"].(string)

			if !keyOK || !valueOK {
				return CreateResult{}, fmt.Errorf("tags contains an invalid tag")
			}

			tags = append(tags, sns.Tag{Key: key, Value: value})
		}
	}

	storage, err := lookupStorage[sns.Storage]("sns")
	if err != nil {
		return CreateResult{}, fmt.Errorf("create SNS topic: %w", err)
	}

	topic, err := storage.CreateTopic(ctx, name, attributes, tags)
	if err != nil {
		return CreateResult{}, fmt.Errorf("create SNS topic: %w", err)
	}

	return CreateResult{Identifier: topic.ARN, Attributes: map[string]any{topicARNAttribute: topic.ARN, "TopicName": topic.Name}}, nil
}

//nolint:wsl // Topic attributes and tags are updated through separate SNS APIs.
func (*snsTopicAdapter) Update(ctx context.Context, req UpdateRequest) (UpdateResult, error) {
	if err := validateProperties(snsTopicSchema, req.Properties); err != nil {
		return UpdateResult{}, err
	}
	storage, err := lookupStorage[sns.Storage]("sns")
	if err != nil {
		return UpdateResult{}, err
	}
	if value, ok := req.Properties["DisplayName"]; ok {
		name, ok := value.(string)
		if !ok {
			return UpdateResult{}, fmt.Errorf("DisplayName must be a string")
		}
		if err := storage.SetTopicAttribute(ctx, req.Identifier, "DisplayName", name); err != nil {
			return UpdateResult{}, fmt.Errorf("update SNS topic display name: %w", err)
		}
	}
	tags, err := topicTags(req.Properties[tagsProperty])
	if err != nil {
		return UpdateResult{}, err
	}
	if err := storage.TagResource(ctx, req.Identifier, tags); err != nil {
		return UpdateResult{}, fmt.Errorf("update SNS topic tags: %w", err)
	}
	oldTags, err := topicTags(req.PreviousProperties[tagsProperty])
	if err != nil {
		return UpdateResult{}, err
	}
	current := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		current[tag.Key] = struct{}{}
	}
	removed := make([]string, 0)
	for _, tag := range oldTags {
		if _, ok := current[tag.Key]; !ok {
			removed = append(removed, tag.Key)
		}
	}
	if len(removed) > 0 {
		if err := storage.UntagResource(ctx, req.Identifier, removed); err != nil {
			return UpdateResult{}, fmt.Errorf("remove SNS topic tags: %w", err)
		}
	}

	return UpdateResult{}, nil
}

//nolint:wsl // Tag conversion validates and appends each API tag together.
func topicTags(value any) ([]sns.Tag, error) {
	if value == nil {
		return nil, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("tags must be a list of tags")
	}
	result := make([]sns.Tag, 0, len(values))
	for _, raw := range values {
		tag, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tags contains an invalid tag")
		}
		key, keyOK := tag["Key"].(string)
		value, valueOK := tag["Value"].(string)
		if !keyOK || !valueOK {
			return nil, fmt.Errorf("tags contains an invalid tag")
		}
		result = append(result, sns.Tag{Key: key, Value: value})
	}

	return result, nil
}

func (*snsTopicAdapter) Delete(ctx context.Context, req DeleteRequest) error {
	storage, err := lookupStorage[sns.Storage]("sns")
	if err != nil {
		return err
	}

	if err := storage.DeleteTopic(ctx, req.Identifier); err != nil {
		return fmt.Errorf("delete SNS topic: %w", err)
	}

	return nil
}
