package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sivchari/kumo/internal/service/iam"
)

type iamRoleProvisionRequest struct {
	Create            *iam.CreateRoleRequest
	ManagedPolicyARNs []string
	InlinePolicies    []iamInlineRolePolicy
}

type iamInlineRolePolicy struct {
	Name     string
	Document string
}

type iamRoleProvisionStorage interface {
	CreateRole(context.Context, *iam.CreateRoleRequest) (*iam.Role, error)
	DeleteRole(context.Context, string) error
	AttachRolePolicy(context.Context, string, string) error
	DetachRolePolicy(context.Context, string, string) error
	PutRolePolicy(context.Context, string, string, string) error
	DeleteRolePolicy(context.Context, string, string) error
}

//nolint:wsl // Provisioning and rollback steps are intentionally kept together.
func createIAMRole(ctx context.Context, storage iamRoleProvisionStorage, req iamRoleProvisionRequest) (*iam.Role, error) {
	role, err := storage.CreateRole(ctx, req.Create)
	if err != nil {
		return nil, fmt.Errorf("create IAM role: %w", err)
	}

	attached := make([]string, 0, len(req.ManagedPolicyARNs))
	for _, policyARN := range req.ManagedPolicyARNs {
		if err := storage.AttachRolePolicy(ctx, role.RoleName, policyARN); err != nil {
			return role, rollbackIAMRole(ctx, storage, role.RoleName, attached, nil, err)
		}
		attached = append(attached, policyARN)
	}

	inline := make([]string, 0, len(req.InlinePolicies))
	for _, policy := range req.InlinePolicies {
		if err := storage.PutRolePolicy(ctx, role.RoleName, policy.Name, policy.Document); err != nil {
			return role, rollbackIAMRole(ctx, storage, role.RoleName, attached, inline, err)
		}
		inline = append(inline, policy.Name)
	}

	return role, nil
}

//nolint:wsl // Rollback executes every compensating action while preserving all errors.
func rollbackIAMRole(ctx context.Context, storage iamRoleProvisionStorage, roleName string, attached, inline []string, cause error) error {
	rollbackErrs := make([]error, 0, len(attached)+len(inline)+1)
	for index := len(inline) - 1; index >= 0; index-- {
		if err := storage.DeleteRolePolicy(ctx, roleName, inline[index]); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("delete inline IAM policy %q during rollback: %w", inline[index], err))
		}
	}
	for index := len(attached) - 1; index >= 0; index-- {
		if err := storage.DetachRolePolicy(ctx, roleName, attached[index]); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("detach IAM policy %q during rollback: %w", attached[index], err))
		}
	}
	if err := storage.DeleteRole(ctx, roleName); err != nil {
		rollbackErrs = append(rollbackErrs, fmt.Errorf("delete IAM role during rollback: %w", err))
	}

	return errors.Join(cause, errors.Join(rollbackErrs...))
}

type iamPolicyProvisionRequest struct {
	PolicyName     string
	PolicyDocument string
	Roles          []string
}

type iamPolicyProvisionStorage interface {
	PutRolePolicy(context.Context, string, string, string) error
	DeleteRolePolicy(context.Context, string, string) error
}

//nolint:wsl // Provisioning and rollback steps are intentionally kept together.
func createIAMPolicy(ctx context.Context, storage iamPolicyProvisionStorage, req iamPolicyProvisionRequest) (*iam.Policy, error) {
	policy := &iam.Policy{PolicyName: req.PolicyName, PolicyID: req.PolicyName}

	provisioned := make([]string, 0, len(req.Roles))
	for _, roleName := range req.Roles {
		if err := storage.PutRolePolicy(ctx, roleName, req.PolicyName, req.PolicyDocument); err != nil {
			return policy, rollbackIAMPolicy(ctx, storage, req.PolicyName, provisioned, err)
		}
		provisioned = append(provisioned, roleName)
	}

	return policy, nil
}

//nolint:wsl // Rollback executes every compensating action while preserving all errors.
func rollbackIAMPolicy(ctx context.Context, storage iamPolicyProvisionStorage, policyName string, provisioned []string, cause error) error {
	rollbackErrs := make([]error, 0, len(provisioned))
	for index := len(provisioned) - 1; index >= 0; index-- {
		if err := storage.DeleteRolePolicy(ctx, provisioned[index], policyName); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("delete inline IAM policy %q from %q during rollback: %w", policyName, provisioned[index], err))
		}
	}

	return errors.Join(cause, errors.Join(rollbackErrs...))
}

type sqsQueuePolicyProvisionStorage interface {
	GetQueueAttributes(context.Context, string, []string) (map[string]string, error)
	SetQueueAttributes(context.Context, string, map[string]string) error
}

//nolint:wsl // Queue policy setup and compensation form one operation.
func createSQSQueuePolicy(ctx context.Context, storage sqsQueuePolicyProvisionStorage, queues []string, policy string) (string, error) {
	original := make(map[string]string, len(queues))
	for _, queueURL := range queues {
		attributes, err := storage.GetQueueAttributes(ctx, queueURL, []string{"Policy"})
		if err != nil {
			return "", fmt.Errorf("get SQS queue policy: %w", err)
		}
		original[queueURL] = attributes[policyProperty]
	}

	for index, queueURL := range queues {
		if err := storage.SetQueueAttributes(ctx, queueURL, map[string]string{policyProperty: policy}); err != nil {
			rollbackErrs := make([]error, 0, index)
			for rollbackIndex := index - 1; rollbackIndex >= 0; rollbackIndex-- {
				rollbackQueue := queues[rollbackIndex]
				if rollbackErr := storage.SetQueueAttributes(ctx, rollbackQueue, map[string]string{policyProperty: original[rollbackQueue]}); rollbackErr != nil {
					rollbackErrs = append(rollbackErrs, fmt.Errorf("restore SQS queue policy on %q: %w", rollbackQueue, rollbackErr))
				}
			}

			return "", errors.Join(fmt.Errorf("set SQS queue policy: %w", err), errors.Join(rollbackErrs...))
		}
	}

	identifier, err := json.Marshal(queues)
	if err != nil {
		return "", fmt.Errorf("encode SQS queue policy identifier: %w", err)
	}

	return string(identifier), nil
}
