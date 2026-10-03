package cloudformation

import (
	"context"
	"strings"
	"testing"
)

const (
	testApplicationStackName = "application"
	testRegion               = "us-east-1"
)

func TestStackIdentityBuildsAndParsesARN(t *testing.T) {
	t.Parallel()

	identity := StackIdentity{
		Partition: "aws",
		Region:    testRegion,
		AccountID: "123456789012",
		StackName: testApplicationStackName,
		UUID:      "01234567-89ab-cdef-0123-456789abcdef",
	}
	wantARN := "arn:aws:cloudformation:us-east-1:123456789012:stack/application/01234567-89ab-cdef-0123-456789abcdef"

	if got := identity.ARN(); got != wantARN {
		t.Fatalf("ARN() = %q, want %q", got, wantARN)
	}

	parsed, err := ParseStackARN(wantARN)
	if err != nil {
		t.Fatalf("ParseStackARN() error = %v", err)
	}

	if parsed != identity {
		t.Fatalf("ParseStackARN() = %#v, want %#v", parsed, identity)
	}
}

func TestParseStackARNRejectsMalformedOrNonCloudFormationARN(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"",
		"arn:aws:s3:::bucket",
		"arn:aws:cloudformation:us-east-1:123456789012:stack/name",
		"arn:aws:cloudformation:us-east-1:123456789012:stack//uuid",
		"arn:aws:cloudformation:us-east-1:123456789012:stack/name/01234567-89ab-cdef-0123-456789abcdef/extra",
		"arn:aws:cloudformation:us-east-1:123456789012:stack/name/not-a-uuid",
		"arn:aws:cloudformation:us-east-1:123456789012:invalid/name/01234567-89ab-cdef-0123-456789abcdef",
		"arn:aws:cloudformation:us-east-1:123456789012:stack/name_with_invalid_character/01234567-89ab-cdef-0123-456789abcdef",
	} {
		if _, err := ParseStackARN(value); err == nil {
			t.Errorf("ParseStackARN(%q) error = nil, want error", value)
		}
	}
}

func TestParseStackARNRejectsInvalidPartitionRegionAndAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
	}{
		{
			name:  "partition",
			value: "arn:aws-invalid:cloudformation:us-east-1:123456789012:stack/name/01234567-89ab-cdef-0123-456789abcdef",
		},
		{
			name:  "region",
			value: "arn:aws:cloudformation:us-east:123456789012:stack/name/01234567-89ab-cdef-0123-456789abcdef",
		},
		{
			name:  "account",
			value: "arn:aws:cloudformation:us-east-1:12345678901:stack/name/01234567-89ab-cdef-0123-456789abcdef",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseStackARN(test.value); err == nil {
				t.Fatalf("ParseStackARN(%q) error = nil, want error", test.value)
			}
		})
	}
}

func TestGenerateStackIDUsesCanonicalStackIdentity(t *testing.T) {
	t.Parallel()

	stackID := generateStackID(testApplicationStackName)

	identity, err := ParseStackARN(stackID)
	if err != nil {
		t.Fatalf("ParseStackARN(%q) error = %v", stackID, err)
	}

	want := StackIdentity{
		Partition: "aws",
		Region:    testRegion,
		AccountID: "123456789012",
		StackName: testApplicationStackName,
	}
	if identity.Partition != want.Partition || identity.Region != want.Region || identity.AccountID != want.AccountID || identity.StackName != want.StackName {
		t.Fatalf("generateStackID() identity = %#v, want fixed fields %#v", identity, want)
	}
}

func TestCreateStackRejectsNamesThatCannotRoundTripThroughARN(t *testing.T) {
	t.Parallel()

	invalidNames := []string{
		"1starts-with-number",
		"contains_underscore",
		strings.Repeat("a", 129),
	}

	for _, stackName := range invalidNames {
		t.Run(stackName, func(t *testing.T) {
			storage := NewMemoryStorage()
			_, err := storage.CreateStack(context.Background(), &CreateStackRequest{
				StackName:    stackName,
				TemplateBody: "{}",
			})

			if err == nil {
				t.Fatal("CreateStack() error = nil, want invalid stack name error")
			}

			if len(storage.Stacks) != 0 {
				t.Fatalf("storage contains invalid stack name %q", stackName)
			}
		})
	}
}
