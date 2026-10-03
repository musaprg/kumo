package cloudformation

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// StackIdentity is the stable identity encoded by a CloudFormation stack ARN.
type StackIdentity struct {
	Partition string
	Region    string
	AccountID string
	StackName string
	UUID      string
}

// ARN returns the CloudFormation stack ARN for the identity.
//
//nolint:gocritic // hugeParam: value receiver preserves value semantics for the public identity helper.
func (s StackIdentity) ARN() string {
	return fmt.Sprintf("arn:%s:%s:%s:%s:stack/%s/%s", s.Partition, serviceName, s.Region, s.AccountID, s.StackName, s.UUID)
}

// ParseStackARN parses a CloudFormation stack ARN.
func ParseStackARN(value string) (StackIdentity, error) {
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != serviceName || !validPartition(parts[1]) || !validRegion(parts[3]) || !validAccountID(parts[4]) {
		return StackIdentity{}, fmt.Errorf("invalid CloudFormation stack ARN %q", value)
	}

	resourceParts := strings.Split(parts[5], "/")

	if len(resourceParts) != 3 || resourceParts[0] != "stack" || !validStackName(resourceParts[1]) || !validStackUUID(resourceParts[2]) {
		return StackIdentity{}, fmt.Errorf("invalid CloudFormation stack ARN resource %q", parts[5])
	}

	return StackIdentity{
		Partition: parts[1],
		Region:    parts[3],
		AccountID: parts[4],
		StackName: resourceParts[1],
		UUID:      resourceParts[2],
	}, nil
}

func validPartition(value string) bool {
	switch value {
	case "aws", "aws-cn", "aws-us-gov":
		return true
	default:
		return false
	}
}

func validRegion(value string) bool {
	if len(value) < 7 || value[2] != '-' || !isASCIILower(value[0]) || !isASCIILower(value[1]) {
		return false
	}

	hyphenCount, valid := regionCharacters(value)
	if !valid || hyphenCount < 2 {
		return false
	}

	return validRegionSuffix(value)
}

func regionCharacters(value string) (int, bool) {
	hyphenCount := 0

	for index := 1; index < len(value); index++ {
		character := value[index]
		if isASCIILower(character) || character >= '0' && character <= '9' {
			continue
		}

		if character != '-' || value[index-1] == '-' {
			return 0, false
		}

		hyphenCount++
	}

	return hyphenCount, true
}

func validRegionSuffix(value string) bool {
	lastHyphen := strings.LastIndexByte(value, '-')
	if lastHyphen == len(value)-1 {
		return false
	}

	for index := lastHyphen + 1; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}

	return true
}

func validAccountID(value string) bool {
	if len(value) != 12 {
		return false
	}

	for index := range value {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}

	return true
}

func validStackName(value string) bool {
	if value == "" || len(value) > 128 || !isASCIIAlpha(value[0]) {
		return false
	}

	for index := 1; index < len(value); index++ {
		character := value[index]
		if !isASCIIAlpha(character) && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}

	return true
}

func validStackUUID(value string) bool {
	parsed, err := uuid.Parse(value)

	return err == nil && parsed.String() == value
}

func isASCIIAlpha(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isASCIILower(value byte) bool {
	return value >= 'a' && value <= 'z'
}
