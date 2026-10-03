package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sivchari/kumo/internal/service/s3"
)

// s3BucketAdapter implements AWS::S3::Bucket.
// Ref and Fn::GetAtt: https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-s3-bucket.html
type s3BucketAdapter struct{}

var s3BucketSchema = Schema{
	TypeName: "AWS::S3::Bucket",
	PropertyUpdate: map[string]PropertyUpdate{
		"BucketName":           propertyUpdateReplacement,
		"WebsiteConfiguration": propertyUpdateNoInterruption,
	},
	Attributes: []string{attributeARN, "DomainName", "DualStackDomainName", "RegionalDomainName", "WebsiteURL"},
}

func (*s3BucketAdapter) Schema() Schema { return cloneSchema(s3BucketSchema) }

func (*s3BucketAdapter) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := validateProperties(s3BucketSchema, req.Properties); err != nil {
		return CreateResult{}, err
	}

	nameValue, provided := req.Properties["BucketName"]

	name, ok := nameValue.(string)
	if provided && !ok {
		return CreateResult{}, fmt.Errorf("BucketName must be a string")
	}

	if provided && name == "" {
		return CreateResult{}, fmt.Errorf("BucketName must not be empty")
	}

	if name == "" {
		name = generatedPhysicalName("cfn-s3")
	}

	websiteConfiguration, err := decodeS3WebsiteConfigurationProperty(req.Properties)
	if err != nil {
		return CreateResult{}, err
	}

	storage, err := lookupStorage[s3.Storage]("s3")
	if err != nil {
		return CreateResult{}, err
	}

	websiteURL, err := createS3Bucket(ctx, storage, name, websiteConfiguration)
	if err != nil {
		return CreateResult{}, err
	}

	return CreateResult{Identifier: name, Attributes: map[string]any{
		attributeARN:          "arn:aws:s3:::" + name,
		"DomainName":          name + ".s3.amazonaws.com",
		"DualStackDomainName": name + ".s3.dualstack.us-east-1.amazonaws.com",
		"RegionalDomainName":  name + ".s3.us-east-1.amazonaws.com",
		"WebsiteURL":          websiteURL,
	}}, nil
}

//nolint:wsl // S3 website updates validate, decode, and apply one property set.
func (*s3BucketAdapter) Update(ctx context.Context, req UpdateRequest) (UpdateResult, error) {
	if err := validateProperties(s3BucketSchema, req.Properties); err != nil {
		return UpdateResult{}, err
	}
	storage, err := lookupStorage[s3.Storage]("s3")
	if err != nil {
		return UpdateResult{}, err
	}
	configuration, err := decodeS3WebsiteConfigurationProperty(req.Properties)
	if err != nil {
		return UpdateResult{}, err
	}
	if configuration == nil {
		if err := storage.DeleteBucketWebsite(ctx, req.Identifier); err != nil {
			return UpdateResult{}, fmt.Errorf("delete S3 bucket website: %w", err)
		}
	} else if err := storage.PutBucketWebsite(ctx, req.Identifier, configuration); err != nil {
		return UpdateResult{}, fmt.Errorf("update S3 bucket website: %w", err)
	}

	return UpdateResult{}, nil
}

type s3BucketStorage interface {
	CreateBucket(context.Context, string) error
	DeleteBucket(context.Context, string) error
	PutBucketWebsite(context.Context, string, *s3.WebsiteConfiguration) error
	GetBucketWebsite(context.Context, string) (*s3.WebsiteConfiguration, error)
}

func createS3Bucket(ctx context.Context, storage s3BucketStorage, name string, websiteConfiguration *s3.WebsiteConfiguration) (string, error) {
	if err := storage.CreateBucket(ctx, name); err != nil {
		return "", fmt.Errorf("create S3 bucket: %w", err)
	}

	websiteURL, err := configureS3Website(ctx, storage, name, websiteConfiguration)
	if err == nil {
		return websiteURL, nil
	}

	if cleanupErr := storage.DeleteBucket(ctx, name); cleanupErr != nil {
		return "", errors.Join(err, fmt.Errorf("cleanup S3 bucket: %w", cleanupErr))
	}

	return "", err
}

func decodeS3WebsiteConfigurationProperty(properties map[string]any) (*s3.WebsiteConfiguration, error) {
	websiteValue, configured := properties["WebsiteConfiguration"]
	if !configured {
		return nil, nil //nolint:nilnil // An absent optional website configuration is represented by nil.
	}

	return decodeS3WebsiteConfiguration(websiteValue)
}

type cloudFormationWebsiteConfiguration struct {
	IndexDocument         json.RawMessage           `json:"IndexDocument"`         //nolint:tagliatelle // CloudFormation uses PascalCase property names.
	ErrorDocument         json.RawMessage           `json:"ErrorDocument"`         //nolint:tagliatelle // CloudFormation uses PascalCase property names.
	RedirectAllRequestsTo *s3.RedirectAllRequestsTo `json:"RedirectAllRequestsTo"` //nolint:tagliatelle // CloudFormation uses PascalCase property names.
	RoutingRules          json.RawMessage           `json:"RoutingRules"`          //nolint:tagliatelle // CloudFormation uses PascalCase property names.
}

type cloudFormationRoutingRule struct {
	RoutingRuleCondition *cloudFormationRoutingRuleCondition `json:"RoutingRuleCondition"` //nolint:tagliatelle // CloudFormation uses PascalCase property names.
	RedirectRule         *cloudFormationRedirectRule         `json:"RedirectRule"`         //nolint:tagliatelle // CloudFormation uses PascalCase property names.
}

type cloudFormationRoutingRuleCondition struct {
	KeyPrefixEquals             string `json:"KeyPrefixEquals"`             //nolint:tagliatelle // CloudFormation uses PascalCase property names.
	HTTPErrorCodeReturnedEquals string `json:"HttpErrorCodeReturnedEquals"` //nolint:tagliatelle // CloudFormation uses PascalCase property names.
}

type cloudFormationRedirectRule struct {
	HostName             string `json:"HostName"`             //nolint:tagliatelle // CloudFormation uses PascalCase property names.
	Protocol             string `json:"Protocol"`             //nolint:tagliatelle // CloudFormation uses PascalCase property names.
	ReplaceKeyPrefixWith string `json:"ReplaceKeyPrefixWith"` //nolint:tagliatelle // CloudFormation uses PascalCase property names.
	ReplaceKeyWith       string `json:"ReplaceKeyWith"`       //nolint:tagliatelle // CloudFormation uses PascalCase property names.
	HTTPRedirectCode     string `json:"HttpRedirectCode"`     //nolint:tagliatelle // CloudFormation uses PascalCase property names.
}

func decodeS3WebsiteConfiguration(value any) (*s3.WebsiteConfiguration, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal S3 WebsiteConfiguration: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()

	var properties cloudFormationWebsiteConfiguration
	if err := decoder.Decode(&properties); err != nil {
		return nil, fmt.Errorf("decode S3 WebsiteConfiguration: %w", err)
	}

	if string(encoded) == "null" {
		return nil, fmt.Errorf("decode S3 WebsiteConfiguration: must be an object")
	}

	routingRules, err := decodeS3WebsiteRoutingRules(properties.RoutingRules)
	if err != nil {
		return nil, err
	}

	configuration := &s3.WebsiteConfiguration{
		RedirectAllRequestsTo: properties.RedirectAllRequestsTo,
		RoutingRules:          routingRules,
	}

	if properties.IndexDocument != nil {
		indexDocument, err := decodeS3WebsiteDocument("IndexDocument", properties.IndexDocument)
		if err != nil {
			return nil, err
		}

		configuration.IndexDocument = &s3.WebsiteIndexDocument{Suffix: indexDocument}
	}

	if properties.ErrorDocument != nil {
		errorDocument, err := decodeS3WebsiteDocument("ErrorDocument", properties.ErrorDocument)
		if err != nil {
			return nil, err
		}

		configuration.ErrorDocument = &s3.WebsiteErrorDocument{Key: errorDocument}
	}

	return configuration, nil
}

func decodeS3WebsiteRoutingRules(raw json.RawMessage) (*s3.WebsiteRoutingRules, error) {
	if raw == nil {
		return nil, nil //nolint:nilnil // An absent optional routing-rules configuration is represented by nil.
	}

	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("S3 WebsiteConfiguration.RoutingRules must be an array")
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	var properties []cloudFormationRoutingRule
	if err := decoder.Decode(&properties); err != nil {
		return nil, fmt.Errorf("decode S3 WebsiteConfiguration.RoutingRules: %w", err)
	}

	routingRules := &s3.WebsiteRoutingRules{Rules: make([]s3.WebsiteRoutingRule, len(properties))}

	for index, property := range properties {
		if property.RedirectRule == nil {
			return nil, fmt.Errorf("S3 WebsiteConfiguration.RoutingRules[%d].RedirectRule is required", index)
		}

		var condition *s3.RoutingRuleCondition
		if property.RoutingRuleCondition != nil {
			condition = &s3.RoutingRuleCondition{
				KeyPrefixEquals:             property.RoutingRuleCondition.KeyPrefixEquals,
				HTTPErrorCodeReturnedEquals: property.RoutingRuleCondition.HTTPErrorCodeReturnedEquals,
			}
		}

		routingRules.Rules[index] = s3.WebsiteRoutingRule{
			Condition: condition,
			Redirect: s3.RoutingRuleRedirect{
				HostName:             property.RedirectRule.HostName,
				Protocol:             property.RedirectRule.Protocol,
				ReplaceKeyPrefixWith: property.RedirectRule.ReplaceKeyPrefixWith,
				ReplaceKeyWith:       property.RedirectRule.ReplaceKeyWith,
				HTTPRedirectCode:     property.RedirectRule.HTTPRedirectCode,
			},
		}
	}

	return routingRules, nil
}

func configureS3Website(ctx context.Context, storage s3BucketStorage, name string, configuration *s3.WebsiteConfiguration) (string, error) {
	if configuration == nil {
		return "", nil
	}

	if err := storage.PutBucketWebsite(ctx, name, configuration); err != nil {
		return "", fmt.Errorf("configure S3 bucket website: %w", err)
	}

	observed, err := storage.GetBucketWebsite(ctx, name)
	if err != nil {
		return "", fmt.Errorf("observe S3 bucket website: %w", err)
	}

	if observed == nil {
		return "", fmt.Errorf("observe S3 bucket website: backend returned no configuration")
	}

	return "http://" + name + ".s3-website-us-east-1.amazonaws.com", nil
}

func decodeS3WebsiteDocument(name string, raw json.RawMessage) (string, error) {
	if string(raw) == "null" {
		return "", fmt.Errorf("S3 WebsiteConfiguration.%s must be a string", name)
	}

	var document string
	if err := json.Unmarshal(raw, &document); err != nil {
		return "", fmt.Errorf("S3 WebsiteConfiguration.%s must be a string: %w", name, err)
	}

	return document, nil
}

func (*s3BucketAdapter) Delete(ctx context.Context, req DeleteRequest) error {
	storage, err := lookupStorage[s3.Storage]("s3")
	if err != nil {
		return err
	}

	if err := storage.DeleteBucket(ctx, req.Identifier); err != nil {
		return fmt.Errorf("delete S3 bucket: %w", err)
	}

	return nil
}
