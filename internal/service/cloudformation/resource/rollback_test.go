package resource

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sivchari/kumo/internal/service/iam"
)

const (
	rollbackRoleName       = "role"
	rollbackQueueOne       = "queue-1"
	rollbackQueueTwo       = "queue-2"
	rollbackOldOne         = "old-1"
	rollbackPolicyDocument = `{"Version":"2012-10-17"}`
)

func TestIAMRoleRollbackPreservesAttachAndCleanupErrors(t *testing.T) {
	attachErr := errors.New("attach failed")
	deleteErr := errors.New("delete role failed")
	storage := &iamRoleRollbackStorage{attachErr: attachErr, deleteErr: deleteErr}

	_, err := createIAMRole(context.Background(), storage, iamRoleProvisionRequest{
		Create:            &iam.CreateRoleRequest{RoleName: rollbackRoleName},
		ManagedPolicyARNs: []string{"policy-1", "policy-2"},
	})
	if !errors.Is(err, attachErr) || !errors.Is(err, deleteErr) {
		t.Fatalf("createIAMRole() error = %v, want attach and cleanup errors", err)
	}

	if !storage.deleted || len(storage.detached) != 1 || storage.detached[0] != "policy-1" {
		t.Fatalf("role rollback = deleted %t, detached %v; want role deletion and first attachment detached", storage.deleted, storage.detached)
	}
}

func TestIAMPolicyRollbackPreservesInlineAndCleanupErrors(t *testing.T) {
	putErr := errors.New("inline policy put failed")
	deleteErr := errors.New("inline policy delete failed")
	storage := &iamPolicyRollbackStorage{putErr: putErr, deleteErr: deleteErr}

	_, err := createIAMPolicy(context.Background(), storage, iamPolicyProvisionRequest{
		PolicyName:     "policy",
		PolicyDocument: rollbackPolicyDocument,
		Roles:          []string{"role-1", "role-2"},
	})
	if !errors.Is(err, putErr) || !errors.Is(err, deleteErr) {
		t.Fatalf("createIAMPolicy() error = %v, want inline put and cleanup errors", err)
	}

	if len(storage.deleted) != 1 || storage.deleted[0] != "role-1" {
		t.Fatalf("policy rollback deleted roles = %v; want first inline policy removed", storage.deleted)
	}
}

func TestSQSQueuePolicyRollbackPreservesSetAndCleanupErrors(t *testing.T) {
	setErr := errors.New("set queue policy failed")
	cleanupErr := errors.New("restore queue policy failed")
	storage := &sqsQueuePolicyRollbackStorage{
		policies:   map[string]string{rollbackQueueOne: rollbackOldOne, rollbackQueueTwo: "old-2"},
		setErr:     setErr,
		cleanupErr: cleanupErr,
	}

	_, err := createSQSQueuePolicy(context.Background(), storage, []string{rollbackQueueOne, rollbackQueueTwo}, "new-policy")
	if !errors.Is(err, setErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("createSQSQueuePolicy() error = %v, want set and cleanup errors", err)
	}

	if storage.policies[rollbackQueueOne] != "new-policy" {
		t.Fatalf("queue-1 policy = %q, want fake failed restore to be observable", storage.policies[rollbackQueueOne])
	}

	if len(storage.setCalls) != 3 {
		t.Fatalf("queue policy set calls = %v, want initial queue-1, failed queue-2, and queue-1 rollback", storage.setCalls)
	}
}

func TestSQSQueuePolicyRollbackRestoresPreviousPolicies(t *testing.T) {
	setErr := errors.New("set queue policy failed")
	storage := &sqsQueuePolicyRollbackStorage{
		policies: map[string]string{rollbackQueueOne: rollbackOldOne, rollbackQueueTwo: "old-2"},
		setErr:   setErr,
	}

	_, err := createSQSQueuePolicy(context.Background(), storage, []string{rollbackQueueOne, rollbackQueueTwo}, "new-policy")
	if !errors.Is(err, setErr) {
		t.Fatalf("createSQSQueuePolicy() error = %v, want set error", err)
	}

	if storage.policies[rollbackQueueOne] != rollbackOldOne {
		t.Fatalf("queue-1 policy after rollback = %q, want old-1", storage.policies[rollbackQueueOne])
	}
}

type iamRoleRollbackStorage struct {
	attachErr error
	deleteErr error
	deleted   bool
	detached  []string
}

func (*iamRoleRollbackStorage) CreateRole(context.Context, *iam.CreateRoleRequest) (*iam.Role, error) {
	return &iam.Role{RoleName: "role", Arn: "arn:role"}, nil
}

func (s *iamRoleRollbackStorage) AttachRolePolicy(_ context.Context, _, policyARN string) error {
	if policyARN == "policy-2" {
		return s.attachErr
	}

	return nil
}

func (s *iamRoleRollbackStorage) DetachRolePolicy(_ context.Context, _, policyARN string) error {
	s.detached = append(s.detached, policyARN)

	return nil
}

func (*iamRoleRollbackStorage) PutRolePolicy(context.Context, string, string, string) error {
	return nil
}

func (*iamRoleRollbackStorage) DeleteRolePolicy(context.Context, string, string) error {
	return nil
}

func (s *iamRoleRollbackStorage) DeleteRole(context.Context, string) error {
	s.deleted = true

	return s.deleteErr
}

type iamPolicyRollbackStorage struct {
	putErr    error
	deleteErr error
	deleted   []string
}

func (s *iamPolicyRollbackStorage) PutRolePolicy(_ context.Context, roleName, _, _ string) error {
	if roleName == "role-2" {
		return s.putErr
	}

	return nil
}

func (s *iamPolicyRollbackStorage) DeleteRolePolicy(_ context.Context, roleName, _ string) error {
	s.deleted = append(s.deleted, roleName)

	return s.deleteErr
}

type sqsQueuePolicyRollbackStorage struct {
	policies   map[string]string
	setErr     error
	cleanupErr error
	setCalls   []string
}

func (s *sqsQueuePolicyRollbackStorage) GetQueueAttributes(_ context.Context, queueURL string, _ []string) (map[string]string, error) {
	return map[string]string{policyProperty: s.policies[queueURL]}, nil
}

func (s *sqsQueuePolicyRollbackStorage) SetQueueAttributes(_ context.Context, queueURL string, attributes map[string]string) error {
	s.setCalls = append(s.setCalls, fmt.Sprintf("%s=%s", queueURL, attributes[policyProperty]))
	if queueURL == rollbackQueueTwo && attributes[policyProperty] == "new-policy" {
		return s.setErr
	}

	if queueURL == rollbackQueueOne && attributes[policyProperty] == rollbackOldOne && s.cleanupErr != nil {
		return s.cleanupErr
	}

	s.policies[queueURL] = attributes[policyProperty]

	return nil
}
