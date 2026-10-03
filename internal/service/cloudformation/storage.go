package cloudformation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"

	"github.com/sivchari/kumo/internal/service/cloudformation/resource"
	"github.com/sivchari/kumo/internal/service/cloudformation/template"
	"github.com/sivchari/kumo/internal/storage"
)

// Storage defines the interface for CloudFormation storage operations.
type Storage interface {
	CreateStack(ctx context.Context, req *CreateStackRequest) (*Stack, error)
	DeleteStack(ctx context.Context, stackName string) error
	DescribeStacks(ctx context.Context, stackName string) ([]*Stack, error)
	ListStacks(ctx context.Context, statusFilter []string) ([]*Stack, error)
	UpdateStack(ctx context.Context, req *UpdateStackRequest) (*Stack, error)
	DescribeStackResources(ctx context.Context, stackName, logicalResourceID string) ([]*StackResource, error)
	GetTemplate(ctx context.Context, stackName string) (string, error)
	ValidateTemplate(ctx context.Context, templateBody string) (*TemplateValidationResult, error)
}

type changeSetStorage interface {
	RunExclusiveStackOperation(ctx context.Context, stackRef string, operation func(context.Context) error) error
	CreateReviewStack(ctx context.Context, stackName string) (*Stack, error)
	CreateChangeSet(ctx context.Context, req *CreateChangeSetRequest, evaluation *template.Evaluation) (*ChangeSet, error)
	GetChangeSet(ctx context.Context, stackRef, changeSetRef string) (*ChangeSet, error)
	MarkChangeSetExecution(ctx context.Context, stackRef, changeSetRef string, success bool, reason string) error
}

type atomicChangeSetExecutionStorage interface {
	RunChangeSetExecution(ctx context.Context, stackRef string, operation func(context.Context) (bool, error)) error
}

type createExecutionStorage interface {
	ReserveCreateExecution(ctx context.Context, token, fingerprint, stackName string) (CreateExecutionRecord, bool, error)
	CompleteCreateExecution(ctx context.Context, token string, stackID, errorCode, reason string, success bool) error
	ReleaseCreateExecution(ctx context.Context, token string) error
}

type executionRecordReader interface {
	GetExecution(ctx context.Context, stackRef, token string) (ExecutionRecord, bool, error)
}

// Option is a configuration option for MemoryStorage.
type Option func(*MemoryStorage)

// WithDataDir enables persistent storage in the specified directory.
func WithDataDir(dir string) Option {
	return func(s *MemoryStorage) {
		s.dataDir = dir
	}
}

// Compile-time interface checks.
var (
	_ json.Marshaler   = (*MemoryStorage)(nil)
	_ json.Unmarshaler = (*MemoryStorage)(nil)
)

// MemoryStorage is an in-memory implementation of Storage.
type MemoryStorage struct {
	mu               sync.RWMutex                     `json:"-"`
	Stacks           map[string]*Stack                `json:"stacks"` // key: canonical stack ARN
	LiveNameIndex    map[string]string                `json:"liveNameIndex"`
	CreateExecutions map[string]CreateExecutionRecord `json:"createExecutions"`
	ChangeSets       map[string]*ChangeSet            `json:"changeSets"`
	dataDir          string
	operationLock    map[string]chan struct{}      `json:"-"`
	stagedExports    map[string]stagedExportRecord `json:"-"`
	stagedImports    map[string]stagedImportRecord `json:"-"`
	initErr          error                         `json:"-"`
}

type stackOperationContextKey struct{}

type stackOperationContext struct {
	arn string
}

type executionTokenContextKey struct{}

type persistedStorage struct {
	Stacks           map[string]*Stack                `json:"stacks"`
	LiveNameIndex    map[string]string                `json:"liveNameIndex"`
	CreateExecutions map[string]CreateExecutionRecord `json:"createExecutions"`
	ChangeSets       map[string]*ChangeSet            `json:"changeSets"`
}

type stagedExportRecord struct {
	StackARN     string
	BaseRevision uint64
	Exports      []StackExport
}

type stagedImportRecord struct {
	StackARN     string
	BaseRevision uint64
	Imports      []StackImport
}

type storageSnapshot struct {
	Stacks           map[string]*Stack
	LiveNameIndex    map[string]string
	CreateExecutions map[string]CreateExecutionRecord
	ChangeSets       map[string]*ChangeSet
	StagedExports    map[string]stagedExportRecord
	StagedImports    map[string]stagedImportRecord
}

const (
	errAlreadyExists = "AlreadyExistsException"
	errExportInUse   = "ExportInUseException"
)

// NewMemoryStorage creates a new MemoryStorage. Initialization failures are
// returned by subsequent storage operations; use NewMemoryStorageWithError when
// the caller needs to handle the failure at construction time.
func NewMemoryStorage(opts ...Option) *MemoryStorage {
	s, err := NewMemoryStorageWithError(opts...)
	if err != nil {
		s.initErr = err
	}

	return s
}

// NewMemoryStorageWithError creates a new MemoryStorage and loads its snapshot.
func NewMemoryStorageWithError(opts ...Option) (*MemoryStorage, error) {
	s := &MemoryStorage{
		Stacks:           make(map[string]*Stack),
		LiveNameIndex:    make(map[string]string),
		CreateExecutions: make(map[string]CreateExecutionRecord),
		ChangeSets:       make(map[string]*ChangeSet),
		operationLock:    make(map[string]chan struct{}),
		stagedExports:    make(map[string]stagedExportRecord),
		stagedImports:    make(map[string]stagedImportRecord),
	}
	for _, o := range opts {
		o(s)
	}

	if s.dataDir != "" {
		if err := storage.Load(s.dataDir, "cloudformation", s); err != nil {
			s.initErr = fmt.Errorf("load cloudformation storage: %w", err)

			return s, s.initErr
		}
	}

	return s, nil
}

// MarshalJSON serializes the storage state to JSON.
func (m *MemoryStorage) MarshalJSON() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.initErr != nil {
		return nil, m.initErr
	}

	data, err := json.Marshal(persistedStorage{
		Stacks:           cloneStacks(m.Stacks),
		LiveNameIndex:    cloneStringMap(m.LiveNameIndex),
		CreateExecutions: cloneCreateExecutions(m.CreateExecutions),
		ChangeSets:       cloneChangeSets(m.ChangeSets),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal: %w", err)
	}

	return data, nil
}

// UnmarshalJSON restores the storage state from JSON.
//
//nolint:cyclop,funlen,wsl // Snapshot restoration keeps compatibility normalization together.
func (m *MemoryStorage) UnmarshalJSON(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var snapshot persistedStorage
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("failed to unmarshal: %w", err)
	}

	stacks := make(map[string]*Stack, len(snapshot.Stacks))
	for key, stack := range snapshot.Stacks {
		if stack == nil {
			return fmt.Errorf("invalid persisted stack record %q: record is null", key)
		}
		if lock := stack.OperationLock; lock != nil && (lock.Token == "" || lock.Operation == "" || lock.StartedAt.IsZero()) {
			return fmt.Errorf("invalid persisted operation lock for record %q", key)
		}
		canonicalARN := stack.StackID
		if canonicalARN == "" {
			canonicalARN = key
		}
		identity, err := ParseStackARN(canonicalARN)
		if err != nil {
			return fmt.Errorf("invalid persisted stack ARN for record %q: %w", key, err)
		}
		canonicalARN = identity.ARN()
		if _, exists := stacks[canonicalARN]; exists {
			return fmt.Errorf("duplicate persisted stack ARN %q", canonicalARN)
		}
		stack.StackID = canonicalARN
		stacks[canonicalARN] = cloneStack(stack)
	}

	m.Stacks = stacks
	m.CreateExecutions = cloneCreateExecutions(snapshot.CreateExecutions)
	if m.CreateExecutions == nil {
		m.CreateExecutions = make(map[string]CreateExecutionRecord)
	}
	m.ChangeSets = cloneChangeSets(snapshot.ChangeSets)
	if m.ChangeSets == nil {
		m.ChangeSets = make(map[string]*ChangeSet)
	}
	m.LiveNameIndex = make(map[string]string, len(m.Stacks))
	for arn, stack := range m.Stacks {
		if stack.StackStatus != StackStatusDeleteComplete {
			m.LiveNameIndex[stack.StackName] = arn
		}
		if stack.Executions == nil {
			stack.Executions = make(map[string]ExecutionRecord)
		}
	}
	if m.operationLock == nil {
		m.operationLock = make(map[string]chan struct{})
	}
	// Stages are process-local handles and cannot remain valid across a state restore.
	m.stagedExports = make(map[string]stagedExportRecord)
	m.stagedImports = make(map[string]stagedImportRecord)
	m.initErr = nil

	return nil
}

// saveLocked persists the current state to disk while the caller holds the lock.
func (m *MemoryStorage) saveLocked() error {
	if m.dataDir == "" {
		return nil
	}

	data, err := json.Marshal(persistedStorage{
		Stacks:           cloneStacks(m.Stacks),
		LiveNameIndex:    cloneStringMap(m.LiveNameIndex),
		CreateExecutions: cloneCreateExecutions(m.CreateExecutions),
		ChangeSets:       cloneChangeSets(m.ChangeSets),
	})
	if err != nil {
		return fmt.Errorf("marshal cloudformation storage: %w", err)
	}

	if err := storage.SaveBytes(m.dataDir, "cloudformation", data); err != nil {
		return fmt.Errorf("save cloudformation storage: %w", err)
	}

	return nil
}

// Close saves the storage state to disk if persistence is enabled.
func (m *MemoryStorage) Close() error {
	if m.initErr != nil {
		return m.initErr
	}

	if m.dataDir == "" {
		return nil
	}

	if err := storage.Save(m.dataDir, "cloudformation", m); err != nil {
		return fmt.Errorf("failed to save: %w", err)
	}

	return nil
}

func (m *MemoryStorage) ready() error {
	if m.initErr != nil {
		return m.initErr
	}

	return nil
}

func (m *MemoryStorage) stackARN(stackRef string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.stackARNLocked(stackRef, true)
}

//nolint:wsl // ARN and live-name resolution intentionally share one validation path.
func (m *MemoryStorage) stackARNLocked(stackRef string, activeOnly bool) (string, error) {
	if identity, err := ParseStackARN(stackRef); err == nil {
		if stack, ok := m.Stacks[identity.ARN()]; ok && (!activeOnly || stack.StackStatus != StackStatusDeleteComplete) {
			return identity.ARN(), nil
		}

		return "", &Error{Code: errStackNotFound, Message: msgStackNotFound}
	}

	arn, ok := m.LiveNameIndex[stackRef]
	if !ok {
		return "", &Error{Code: errStackNotFound, Message: msgStackNotFound}
	}
	stack, ok := m.Stacks[arn]
	if !ok || activeOnly && stack.StackStatus == StackStatusDeleteComplete {
		return "", &Error{Code: errStackNotFound, Message: msgStackNotFound}
	}

	return arn, nil
}

//nolint:wsl // Runtime lock initialization is kept under the storage mutex.
func (m *MemoryStorage) stackOperationChannel(arn string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.operationLock == nil {
		m.operationLock = make(map[string]chan struct{})
	}
	lock := m.operationLock[arn]
	if lock == nil {
		lock = make(chan struct{}, 1)
		m.operationLock[arn] = lock
	}

	return lock
}

//nolint:wsl // Semaphore acquisition and release surround exactly one operation.
func (m *MemoryStorage) withStackLock(ctx context.Context, arn string, operation func() error) error {
	if current, ok := ctx.Value(stackOperationContextKey{}).(stackOperationContext); ok && current.arn == arn {
		return operation()
	}
	lock := m.stackOperationChannel(arn)
	select {
	case lock <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("wait for stack operation: %w", ctx.Err())
	}
	defer func() { <-lock }()

	return operation()
}

func (m *MemoryStorage) snapshotLocked() storageSnapshot {
	return storageSnapshot{
		Stacks:           cloneStacks(m.Stacks),
		LiveNameIndex:    cloneStringMap(m.LiveNameIndex),
		CreateExecutions: cloneCreateExecutions(m.CreateExecutions),
		ChangeSets:       cloneChangeSets(m.ChangeSets),
		StagedExports:    cloneStagedExports(m.stagedExports),
		StagedImports:    cloneStagedImports(m.stagedImports),
	}
}

func (m *MemoryStorage) restoreSnapshotLocked(snapshot storageSnapshot) {
	m.Stacks = snapshot.Stacks
	m.LiveNameIndex = snapshot.LiveNameIndex
	m.CreateExecutions = snapshot.CreateExecutions
	m.ChangeSets = snapshot.ChangeSets
	m.stagedExports = snapshot.StagedExports
	m.stagedImports = snapshot.StagedImports
}

// RunExclusiveStackOperation serializes a complete lifecycle operation for one
// stack without holding the storage mutex while the callback runs.
//
//nolint:wsl // Lock acquisition, callback execution, and cleanup form one boundary.
func (m *MemoryStorage) RunExclusiveStackOperation(ctx context.Context, stackRef string, operation func(context.Context) error) error {
	if err := m.ready(); err != nil {
		return err
	}
	m.mu.RLock()
	arn, err := m.stackARNLocked(stackRef, false)
	m.mu.RUnlock()
	if err != nil {
		return err
	}
	if current, ok := ctx.Value(stackOperationContextKey{}).(stackOperationContext); ok && current.arn == arn {
		return operation(ctx)
	}

	requestToken, tokenized := ctx.Value(executionTokenContextKey{}).(string)
	if tokenized {
		lock := m.stackOperationChannel(arn)
		select {
		case lock <- struct{}{}:
			defer func() { <-lock }()
		default:
			m.mu.RLock()
			stack := m.Stacks[arn]
			var active *StackLock
			if stack != nil {
				active = stack.OperationLock
			}
			matching := active != nil && active.ExecutionToken == requestToken
			m.mu.RUnlock()
			if matching {
				return operationInProgressError()
			}

			return m.withStackLock(ctx, arn, func() error {
				return m.runExclusiveStackOperation(ctx, arn, operation)
			})
		}

		return m.runExclusiveStackOperation(ctx, arn, operation)
	}

	return m.withStackLock(ctx, arn, func() error {
		return m.runExclusiveStackOperation(ctx, arn, operation)
	})
}

// RunChangeSetExecution keeps lifecycle state and terminal review state in one
// compensatable stack operation. The callback's boolean controls whether its
// mutations are committed when it also returns an error.
//
//nolint:wsl // Snapshot, callback, and compensation form one transaction boundary.
func (m *MemoryStorage) RunChangeSetExecution(ctx context.Context, stackRef string, operation func(context.Context) (bool, error)) error {
	return m.RunExclusiveStackOperation(ctx, stackRef, func(operationCtx context.Context) error {
		m.mu.RLock()
		before := m.snapshotLocked()
		m.mu.RUnlock()
		commit, err := operation(operationCtx)
		if commit {
			return err
		}
		m.mu.Lock()
		m.restoreSnapshotLocked(before)
		m.mu.Unlock()

		return err
	})
}

//nolint:wsl // Lease acquisition, callback execution, and terminal cleanup form one boundary.
func (m *MemoryStorage) runExclusiveStackOperation(ctx context.Context, arn string, operation func(context.Context) error) error {
	token := uuid.NewString()
	executionToken, _ := ctx.Value(executionTokenContextKey{}).(string)
	m.mu.Lock()
	stack := m.Stacks[arn]
	if stack == nil {
		m.mu.Unlock()

		return &Error{Code: errStackNotFound, Message: msgStackNotFound}
	}
	if stack.OperationLock != nil {
		m.mu.Unlock()

		return operationInProgressError()
	}
	original := m.snapshotLocked()
	stack.OperationLock = &StackLock{Token: token, Operation: "LifecycleOperation", StartedAt: time.Now(), ExecutionToken: executionToken}
	if err := m.saveLocked(); err != nil {
		m.restoreSnapshotLocked(original)
		m.mu.Unlock()

		return err
	}
	m.mu.Unlock()

	operationErr := operation(context.WithValue(ctx, stackOperationContextKey{}, stackOperationContext{arn: arn}))

	m.mu.Lock()
	if stack = m.Stacks[arn]; stack != nil {
		stack.OperationLock = nil
		if saveErr := m.saveLocked(); saveErr != nil {
			m.restoreSnapshotLocked(original)
			operationErr = errors.Join(operationErr, saveErr)
		}
	}
	m.mu.Unlock()

	return operationErr
}

// RunStackOperation serializes one stack operation and commits its working snapshot.
// The callback runs without the storage mutex, so adapters may safely call other
// services while an operation is in progress.
//
//nolint:funlen,wsl // Lock acquisition, callback execution, and commit form one transaction.
func (m *MemoryStorage) RunStackOperation(ctx context.Context, stackRef string, operation func(context.Context, *Stack) error) error {
	if err := m.ready(); err != nil {
		return err
	}
	arn, err := m.stackARN(stackRef)
	if err != nil {
		return err
	}

	return m.withStackLock(ctx, arn, func() error {
		nested := false
		if current, ok := ctx.Value(stackOperationContextKey{}).(stackOperationContext); ok {
			nested = current.arn == arn
		}
		m.mu.Lock()
		stack := m.Stacks[arn]
		if stack == nil || stack.StackStatus == StackStatusDeleteComplete {
			m.mu.Unlock()

			return &Error{Code: errStackNotFound, Message: msgStackNotFound}
		}
		original := cloneStack(stack)
		if !nested {
			token := uuid.NewString()
			stack.OperationLock = &StackLock{Token: token, Operation: "RunStackOperation", StartedAt: time.Now()}
		}
		working := cloneStack(stack)
		if !nested {
			if err := m.saveLocked(); err != nil {
				m.Stacks[arn] = original
				m.mu.Unlock()

				return err
			}
		}
		m.mu.Unlock()

		operationErr := operation(ctx, working)

		m.mu.Lock()
		defer m.mu.Unlock()
		stack = m.Stacks[arn]
		if stack == nil {
			return &Error{Code: errStackNotFound, Message: msgStackNotFound}
		}
		if operationErr == nil {
			copyStackState(stack, working)
			stack.Revision++
			appendJournalLocked(stack, "RunStackOperation", "COMMITTED", "")
		} else {
			appendJournalLocked(stack, "RunStackOperation", "FAILED", operationErr.Error())
		}
		if !nested {
			stack.OperationLock = nil
		}
		if saveErr := m.saveLocked(); saveErr != nil {
			m.Stacks[arn] = original

			return errors.Join(operationErr, saveErr)
		}

		return operationErr
	})
}

// BeginExecution reserves a client-request token or returns its durable result.
//
//nolint:funlen,nestif,nlreturn,wsl // Lookup and reservation are kept in one serialized boundary.
func (m *MemoryStorage) BeginExecution(ctx context.Context, stackRef, operation, token, fingerprint string) (ExecutionRecord, bool, error) {
	if err := m.ready(); err != nil {
		return ExecutionRecord{}, false, err
	}
	m.mu.RLock()
	arn, err := m.stackARNLocked(stackRef, false)
	m.mu.RUnlock()
	if err != nil {
		return ExecutionRecord{}, false, err
	}
	var existing ExecutionRecord
	replayed := false
	err = m.withStackLock(ctx, arn, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		stack := m.Stacks[arn]
		if stack == nil {
			return &Error{Code: errStackNotFound, Message: msgStackNotFound}
		}
		if stack.Executions == nil {
			stack.Executions = make(map[string]ExecutionRecord)
		}
		if record, ok := stack.Executions[token]; ok {
			if record.Operation != operation || record.Fingerprint != fingerprint {
				return &Error{Code: errTokenAlreadyExists, Message: fmt.Sprintf("client request token %q was reused for a different request", token)}
			}
			if !record.Completed {
				if recovered, ok := recoverExecutionRecordLocked(&record, token, operation, stack); ok {
					stack.Executions[token] = recovered
					existing = recovered
					replayed = true
					if err := m.saveLocked(); err != nil {
						// The lifecycle result is already committed. Keep the recovered
						// record available for an in-process retry if this save fails.
						return nil
					}

					return nil
				}
				return &Error{Code: errTokenAlreadyExists, Message: fmt.Sprintf("client request token %q is already in progress", token)}
			}
			existing = record
			replayed = true

			return nil
		}
		if stack.StackStatus == StackStatusDeleteComplete {
			return &Error{Code: errStackNotFound, Message: msgStackNotFound}
		}
		before := m.snapshotLocked()
		stack.Executions[token] = ExecutionRecord{Operation: operation, Fingerprint: fingerprint, StartedAt: time.Now()}
		stack.Revision++
		if err := m.saveLocked(); err != nil {
			m.restoreSnapshotLocked(before)

			return err
		}

		return nil
	})
	if err != nil {
		return ExecutionRecord{}, false, err
	}

	return existing, replayed, nil
}

// CompleteExecution records the terminal result for a previously reserved token.
//
//nolint:wsl // Lookup and completion are kept in one serialized boundary.
func (m *MemoryStorage) CompleteExecution(ctx context.Context, stackRef, token string, success bool, errorCode, reason string) error {
	if err := m.ready(); err != nil {
		return err
	}
	arn, err := m.stackARNForContext(ctx, stackRef)
	if err != nil {
		return err
	}

	return m.withStackLock(ctx, arn, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		stack := m.Stacks[arn]
		if stack == nil {
			return &Error{Code: errStackNotFound, Message: msgStackNotFound}
		}
		record, ok := stack.Executions[token]
		if !ok {
			return &Error{Code: "TokenNotFoundException", Message: fmt.Sprintf("client request token %q was not reserved", token)}
		}
		before := m.snapshotLocked()
		record.Completed = true
		record.Success = success
		record.ErrorCode = errorCode
		record.Error = reason
		record.CompletedAt = time.Now()
		stack.Executions[token] = record
		stack.Revision++
		if err := m.saveLocked(); err != nil {
			m.restoreSnapshotLocked(before)

			return err
		}

		return nil
	})
}

// GetExecution returns a durable lifecycle result for a client token.
//
//nolint:wsl // Read locking and lookup form one short snapshot boundary.
func (m *MemoryStorage) GetExecution(_ context.Context, stackRef, token string) (ExecutionRecord, bool, error) {
	if err := m.ready(); err != nil {
		return ExecutionRecord{}, false, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	arn, err := m.stackARNLocked(stackRef, false)
	if err != nil {
		return ExecutionRecord{}, false, err
	}
	stack := m.Stacks[arn]
	if stack == nil {
		return ExecutionRecord{}, false, &Error{Code: errStackNotFound, Message: msgStackNotFound}
	}
	record, ok := stack.Executions[token]

	return record, ok, nil
}

func (m *MemoryStorage) stackARNForContext(ctx context.Context, stackRef string) (string, error) {
	if current, ok := ctx.Value(stackOperationContextKey{}).(stackOperationContext); ok {
		m.mu.RLock()
		defer m.mu.RUnlock()
		stack, exists := m.Stacks[current.arn]

		if exists && (stack.StackID == stackRef || stack.StackName == stackRef || current.arn == stackRef) {
			return current.arn, nil
		}
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.stackARNLocked(stackRef, false)
}

func operationInProgressError() error {
	return &Error{Code: "OperationInProgressException", Message: "stack operation is already in progress"}
}

// ReserveCreateExecution reserves a CreateStack token before name reservation.
//
//nolint:nestif,wsl // Reservation and persistence are one atomic boundary.
func (m *MemoryStorage) ReserveCreateExecution(_ context.Context, token, fingerprint, stackName string) (CreateExecutionRecord, bool, error) {
	if err := m.ready(); err != nil {
		return CreateExecutionRecord{}, false, err
	}
	if token == "" {
		return CreateExecutionRecord{}, false, &Error{Code: errInvalidParameter, Message: "client request token is required"}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.CreateExecutions[token]; ok {
		if existing.Fingerprint != fingerprint {
			return CreateExecutionRecord{}, false, &Error{Code: errTokenAlreadyExists, Message: fmt.Sprintf("client request token %q was reused for a different request", token)}
		}
		if !existing.Completed {
			if recovered, ok := m.recoverCreateExecutionLocked(&existing, token); ok {
				m.CreateExecutions[token] = recovered
				if err := m.saveLocked(); err != nil {
					// The lifecycle result is already committed. Keep the recovered
					// record available for an in-process retry if this save fails.
					return recovered, true, nil
				}

				return recovered, true, nil
			}

			return CreateExecutionRecord{}, false, &Error{Code: errTokenAlreadyExists, Message: fmt.Sprintf("client request token %q is already in progress", token)}
		}

		return existing, true, nil
	}

	before := m.snapshotLocked()
	m.CreateExecutions[token] = CreateExecutionRecord{Fingerprint: fingerprint, StackName: stackName, StartedAt: time.Now()}
	if err := m.saveLocked(); err != nil {
		m.restoreSnapshotLocked(before)

		return CreateExecutionRecord{}, false, err
	}

	return CreateExecutionRecord{}, false, nil
}

//nolint:wsl // Recovery scans immutable event history before updating one record.
func (m *MemoryStorage) recoverCreateExecutionLocked(record *CreateExecutionRecord, token string) (CreateExecutionRecord, bool) {
	stacks := m.Stacks
	if record.StackID != "" {
		stacks = map[string]*Stack{record.StackID: m.Stacks[record.StackID]}
	}
	for _, stack := range stacks {
		if stack == nil {
			continue
		}
		if stack.StackName != record.StackName {
			continue
		}

		event, ok := createTerminalEvent(stack.Events, token)
		if !ok {
			continue
		}

		recovered := *record
		recovered.StackID = stack.StackID
		recovered.Completed = true
		recovered.Success = event.ResourceStatus == StackStatusCreateComplete
		recovered.Error = event.ResourceStatusReason
		recovered.ErrorCode = event.ErrorCode
		recovered.CompletedAt = event.Timestamp

		if !recovered.Success && recovered.ErrorCode == "" {
			recovered.ErrorCode = errInvalidParameter
		}

		return recovered, true
	}

	return CreateExecutionRecord{}, false
}

//nolint:wsl // Recovery copies one token record from immutable terminal event history.
func recoverExecutionRecordLocked(record *ExecutionRecord, token, operation string, stack *Stack) (ExecutionRecord, bool) {
	status := map[string]string{
		operationCreate: StackStatusCreateComplete,
		operationUpdate: StackStatusUpdateComplete,
		operationDelete: StackStatusDeleteComplete,
	}[operation]
	failedStatus := map[string]string{
		operationCreate: StackStatusCreateFailed,
		operationUpdate: StackStatusUpdateFailed,
		operationDelete: StackStatusDeleteFailed,
	}[operation]
	for index := len(stack.Events) - 1; index >= 0; index-- {
		event := stack.Events[index]
		if event.ClientRequestToken != token || event.ResourceStatus != status && event.ResourceStatus != failedStatus {
			continue
		}
		recovered := *record
		recovered.Completed = true
		recovered.Success = event.ResourceStatus == status
		recovered.ErrorCode = event.ErrorCode
		recovered.Error = event.ResourceStatusReason
		recovered.CompletedAt = event.Timestamp

		return recovered, true
	}

	return ExecutionRecord{}, false
}

func createTerminalEvent(events []StackEvent, token string) (StackEvent, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.ClientRequestToken != token {
			continue
		}

		if event.ResourceStatus == StackStatusCreateComplete || event.ResourceStatus == StackStatusCreateFailed {
			return event, true
		}
	}

	return StackEvent{}, false
}

// CompleteCreateExecution persists a CreateStack result for token replay.
//
//nolint:wsl // Completion and persistence are one atomic boundary.
func (m *MemoryStorage) CompleteCreateExecution(_ context.Context, token, stackID, errorCode, reason string, success bool) error {
	if err := m.ready(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.CreateExecutions[token]
	if !ok {
		return &Error{Code: "TokenNotFoundException", Message: fmt.Sprintf("client request token %q was not reserved", token)}
	}
	if record.Completed {
		return nil
	}
	record.StackID = stackID
	record.ErrorCode = errorCode
	record.Error = reason
	record.Success = success
	record.Completed = true
	record.CompletedAt = time.Now()
	m.CreateExecutions[token] = record

	return m.saveLocked()
}

// ReleaseCreateExecution removes a reservation that never created a stack.
//
//nolint:wsl // Release and persistence are one atomic boundary.
func (m *MemoryStorage) ReleaseCreateExecution(_ context.Context, token string) error {
	if err := m.ready(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.CreateExecutions[token]
	if !ok || record.Completed {
		return nil
	}
	before := m.snapshotLocked()
	delete(m.CreateExecutions, token)
	if err := m.saveLocked(); err != nil {
		m.restoreSnapshotLocked(before)

		return err
	}

	return nil
}

// GetStack returns a deep snapshot of a stack addressed by name or ARN.
func (m *MemoryStorage) GetStack(_ context.Context, stackRef string) (*Stack, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	arn, err := m.stackARNLocked(stackRef, false)
	if err != nil {
		return nil, err
	}

	return cloneStack(m.Stacks[arn]), nil
}

// CreateReviewStack creates the empty stack shell used by a CREATE change set.
//
//nolint:wsl // Review-shell creation keeps reservation and persistence together.
func (m *MemoryStorage) CreateReviewStack(_ context.Context, stackName string) (*Stack, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	if stackName == "" || !validStackName(stackName) {
		return nil, &Error{Code: errInvalidParameter, Message: msgStackNameRequired}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.LiveNameIndex[stackName]; exists {
		return nil, &Error{Code: errAlreadyExists, Message: "Stack already exists"}
	}
	stackID := generateStackID(stackName)
	now := time.Now()
	stack := &Stack{
		StackID:         stackID,
		StackName:       stackName,
		StackStatus:     StackStatusReviewInProgress,
		CreationTime:    now,
		LastUpdatedTime: now,
		Resources:       []StackResource{},
		Revision:        1,
		Executions:      make(map[string]ExecutionRecord),
	}
	appendJournalLocked(stack, "CreateChangeSet", "REVIEW", "")
	m.Stacks[stackID] = stack
	m.LiveNameIndex[stackName] = stackID
	if err := m.saveLocked(); err != nil {
		delete(m.Stacks, stackID)
		delete(m.LiveNameIndex, stackName)

		return nil, err
	}

	return cloneStack(stack), nil
}

// CreateChangeSet stores an immutable, revision-bound change-set review.
//
//nolint:cyclop,funlen,gocognit,wsl // Review creation validates identity, computes a diff, and persists one atomic record.
func (m *MemoryStorage) CreateChangeSet(ctx context.Context, req *CreateChangeSetRequest, evaluation *template.Evaluation) (*ChangeSet, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	if req == nil || req.StackName == "" || req.ChangeSetName == "" || evaluation == nil {
		return nil, &Error{Code: errInvalidParameter, Message: "StackName, ChangeSetName, and evaluated template are required"}
	}
	arn, err := m.stackARN(req.StackName)
	if err != nil {
		return nil, err
	}
	fingerprint, err := changeSetFingerprint(req, evaluation)
	if err != nil {
		return nil, err
	}
	var result *ChangeSet
	err = m.withStackLock(ctx, arn, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		stack := m.Stacks[arn]
		if stack == nil || stack.StackStatus == StackStatusDeleteComplete {
			return &Error{Code: errStackNotFound, Message: msgStackNotFound}
		}
		nested := false
		if current, ok := ctx.Value(stackOperationContextKey{}).(stackOperationContext); ok {
			nested = current.arn == arn
		}
		if stack.OperationLock != nil && !nested {
			return operationInProgressError()
		}
		if req.baseRevisionSet && stack.Revision != req.baseRevision {
			return &Error{Code: "StaleChangeSetException", Message: "Change set is stale because the stack revision changed"}
		}
		for _, existing := range m.ChangeSets {
			if existing.StackID != arn {
				continue
			}
			if existing.Name == req.ChangeSetName {
				if existing.ClientToken == req.ClientToken && existing.Fingerprint == fingerprint {
					result = cloneChangeSet(existing)

					return nil
				}

				return &Error{Code: errAlreadyExists, Message: "Change set already exists"}
			}
			if req.ClientToken != "" && existing.ClientToken == req.ClientToken {
				if existing.Fingerprint == fingerprint {
					result = cloneChangeSet(existing)

					return nil
				}

				return &Error{Code: errTokenAlreadyExists, Message: "client request token was reused for a different change set"}
			}
		}

		resourceChanges := calculateResourceChanges(stack, evaluation)
		status := ChangeSetStatusCreateComplete
		executionStatus := ChangeSetExecutionAvailable
		statusReason := req.preflightError
		if statusReason != "" {
			status = ChangeSetStatusFailed
			executionStatus = ChangeSetExecutionUnavailable
		} else if len(resourceChanges) == 0 && req.ChangeSetType != changeSetTypeCreate && !changeSetParameterChanges(stack, evaluation) {
			status = ChangeSetStatusFailed
			executionStatus = ChangeSetExecutionUnavailable
			statusReason = "The submitted information didn't contain changes. Submit different information to create a change set."
		}
		changeSet := &ChangeSet{
			ID:              generateChangeSetID(req.ChangeSetName),
			Name:            req.ChangeSetName,
			StackID:         stack.StackID,
			StackName:       stack.StackName,
			Description:     req.Description,
			ChangeSetType:   effectiveChangeSetType(req.ChangeSetType),
			Status:          status,
			StatusReason:    statusReason,
			ExecutionStatus: executionStatus,
			CreationTime:    time.Now(),
			ClientToken:     req.ClientToken,
			Fingerprint:     fingerprint,
			BaseRevision:    stack.Revision,
			TemplateBody:    req.TemplateBody,
			Parameters:      cloneStringMap(evaluation.Parameters),
			Imports:         cloneImports(req.imports),
			Evaluation:      cloneEvaluation(evaluation),
			ResourceChanges: cloneResourceChanges(resourceChanges),
			Options: ExecutionOptions{
				Capabilities:  append([]string(nil), req.Capabilities...),
				ResourceTypes: append([]string(nil), req.ResourceTypes...),
				RoleARN:       req.RoleARN,
				Tags:          cloneTags(req.Tags),
			},
			Metadata: &LifecycleMetadata{
				TemplateBody:     req.TemplateBody,
				Parameters:       cloneStringMap(evaluation.Parameters),
				Tags:             cloneTags(req.Tags),
				RoleARN:          req.RoleARN,
				ResourceTypes:    append([]string(nil), req.ResourceTypes...),
				NotificationARNs: append([]string(nil), req.NotificationARNs...),
			},
		}
		m.ChangeSets[changeSet.ID] = changeSet
		if err := m.saveLocked(); err != nil {
			delete(m.ChangeSets, changeSet.ID)

			return err
		}
		result = cloneChangeSet(changeSet)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// GetChangeSet returns an immutable snapshot addressed by stack and name or ID.
//
//nolint:wsl // Change-set lookup keeps optional stack resolution and snapshot cloning together.
func (m *MemoryStorage) GetChangeSet(_ context.Context, stackRef, changeSetRef string) (*ChangeSet, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	stackARN := ""
	if stackRef != "" {
		var err error
		stackARN, err = m.stackARNLocked(stackRef, false)
		if err != nil {
			return nil, err
		}
	}
	for _, changeSet := range m.ChangeSets {
		if (stackARN == "" || changeSet.StackID == stackARN) && (changeSet.ID == changeSetRef || changeSet.Name == changeSetRef) {
			return cloneChangeSet(changeSet), nil
		}
	}

	return nil, &Error{Code: "ChangeSetNotFoundException", Message: "Change set not found"}
}

// MarkChangeSetExecution records a terminal change-set execution and obsoletes siblings.
//
//nolint:wsl // Change-set terminal state and sibling invalidation are one atomic mutation.
func (m *MemoryStorage) MarkChangeSetExecution(ctx context.Context, stackRef, changeSetRef string, success bool, reason string) error {
	stackARN, err := m.stackARNForContext(ctx, stackRef)
	if err != nil {
		return err
	}

	return m.withStackLock(ctx, stackARN, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		var current *ChangeSet
		for _, candidate := range m.ChangeSets {
			if candidate.StackID == stackARN && (candidate.ID == changeSetRef || candidate.Name == changeSetRef) {
				current = candidate

				break
			}
		}
		if current == nil {
			return &Error{Code: "ChangeSetNotFoundException", Message: "Change set not found"}
		}
		before := m.snapshotLocked()
		if success {
			current.ExecutionStatus = ChangeSetExecutionComplete
			current.StatusReason = ""
			for _, sibling := range m.ChangeSets {
				if sibling.StackID == current.StackID && sibling.ID != current.ID && sibling.Status == ChangeSetStatusCreateComplete && sibling.ExecutionStatus == ChangeSetExecutionAvailable {
					sibling.Status = ChangeSetStatusObsolete
					sibling.ExecutionStatus = ChangeSetExecutionUnavailable
					sibling.StatusReason = "The change set was made obsolete by the execution of another change set."
				}
			}
		} else {
			current.Status = ChangeSetStatusFailed
			current.ExecutionStatus = ChangeSetExecutionUnavailable
			current.StatusReason = reason
		}

		if err := m.saveLocked(); err != nil {
			m.restoreSnapshotLocked(before)

			return err
		}

		return nil
	})
}

// SetEvaluatedState persists the canonical evaluated template state.
func (m *MemoryStorage) SetEvaluatedState(ctx context.Context, stackRef string, evaluation *template.Evaluation) error {
	return m.RunStackOperation(ctx, stackRef, func(_ context.Context, stack *Stack) error {
		stack.EvaluatedState = cloneEvaluation(evaluation)

		return nil
	})
}

// AppendStackEvent persists one event in the stack's durable event history.
//
//nolint:gocritic // StackEvent is intentionally passed by value as an event snapshot.
func (m *MemoryStorage) AppendStackEvent(ctx context.Context, stackRef string, event StackEvent) error {
	return m.withStackMutation(ctx, stackRef, true, func(stack *Stack) error {
		appendEventLocked(stack, event.ResourceStatus, event.ClientRequestToken, event)

		return nil
	})
}

// AppendStackJournal persists one durable lifecycle transition.
func (m *MemoryStorage) AppendStackJournal(ctx context.Context, stackRef, operation, status, reason string) error {
	return m.AppendStackJournalEntry(ctx, stackRef, StackJournalEntry{Operation: operation, Status: status, Reason: reason})
}

// AppendStackJournalEntry persists one detailed lifecycle transition.
//
//nolint:gocritic // The value is copied to preserve an immutable journal snapshot.
func (m *MemoryStorage) AppendStackJournalEntry(ctx context.Context, stackRef string, entry StackJournalEntry) error {
	return m.withStackMutation(ctx, stackRef, true, func(stack *Stack) error {
		entry.Revision = stack.Revision
		if entry.Timestamp.IsZero() {
			entry.Timestamp = time.Now()
		}

		entry.Properties = cloneAnyMap(entry.Properties)
		stack.Journal = append(stack.Journal, entry)

		return nil
	})
}

// ListStackEvents returns a snapshot of the stack's durable events.
//
//nolint:wsl // Filtering and deterministic ordering belong to one snapshot read.
func (m *MemoryStorage) ListStackEvents(ctx context.Context, stackRef, clientRequestToken string) ([]StackEvent, error) {
	stack, err := m.GetStack(ctx, stackRef)
	if err != nil {
		return nil, err
	}
	result := make([]StackEvent, 0, len(stack.Events))
	for index := range stack.Events {
		event := stack.Events[index]
		if clientRequestToken == "" || event.ClientRequestToken == clientRequestToken {
			result = append(result, event)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Timestamp.Equal(result[right].Timestamp) {
			return result[left].EventID > result[right].EventID
		}

		return result[left].Timestamp.After(result[right].Timestamp)
	})

	return result, nil
}

//nolint:wsl // Mutation validation, application, and persistence are one unit.
func (m *MemoryStorage) withStackMutation(ctx context.Context, stackRef string, allowDeleted bool, mutation func(*Stack) error) error {
	if err := m.ready(); err != nil {
		return err
	}
	arn, err := m.stackARN(stackRef)
	if allowDeleted {
		m.mu.RLock()
		arn, err = m.stackARNLocked(stackRef, false)
		m.mu.RUnlock()
	}
	if err != nil {
		return err
	}

	return m.withStackLock(ctx, arn, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		stack := m.Stacks[arn]
		if stack == nil || !allowDeleted && stack.StackStatus == StackStatusDeleteComplete {
			return &Error{Code: errStackNotFound, Message: msgStackNotFound}
		}
		before := m.snapshotLocked()
		if err := mutation(stack); err != nil {
			return err
		}
		stack.Revision++
		if err := m.saveLocked(); err != nil {
			m.restoreSnapshotLocked(before)

			return err
		}

		return nil
	})
}

// StageExports validates an export replacement without changing committed state.
//
//nolint:wsl // Export staging validates and normalizes all entries atomically.
func (m *MemoryStorage) StageExports(_ context.Context, stackRef string, exports []StackExport) (*ExportStage, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	arn, err := m.stackARNLocked(stackRef, true)
	if err != nil {
		return nil, err
	}
	stack := m.Stacks[arn]
	staged := cloneExports(exports)
	seen := make(map[string]struct{}, len(staged))
	for index := range staged {
		if staged[index].Name == "" {
			return nil, &Error{Code: errInvalidParameter, Message: "export name is required"}
		}
		if _, exists := seen[staged[index].Name]; exists {
			return nil, &Error{Code: errInvalidParameter, Message: "duplicate export name"}
		}
		seen[staged[index].Name] = struct{}{}
		staged[index].ExporterStackARN = arn
		if owner := m.exportOwnerLocked(staged[index].Name); owner != "" && owner != arn {
			return nil, &Error{Code: errAlreadyExists, Message: fmt.Sprintf("export %q already exists", staged[index].Name)}
		}
	}

	sort.Slice(staged, func(left, right int) bool { return staged[left].Name < staged[right].Name })

	stage := &ExportStage{ID: uuid.NewString(), StackARN: arn, BaseRevision: stack.Revision, Exports: staged}
	m.stagedExports[stage.ID] = stagedExportRecord{
		StackARN:     stage.StackARN,
		BaseRevision: stage.BaseRevision,
		Exports:      cloneExports(stage.Exports),
	}

	return cloneExportStage(stage), nil
}

// CommitExports atomically commits a previously staged export replacement.
//
//nolint:cyclop,funlen,wsl // Conflict checks and replacement must remain visibly atomic.
func (m *MemoryStorage) CommitExports(ctx context.Context, stage *ExportStage) error {
	if err := m.ready(); err != nil {
		return err
	}
	if stage == nil {
		return &Error{Code: errInvalidParameter, Message: "export stage is required"}
	}
	stageID := stage.ID
	m.mu.RLock()
	record, ok := m.stagedExports[stageID]
	m.mu.RUnlock()
	if !ok {
		return &Error{Code: errInvalidParameter, Message: "export stage is invalid"}
	}

	return m.withStackLock(ctx, record.StackARN, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		record, ok := m.stagedExports[stageID]
		if !ok {
			return &Error{Code: errInvalidParameter, Message: "export stage is invalid"}
		}
		stack := m.Stacks[record.StackARN]
		if stack == nil || stack.StackStatus == StackStatusDeleteComplete {
			return &Error{Code: errStackNotFound, Message: msgStackNotFound}
		}
		before := cloneStack(stack)
		if stack.Revision != record.BaseRevision {
			return &Error{Code: revisionConflictException, Message: "export stage revision is stale"}
		}

		existing := exportMap(stack.Exports)
		next := exportMap(record.Exports)
		for name, previous := range existing {
			current, stillExists := next[name]
			if stillExists && current.Value == previous.Value {
				continue
			}
			if importer := m.activeImporterLocked(record.StackARN, name); importer != "" {
				return &Error{Code: errExportInUse, Message: fmt.Sprintf("export %q is imported by %s", name, importer)}
			}
		}
		for name, export := range next {
			if owner := m.exportOwnerLocked(name); owner != "" && owner != record.StackARN {
				return &Error{Code: errAlreadyExists, Message: fmt.Sprintf("export %q already exists", name)}
			}
			export.ExporterStackARN = record.StackARN
			next[name] = export
		}

		stack.Exports = sortedExports(next)
		stack.Revision++
		appendJournalLocked(stack, "CommitExports", "COMMITTED", "")
		if err := m.saveLocked(); err != nil {
			m.Stacks[record.StackARN] = before

			return err
		}
		delete(m.stagedExports, stageID)

		return nil
	})
}

// GetExports returns a snapshot of committed exports for a stack.
//
//nolint:wsl // Export lookup and cloning form one read snapshot.
func (m *MemoryStorage) GetExports(_ context.Context, stackRef string) ([]StackExport, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	arn, err := m.stackARNLocked(stackRef, false)
	if err != nil {
		return nil, err
	}

	return cloneExports(m.Stacks[arn].Exports), nil
}

// ListExports returns all committed exports from live stacks in name order.
//
//nolint:wsl // Snapshot filtering and ordering form one read boundary.
func (m *MemoryStorage) ListExports(_ context.Context) ([]StackExport, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]StackExport, 0)

	for _, stack := range m.Stacks {
		if stack.StackStatus == StackStatusDeleteComplete {
			continue
		}

		result = append(result, stack.Exports...)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Name == result[right].Name {
			return result[left].ExporterStackARN < result[right].ExporterStackARN
		}

		return result[left].Name < result[right].Name
	})

	return cloneExports(result), nil
}

// UpdateStackMetadata updates request-owned stack metadata after lifecycle success.
func (m *MemoryStorage) UpdateStackMetadata(ctx context.Context, stackRef, templateBody string, parameters map[string]string) error {
	return m.RunStackOperation(ctx, stackRef, func(_ context.Context, stack *Stack) error {
		stack.TemplateBody = templateBody
		stack.Parameters = cloneStringMap(parameters)

		return nil
	})
}

// StageImports validates an import replacement without changing committed state.
//
//nolint:wsl // Import staging validates and normalizes every edge in one pass.
func (m *MemoryStorage) StageImports(_ context.Context, stackRef string, imports []StackImport) (*ImportStage, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	arn, err := m.stackARNLocked(stackRef, true)
	if err != nil {
		return nil, err
	}
	stack := m.Stacks[arn]
	staged := cloneImports(imports)
	seen := make(map[string]struct{}, len(staged))
	for index := range staged {
		if staged[index].ExportName == "" {
			return nil, &Error{Code: errInvalidParameter, Message: "import export name is required"}
		}
		if _, exists := seen[staged[index].ExportName]; exists {
			return nil, &Error{Code: errInvalidParameter, Message: "duplicate import name"}
		}
		seen[staged[index].ExportName] = struct{}{}
		export, ok := m.findExportLocked(staged[index].ExportName)
		if !ok {
			return nil, &Error{Code: "ExportNotFoundException", Message: fmt.Sprintf("export %q does not exist", staged[index].ExportName)}
		}
		staged[index].ExporterStackARN = export.ExporterStackARN
		staged[index].ImporterStackARN = arn
	}
	sort.Slice(staged, func(left, right int) bool { return staged[left].ExportName < staged[right].ExportName })

	stage := &ImportStage{ID: uuid.NewString(), StackARN: arn, BaseRevision: stack.Revision, Imports: staged}
	m.stagedImports[stage.ID] = stagedImportRecord{
		StackARN:     stage.StackARN,
		BaseRevision: stage.BaseRevision,
		Imports:      cloneImports(stage.Imports),
	}

	return cloneImportStage(stage), nil
}

// CommitImports atomically commits a previously staged import replacement.
//
//nolint:wsl // Revalidation and edge replacement must remain one transaction.
func (m *MemoryStorage) CommitImports(ctx context.Context, stage *ImportStage) error {
	if err := m.ready(); err != nil {
		return err
	}
	if stage == nil {
		return &Error{Code: errInvalidParameter, Message: "import stage is required"}
	}
	stageID := stage.ID
	m.mu.RLock()
	record, ok := m.stagedImports[stageID]
	m.mu.RUnlock()
	if !ok {
		return &Error{Code: errInvalidParameter, Message: "import stage is invalid"}
	}

	return m.withStackLock(ctx, record.StackARN, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		record, ok := m.stagedImports[stageID]
		if !ok {
			return &Error{Code: errInvalidParameter, Message: "import stage is invalid"}
		}
		stack := m.Stacks[record.StackARN]
		if stack == nil || stack.StackStatus == StackStatusDeleteComplete {
			return &Error{Code: errStackNotFound, Message: msgStackNotFound}
		}
		before := cloneStack(stack)
		if stack.Revision != record.BaseRevision {
			return &Error{Code: revisionConflictException, Message: "import stage revision is stale"}
		}
		imports := cloneImports(record.Imports)
		for index := range imports {
			export, ok := m.findExportLocked(imports[index].ExportName)
			if !ok {
				return &Error{Code: "ExportNotFoundException", Message: fmt.Sprintf("export %q does not exist", imports[index].ExportName)}
			}
			imports[index].ExporterStackARN = export.ExporterStackARN
			imports[index].ImporterStackARN = record.StackARN
		}
		stack.Imports = imports
		stack.Revision++
		appendJournalLocked(stack, "CommitImports", "COMMITTED", "")
		if err := m.saveLocked(); err != nil {
			m.Stacks[record.StackARN] = before

			return err
		}
		delete(m.stagedImports, stageID)

		return nil
	})
}

// GetImports returns a snapshot of committed import edges for a stack.
//
//nolint:wsl // Import retrieval resolves and clones one stack snapshot.
func (m *MemoryStorage) GetImports(_ context.Context, stackRef string) ([]StackImport, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	arn, err := m.stackARNLocked(stackRef, false)
	if err != nil {
		return nil, err
	}

	return cloneImports(m.Stacks[arn].Imports), nil
}

func (m *MemoryStorage) exportOwnerLocked(name string) string {
	for arn, stack := range m.Stacks {
		for _, export := range stack.Exports {
			if export.Name == name {
				return arn
			}
		}
	}

	return ""
}

func (m *MemoryStorage) findExportLocked(name string) (StackExport, bool) {
	for _, stack := range m.Stacks {
		for _, export := range stack.Exports {
			if export.Name == name {
				return export, true
			}
		}
	}

	return StackExport{}, false
}

//nolint:wsl // The graph scan intentionally keeps its filtering conditions together.
func (m *MemoryStorage) activeImporterLocked(exporterARN, exportName string) string {
	for arn, stack := range m.Stacks {
		if stack.StackStatus == StackStatusDeleteComplete {
			continue
		}
		for _, importEdge := range stack.Imports {
			if importEdge.ExporterStackARN == exporterARN && (exportName == "" || importEdge.ExportName == exportName) {
				return arn
			}
		}
	}

	return ""
}

// CreateStack creates a new stack.
//
//nolint:funlen,wsl // Stack reservation and metadata initialization are one atomic boundary.
func (m *MemoryStorage) CreateStack(_ context.Context, req *CreateStackRequest) (*Stack, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if req.StackName == "" {
		return nil, &Error{Code: errInvalidParameter, Message: msgStackNameRequired}
	}

	if !validStackName(req.StackName) {
		return nil, &Error{Code: errInvalidParameter, Message: "StackName contains invalid characters"}
	}
	if err := validateCreateRequest(*req); err != nil {
		return nil, err
	}

	if _, exists := m.LiveNameIndex[req.StackName]; exists {
		return nil, &Error{Code: errAlreadyExists, Message: "Stack already exists"}
	}

	if req.TemplateURL != "" {
		return nil, &Error{Code: errInvalidParameter, Message: msgTemplateURLUnsupported}
	}
	if req.TemplateBody == "" {
		return nil, &Error{Code: errInvalidParameter, Message: "TemplateBody must be specified"}
	}
	req.ClientRequestToken = executionToken(req.ClientRequestToken)

	stackID := generateStackID(req.StackName)
	now := time.Now()

	stack := &Stack{
		StackID:                     stackID,
		StackName:                   req.StackName,
		TemplateBody:                req.TemplateBody,
		Parameters:                  cloneStringMap(req.Parameters),
		StackStatus:                 StackStatusCreateInProgress,
		CreationTime:                now,
		LastUpdatedTime:             now,
		Resources:                   []StackResource{},
		Revision:                    1,
		Executions:                  make(map[string]ExecutionRecord),
		Tags:                        cloneTags(req.Tags),
		RoleARN:                     req.RoleARN,
		ResourceTypes:               append([]string(nil), req.ResourceTypes...),
		NotificationARNs:            append([]string(nil), req.NotificationARNs...),
		RollbackConfiguration:       cloneRollbackConfiguration(req.RollbackConfiguration),
		DeploymentConfig:            cloneDeploymentConfig(req.DeploymentConfig),
		StackPolicyBody:             req.StackPolicyBody,
		EnableTerminationProtection: req.EnableTerminationProtection,
		RetainExceptOnCreate:        req.RetainExceptOnCreate,
		TimeoutInMinutes:            req.TimeoutInMinutes,
		DisableValidation:           req.DisableValidation,
		DisableRollback:             effectiveDisableRollback(req.DisableRollback, req.DeploymentConfig),
		OnFailure:                   FailurePolicy(req.OnFailure),
	}
	appendJournalLocked(stack, "CreateStack", "IN_PROGRESS", "")
	appendEventLocked(stack, ResourceStatusCreateInProgress, req.ClientRequestToken)

	m.Stacks[stackID] = stack
	m.LiveNameIndex[req.StackName] = stackID
	previousExecution, hadExecution := m.CreateExecutions[req.ClientRequestToken]
	originalExecution := previousExecution
	if hadExecution && !previousExecution.Completed {
		previousExecution.StackID = stackID
		m.CreateExecutions[req.ClientRequestToken] = previousExecution
	}

	if err := m.saveLocked(); err != nil {
		delete(m.Stacks, stackID)
		delete(m.LiveNameIndex, req.StackName)
		if hadExecution {
			m.CreateExecutions[req.ClientRequestToken] = originalExecution
		} else {
			delete(m.CreateExecutions, req.ClientRequestToken)
		}

		return nil, err
	}

	return cloneStack(stack), nil
}

// MarkCreateFailed persists a failed create and optionally tombstones the stack
// when the request selected the DELETE failure policy.
//
//nolint:wsl // Failure state, tombstone state, and persistence are one boundary.
func (m *MemoryStorage) MarkCreateFailed(ctx context.Context, stackRef, reason, errorCode string, deleteOnFailure bool, token string) error {
	if err := m.ready(); err != nil {
		return err
	}
	arn, err := m.stackARN(stackRef)
	if err != nil {
		return err
	}

	return m.withStackLock(ctx, arn, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		stack := m.Stacks[arn]
		if stack == nil {
			return &Error{Code: errStackNotFound, Message: msgStackNotFound}
		}
		before := m.snapshotLocked()
		stack.StackStatus = StackStatusCreateFailed
		stack.StackStatusReason = reason
		stack.LastUpdatedTime = time.Now()
		stack.Revision++
		appendJournalLocked(stack, "CreateStack", "FAILED", reason)
		appendEventLocked(stack, StackStatusCreateFailed, token, StackEvent{ResourceStatus: StackStatusCreateFailed, ResourceStatusReason: reason, ErrorCode: errorCode, ClientRequestToken: token})
		if deleteOnFailure {
			delete(m.LiveNameIndex, stack.StackName)
			stack.StackStatus = StackStatusDeleteComplete
			stack.DeletionTime = time.Now()
			stack.Revision++
			appendJournalLocked(stack, "CreateStack", "DELETED", reason)
			appendEventLocked(stack, StackStatusDeleteComplete, token, StackEvent{ResourceStatus: StackStatusDeleteComplete, ResourceStatusReason: reason, ClientRequestToken: token})
		}
		if err := m.saveLocked(); err != nil {
			m.restoreSnapshotLocked(before)

			return err
		}

		return nil
	})
}

// DeleteStack deletes a stack.
//
//nolint:wsl // Deletion checks graph constraints before applying the tombstone.
func (m *MemoryStorage) DeleteStack(ctx context.Context, stackRef string) error {
	if err := m.ready(); err != nil {
		return err
	}
	arn, err := m.stackARN(stackRef)
	if err != nil {
		return err
	}

	return m.withStackLock(ctx, arn, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()

		stack := m.Stacks[arn]
		if stack == nil || stack.StackStatus == StackStatusDeleteComplete {
			return &Error{Code: errStackNotFound, Message: msgStackNotFound}
		}
		if importer := m.activeImporterLocked(arn, ""); importer != "" {
			return &Error{Code: errExportInUse, Message: fmt.Sprintf("stack has an active importer %s", importer)}
		}
		before := m.snapshotLocked()

		delete(m.LiveNameIndex, stack.StackName)
		stack.StackStatus = StackStatusDeleteComplete
		stack.DeletionTime = time.Now()
		stack.Exports = nil
		stack.Imports = nil
		stack.Revision++
		appendJournalLocked(stack, "DeleteStack", "COMMITTED", "")
		appendEventLocked(stack, ResourceStatusDeleteComplete, "DeleteStack")
		if err := m.saveLocked(); err != nil {
			m.restoreSnapshotLocked(before)

			return err
		}

		return nil
	})
}

// ValidateStackDeletion rejects deletion while another live stack imports an export.
//
//nolint:wsl // Deletion validation keeps graph lookup and rejection together.
func (m *MemoryStorage) ValidateStackDeletion(_ context.Context, stackRef string) error {
	if err := m.ready(); err != nil {
		return err
	}
	arn, err := m.stackARN(stackRef)
	if err != nil {
		return err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if importer := m.activeImporterLocked(arn, ""); importer != "" {
		return &Error{Code: errExportInUse, Message: fmt.Sprintf("stack has an active importer %s", importer)}
	}

	return nil
}

// ValidateExportMutation rejects changing or removing an export with active importers.
//
//nolint:wsl // Export validation keeps the previous and proposed export sets together.
func (m *MemoryStorage) ValidateExportMutation(_ context.Context, stackRef string, evaluation *template.Evaluation) error {
	if err := m.ready(); err != nil {
		return err
	}
	arn, err := m.stackARN(stackRef)
	if err != nil {
		return err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	stack := m.Stacks[arn]
	if stack == nil {
		return &Error{Code: errStackNotFound, Message: msgStackNotFound}
	}
	next := make(map[string]string)
	if evaluation != nil {
		for _, output := range evaluation.Outputs {
			if output.ExportName != "" {
				next[output.ExportName] = fmt.Sprint(output.Value)
			}
		}
	}
	for _, export := range stack.Exports {
		value, exists := next[export.Name]
		if exists && value == export.Value {
			continue
		}
		if importer := m.activeImporterLocked(arn, export.Name); importer != "" {
			return &Error{Code: errExportInUse, Message: fmt.Sprintf("export %q is imported by %s", export.Name, importer)}
		}
	}

	return nil
}

// FinalizeDeletedStack commits a successful lifecycle delete as a tombstone and
// releases the live name and export/import graph edges atomically.
//
//nolint:wsl // Tombstone, graph cleanup, and persistence are one atomic mutation.
func (m *MemoryStorage) FinalizeDeletedStack(ctx context.Context, stackRef string, evaluation *template.Evaluation, resources []StackResource, event *StackEvent, metadata ...LifecycleMetadata) error {
	if err := m.ready(); err != nil {
		return err
	}
	arn, err := m.stackARN(stackRef)
	if err != nil {
		return err
	}

	return m.withStackLock(ctx, arn, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		stack := m.Stacks[arn]
		if stack == nil || stack.StackStatus == StackStatusDeleteComplete {
			return &Error{Code: errStackNotFound, Message: msgStackNotFound}
		}
		if importer := m.activeImporterLocked(arn, ""); importer != "" {
			return &Error{Code: errExportInUse, Message: fmt.Sprintf("stack has an active importer %s", importer)}
		}
		before := m.snapshotLocked()
		delete(m.LiveNameIndex, stack.StackName)
		stack.StackStatus = StackStatusDeleteComplete
		stack.StackStatusReason = ""
		stack.EvaluatedState = cloneEvaluation(evaluation)
		stack.Resources = append([]StackResource(nil), resources...)
		stack.Exports = nil
		stack.Imports = nil
		if len(metadata) > 0 {
			applyLifecycleMetadata(stack, metadata[0])
		}
		stack.DeletionTime = time.Now()
		stack.LastUpdatedTime = time.Now()
		stack.Revision++
		appendJournalLocked(stack, "FinalizeDeletedStack", "COMMITTED", "")
		if event != nil {
			appendEventLocked(stack, event.ResourceStatus, event.ClientRequestToken, *event)
		}
		if err := m.saveLocked(); err != nil {
			m.restoreSnapshotLocked(before)

			return err
		}

		return nil
	})
}

// DescribeStacks describes stacks.
//
//nolint:wsl // The name-index and ARN paths produce one deterministic snapshot.
func (m *MemoryStorage) DescribeStacks(_ context.Context, stackRef string) ([]*Stack, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	if stackRef != "" {
		arn, err := m.stackARNLocked(stackRef, false)
		if err != nil {
			return nil, err
		}

		return []*Stack{cloneStack(m.Stacks[arn])}, nil
	}

	arns := make([]string, 0, len(m.LiveNameIndex))
	for _, arn := range m.LiveNameIndex {
		arns = append(arns, arn)
	}
	sort.Strings(arns)
	result := make([]*Stack, 0, len(arns))
	for _, arn := range arns {
		result = append(result, cloneStack(m.Stacks[arn]))
	}

	return result, nil
}

// ListStacks lists stacks with optional status filter.
//
//nolint:wsl // List filtering and deterministic ordering form one read snapshot.
func (m *MemoryStorage) ListStacks(_ context.Context, statusFilter []string) ([]*Stack, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*Stack, 0, len(m.Stacks))

	for _, stack := range m.Stacks {
		if len(statusFilter) > 0 {
			if !containsStatus(statusFilter, stack.StackStatus) {
				continue
			}
		}

		result = append(result, cloneStack(stack))
	}
	sort.Slice(result, func(left, right int) bool { return result[left].StackID < result[right].StackID })

	return result, nil
}

// UpdateStack updates a stack.
//
//nolint:wsl // Update applies state, journal, event, and persistence together.
func (m *MemoryStorage) UpdateStack(ctx context.Context, req *UpdateStackRequest) (*Stack, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	arn, err := m.stackARN(req.StackName)
	if err != nil {
		return nil, err
	}
	var result *Stack
	err = m.withStackLock(ctx, arn, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()

		stack := m.Stacks[arn]
		if stack == nil || stack.StackStatus == StackStatusDeleteComplete {
			return &Error{Code: errStackNotFound, Message: msgStackNotFound}
		}
		if req.TemplateBody == "" {
			return &Error{Code: errInvalidParameter, Message: "TemplateBody is required for update"}
		}
		before := cloneStack(stack)

		now := time.Now()
		stack.TemplateBody = req.TemplateBody
		stack.LastUpdatedTime = now
		stack.StackStatus = StackStatusUpdateComplete
		if req.Parameters != nil {
			stack.Parameters = cloneStringMap(req.Parameters)
		}
		stack.Resources = parseTemplateResources(req.TemplateBody, stack.StackID, stack.StackName)
		applyLifecycleMetadata(stack, LifecycleMetadata{
			TemplateBody:                req.TemplateBody,
			Parameters:                  req.Parameters,
			Tags:                        req.Tags,
			RoleARN:                     req.RoleARN,
			ResourceTypes:               req.ResourceTypes,
			NotificationARNs:            req.NotificationARNs,
			RollbackConfiguration:       req.RollbackConfiguration,
			DeploymentConfig:            req.DeploymentConfig,
			StackPolicyBody:             req.StackPolicyBody,
			StackPolicyDuringUpdateBody: req.StackPolicyDuringUpdateBody,
			RetainExceptOnCreate:        req.RetainExceptOnCreate,
			DisableValidation:           req.DisableValidation,
		})
		stack.Revision++
		appendJournalLocked(stack, "UpdateStack", "COMMITTED", "")
		appendEventLocked(stack, StackStatusUpdateComplete, "UpdateStack")
		if err := m.saveLocked(); err != nil {
			m.Stacks[arn] = before

			return err
		}
		result = cloneStack(stack)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// DescribeStackResources describes stack resources.
//
//nolint:wsl // Resource filtering returns independent snapshots from one read lock.
func (m *MemoryStorage) DescribeStackResources(_ context.Context, stackRef, logicalResourceID string) ([]*StackResource, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	arn, err := m.stackARNLocked(stackRef, false)
	if err != nil {
		return nil, err
	}
	stack := m.Stacks[arn]

	if logicalResourceID != "" {
		for i := range stack.Resources {
			if stack.Resources[i].LogicalResourceID == logicalResourceID {
				resource := stack.Resources[i]

				return []*StackResource{&resource}, nil
			}
		}

		return []*StackResource{}, nil
	}

	result := make([]*StackResource, len(stack.Resources))
	for i := range stack.Resources {
		resource := stack.Resources[i]
		result[i] = &resource
	}

	return result, nil
}

// GetTemplate gets the template of a stack.
func (m *MemoryStorage) GetTemplate(_ context.Context, stackRef string) (string, error) {
	if err := m.ready(); err != nil {
		return "", err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	arn, err := m.stackARNLocked(stackRef, false)
	if err != nil {
		return "", err
	}

	return m.Stacks[arn].TemplateBody, nil
}

// ValidateTemplate validates a template.
func (m *MemoryStorage) ValidateTemplate(_ context.Context, templateBody string) (*TemplateValidationResult, error) {
	if templateBody == "" {
		return nil, &Error{Code: errInvalidParameter, Message: "TemplateBody is required"}
	}

	if _, err := template.Parse(templateBody); err != nil {
		return nil, &Error{Code: errInvalidParameter, Message: err.Error()}
	}

	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(templateBody), &parsed); err != nil {
		return nil, &Error{Code: errInvalidParameter, Message: "Template format error: " + err.Error()}
	}

	result := &TemplateValidationResult{
		Parameters:   []TemplateParameter{},
		Capabilities: []string{},
	}

	if desc, ok := parsed["Description"].(string); ok {
		result.Description = desc
	}

	if params, ok := parsed["Parameters"].(map[string]any); ok {
		for key, value := range params {
			param := parseTemplateParameter(key, value)
			result.Parameters = append(result.Parameters, param)
		}
	}

	return result, nil
}

// parseTemplateParameter extracts a TemplateParameter from a template parameter definition.
//
//nolint:wsl // Parameter metadata extraction intentionally stays grouped by field.
func parseTemplateParameter(key string, value any) TemplateParameter {
	param := TemplateParameter{
		ParameterKey: key,
	}

	paramDef, ok := value.(map[string]any)
	if !ok {
		return param
	}

	if defaultVal, ok := paramDef["Default"].(string); ok {
		param.DefaultValue = defaultVal
	}

	if desc, ok := paramDef["Description"].(string); ok {
		param.Description = desc
	}

	if desc, ok := paramDef["ConstraintDescription"].(string); ok {
		param.ConstraintDescription = desc
	}

	if values, ok := paramDef["AllowedValues"].([]any); ok {
		for _, value := range values {
			param.AllowedValues = append(param.AllowedValues, fmt.Sprint(value))
		}
	}
	if pattern, ok := paramDef["AllowedPattern"].(string); ok {
		param.AllowedPattern = pattern
	}
	param.MinLength = summaryIntPointer(paramDef["MinLength"])
	param.MaxLength = summaryIntPointer(paramDef["MaxLength"])
	param.MinValue = summaryFloatPointer(paramDef["MinValue"])
	param.MaxValue = summaryFloatPointer(paramDef["MaxValue"])

	if paramType, ok := paramDef["Type"].(string); ok {
		param.ParameterType = paramType
	}

	if noEcho, ok := paramDef["NoEcho"].(bool); ok {
		param.NoEcho = noEcho
	}

	return param
}

// Helper functions.

func generateStackID(stackName string) string {
	return StackIdentity{
		Partition: "aws",
		Region:    "us-east-1",
		AccountID: "123456789012",
		StackName: stackName,
		UUID:      uuid.New().String(),
	}.ARN()
}

func parseTemplateResources(templateBody, stackID, stackName string) []StackResource {
	var template map[string]any
	if err := json.Unmarshal([]byte(templateBody), &template); err != nil {
		return []StackResource{}
	}

	resources := []StackResource{}
	now := time.Now()

	if resourcesSection, ok := template["Resources"].(map[string]any); ok {
		for logicalID, resourceDef := range resourcesSection {
			resource := StackResource{
				LogicalResourceID:  logicalID,
				PhysicalResourceID: generatePhysicalResourceID(logicalID),
				ResourceStatus:     ResourceStatusCreateComplete,
				Timestamp:          now,
				StackID:            stackID,
				StackName:          stackName,
			}

			if def, ok := resourceDef.(map[string]any); ok {
				if resourceType, ok := def["Type"].(string); ok {
					resource.ResourceType = resourceType
				}
			}

			resources = append(resources, resource)
		}
	}

	return resources
}

func generatePhysicalResourceID(logicalID string) string {
	return logicalID + "-" + uuid.New().String()[:8]
}

func containsStatus(statusFilter []string, status string) bool {
	return slices.Contains(statusFilter, status)
}

func appendJournalLocked(stack *Stack, operation, status, reason string) {
	stack.Journal = append(stack.Journal, StackJournalEntry{
		Revision:  stack.Revision,
		Operation: operation,
		Status:    status,
		Reason:    reason,
		Timestamp: time.Now(),
	})
}

//nolint:wsl // Event defaults and bounded append are one snapshot mutation.
func appendEventLocked(stack *Stack, status, token string, provided ...StackEvent) {
	event := StackEvent{}
	if len(provided) > 0 {
		event = provided[0]
	}
	if event.EventID == "" {
		event.EventID = uuid.NewString()
	}
	if event.StackID == "" {
		event.StackID = stack.StackID
	}
	if event.StackName == "" {
		event.StackName = stack.StackName
	}
	if event.ResourceStatus == "" {
		event.ResourceStatus = status
	}
	if event.ClientRequestToken == "" {
		event.ClientRequestToken = token
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	for index := range stack.Events {
		if stack.Events[index].EventID == event.EventID {
			return
		}
	}
	stack.Events = append(stack.Events, event)
	if len(stack.Events) > 1000 {
		stack.Events = stack.Events[len(stack.Events)-1000:]
	}
}

func copyStackState(destination, source *Stack) {
	destination.TemplateBody = source.TemplateBody
	destination.Parameters = cloneStringMap(source.Parameters)
	destination.StackStatus = source.StackStatus
	destination.StackStatusReason = source.StackStatusReason
	destination.CreationTime = source.CreationTime
	destination.LastUpdatedTime = source.LastUpdatedTime
	destination.DeletionTime = source.DeletionTime
	destination.Resources = append([]StackResource(nil), source.Resources...)
	destination.EvaluatedState = cloneEvaluation(source.EvaluatedState)
	destination.Journal = cloneJournal(source.Journal)
	destination.Events = append([]StackEvent(nil), source.Events...)
	destination.Executions = cloneExecutions(source.Executions)
	destination.Exports = cloneExports(source.Exports)
	destination.Imports = cloneImports(source.Imports)
	destination.Tags = cloneTags(source.Tags)
	destination.RoleARN = source.RoleARN
	destination.ResourceTypes = append([]string(nil), source.ResourceTypes...)
	destination.NotificationARNs = append([]string(nil), source.NotificationARNs...)
	destination.RollbackConfiguration = cloneRollbackConfiguration(source.RollbackConfiguration)
	destination.DeploymentConfig = cloneDeploymentConfig(source.DeploymentConfig)
	destination.StackPolicyBody = source.StackPolicyBody
	destination.StackPolicyDuringUpdateBody = source.StackPolicyDuringUpdateBody
	destination.EnableTerminationProtection = source.EnableTerminationProtection
	destination.RetainExceptOnCreate = source.RetainExceptOnCreate
	destination.TimeoutInMinutes = source.TimeoutInMinutes
	destination.DisableValidation = source.DisableValidation
	destination.DisableRollback = source.DisableRollback
	destination.OnFailure = source.OnFailure
}

//nolint:wsl // Cloning a stack intentionally copies every mutable nested field.
func cloneStack(stack *Stack) *Stack {
	if stack == nil {
		return nil
	}
	clone := *stack
	clone.Parameters = cloneStringMap(stack.Parameters)
	clone.Resources = append([]StackResource(nil), stack.Resources...)
	clone.Journal = cloneJournal(stack.Journal)
	clone.Events = append([]StackEvent(nil), stack.Events...)
	clone.Executions = cloneExecutions(stack.Executions)
	clone.Exports = cloneExports(stack.Exports)
	clone.Imports = cloneImports(stack.Imports)
	clone.Tags = cloneTags(stack.Tags)
	clone.ResourceTypes = append([]string(nil), stack.ResourceTypes...)
	clone.NotificationARNs = append([]string(nil), stack.NotificationARNs...)
	clone.RollbackConfiguration = cloneRollbackConfiguration(stack.RollbackConfiguration)
	clone.DeploymentConfig = cloneDeploymentConfig(stack.DeploymentConfig)
	clone.StackPolicyDuringUpdateBody = stack.StackPolicyDuringUpdateBody
	if stack.OperationLock != nil {
		lock := *stack.OperationLock
		clone.OperationLock = &lock
	}
	clone.EvaluatedState = cloneEvaluation(stack.EvaluatedState)

	return &clone
}

func cloneTags(tags []Tag) []Tag {
	return append([]Tag(nil), tags...)
}

//nolint:wsl // Configuration cloning keeps the nil check and copy together.
func cloneRollbackConfiguration(config *RollbackConfiguration) *RollbackConfiguration {
	if config == nil {
		return nil
	}
	clone := *config
	clone.RollbackTriggers = append([]RollbackTrigger(nil), config.RollbackTriggers...)

	return &clone
}

//nolint:wsl // Configuration cloning keeps the nil check and copy together.
func cloneDeploymentConfig(config *DeploymentConfig) *DeploymentConfig {
	if config == nil {
		return nil
	}
	clone := *config

	return &clone
}

//nolint:wsl // Metadata cloning copies every mutable nested field together.
func cloneLifecycleMetadata(metadata *LifecycleMetadata) *LifecycleMetadata {
	if metadata == nil {
		return nil
	}
	clone := *metadata
	clone.Parameters = cloneStringMap(metadata.Parameters)
	clone.Tags = cloneTags(metadata.Tags)
	clone.ResourceTypes = append([]string(nil), metadata.ResourceTypes...)
	clone.NotificationARNs = append([]string(nil), metadata.NotificationARNs...)
	clone.RollbackConfiguration = cloneRollbackConfiguration(metadata.RollbackConfiguration)
	clone.DeploymentConfig = cloneDeploymentConfig(metadata.DeploymentConfig)

	return &clone
}

//nolint:cyclop,gocritic,wsl // Metadata application copies the complete lifecycle contract.
func applyLifecycleMetadata(stack *Stack, metadata LifecycleMetadata) {
	if metadata.TemplateBody != "" {
		stack.TemplateBody = metadata.TemplateBody
	}
	if metadata.Parameters != nil {
		stack.Parameters = cloneStringMap(metadata.Parameters)
	}
	if metadata.Tags != nil {
		stack.Tags = cloneTags(metadata.Tags)
	}
	if metadata.RoleARN != "" {
		stack.RoleARN = metadata.RoleARN
	}
	if metadata.ResourceTypes != nil {
		stack.ResourceTypes = append([]string(nil), metadata.ResourceTypes...)
	}
	if metadata.NotificationARNs != nil {
		stack.NotificationARNs = append([]string(nil), metadata.NotificationARNs...)
	}
	if metadata.RollbackConfiguration != nil {
		stack.RollbackConfiguration = cloneRollbackConfiguration(metadata.RollbackConfiguration)
	}
	if metadata.DeploymentConfig != nil {
		stack.DeploymentConfig = cloneDeploymentConfig(metadata.DeploymentConfig)
	}
	if metadata.StackPolicyBody != "" {
		stack.StackPolicyBody = metadata.StackPolicyBody
	}
	if metadata.StackPolicyDuringUpdateBody != "" {
		stack.StackPolicyDuringUpdateBody = metadata.StackPolicyDuringUpdateBody
	}
	if metadata.EnableTerminationProtection {
		stack.EnableTerminationProtection = true
	}
	stack.RetainExceptOnCreate = metadata.RetainExceptOnCreate
	if metadata.TimeoutInMinutes != 0 {
		stack.TimeoutInMinutes = metadata.TimeoutInMinutes
	}
	stack.DisableValidation = metadata.DisableValidation
	stack.DisableRollback = metadata.DisableRollback
	if metadata.DeploymentConfig != nil {
		stack.DisableRollback = stack.DisableRollback || metadata.DeploymentConfig.DisableRollback
	}
	if metadata.OnFailure != "" {
		stack.OnFailure = metadata.OnFailure
	}
}

//nolint:wsl // Execution records are copied in one deterministic map traversal.
func cloneExecutions(executions map[string]ExecutionRecord) map[string]ExecutionRecord {
	if executions == nil {
		return nil
	}
	clone := make(map[string]ExecutionRecord, len(executions))
	for token := range executions {
		clone[token] = executions[token]
	}

	return clone
}

//nolint:gocritic,wsl // Cloning records intentionally copies each map value.
func cloneCreateExecutions(executions map[string]CreateExecutionRecord) map[string]CreateExecutionRecord {
	if executions == nil {
		return nil
	}
	clone := make(map[string]CreateExecutionRecord, len(executions))
	for token, execution := range executions {
		clone[token] = execution
	}

	return clone
}

func cloneStacks(stacks map[string]*Stack) map[string]*Stack {
	clone := make(map[string]*Stack, len(stacks))
	for arn, stack := range stacks {
		clone[arn] = cloneStack(stack)
	}

	return clone
}

//nolint:wsl // Change-set map cloning is one deterministic traversal.
func cloneChangeSets(changeSets map[string]*ChangeSet) map[string]*ChangeSet {
	if changeSets == nil {
		return nil
	}
	clone := make(map[string]*ChangeSet, len(changeSets))
	for id, changeSet := range changeSets {
		clone[id] = cloneChangeSet(changeSet)
	}

	return clone
}

//nolint:wsl // Change-set cloning copies all nested review state in one boundary.
func cloneChangeSet(changeSet *ChangeSet) *ChangeSet {
	if changeSet == nil {
		return nil
	}
	clone := *changeSet
	clone.Parameters = cloneStringMap(changeSet.Parameters)
	clone.Imports = cloneImports(changeSet.Imports)
	clone.Evaluation = cloneEvaluation(changeSet.Evaluation)
	clone.ResourceChanges = cloneResourceChanges(changeSet.ResourceChanges)
	clone.Options = cloneExecutionOptions(&changeSet.Options)
	clone.Metadata = cloneLifecycleMetadata(changeSet.Metadata)

	return &clone
}

func cloneResourceChanges(changes []ResourceChange) []ResourceChange {
	return append([]ResourceChange(nil), changes...)
}

//nolint:wsl // Option cloning copies each mutable slice and configuration together.
func cloneExecutionOptions(options *ExecutionOptions) ExecutionOptions {
	if options == nil {
		return ExecutionOptions{}
	}
	clone := *options
	clone.Capabilities = append([]string(nil), options.Capabilities...)
	clone.RetainResources = append([]string(nil), options.RetainResources...)
	clone.ResourceTypes = append([]string(nil), options.ResourceTypes...)
	clone.Tags = cloneTags(options.Tags)
	clone.DeploymentConfig = cloneDeploymentConfig(options.DeploymentConfig)
	clone.RollbackConfiguration = cloneRollbackConfiguration(options.RollbackConfiguration)

	return clone
}

//nolint:wsl // Cloning staged records keeps allocation and traversal together.
func cloneStagedExports(records map[string]stagedExportRecord) map[string]stagedExportRecord {
	if records == nil {
		return nil
	}
	clone := make(map[string]stagedExportRecord, len(records))
	for id, record := range records {
		record.Exports = cloneExports(record.Exports)
		clone[id] = record
	}

	return clone
}

//nolint:wsl // Cloning staged records keeps allocation and traversal together.
func cloneStagedImports(records map[string]stagedImportRecord) map[string]stagedImportRecord {
	if records == nil {
		return nil
	}
	clone := make(map[string]stagedImportRecord, len(records))
	for id, record := range records {
		record.Imports = cloneImports(record.Imports)
		clone[id] = record
	}

	return clone
}

func cloneJournal(entries []StackJournalEntry) []StackJournalEntry {
	clone := make([]StackJournalEntry, len(entries))
	for index := range entries {
		clone[index] = entries[index]
		clone[index].Properties = cloneAnyMap(entries[index].Properties)
		clone[index].Capabilities = append([]string(nil), entries[index].Capabilities...)
		clone[index].Attributes = cloneAnyMap(entries[index].Attributes)
	}

	return clone
}

//nolint:wsl // Cloning maps is deliberately kept as one compact traversal.
func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}

	return clone
}

//nolint:wsl // Evaluation cloning traverses each independent container.
func cloneEvaluation(evaluation *template.Evaluation) *template.Evaluation {
	if evaluation == nil {
		return nil
	}
	clone := &template.Evaluation{
		Parameters: cloneStringMap(evaluation.Parameters),
		Conditions: make(map[string]bool, len(evaluation.Conditions)),
		Resources:  make(map[string]template.Resource, len(evaluation.Resources)),
		Outputs:    make(map[string]template.Output, len(evaluation.Outputs)),
	}
	for name, value := range evaluation.Conditions {
		clone.Conditions[name] = value
	}
	for name, resource := range evaluation.Resources {
		resource.Properties = cloneAnyMap(resource.Properties)
		resource.DependsOn = append([]string(nil), resource.DependsOn...)
		resource.Dependencies = append([]string(nil), resource.Dependencies...)
		clone.Resources[name] = resource
	}
	for name, output := range evaluation.Outputs {
		output.Value = cloneAny(output.Value)
		clone.Outputs[name] = output
	}

	return clone
}

func cloneAny(value any) any {
	if value == nil {
		return nil
	}

	return cloneAnyValue(reflect.ValueOf(value)).Interface()
}

//nolint:exhaustive,wsl // Reflection branches keep each mutable container clone independent.
func cloneAnyValue(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}

	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		clone := reflect.New(value.Type()).Elem()
		clone.Set(cloneAnyValue(value.Elem()))

		return clone
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		clone := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			clone.SetMapIndex(cloneAnyValue(iter.Key()), cloneAnyValue(iter.Value()))
		}

		return clone
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		clone := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			clone.Index(index).Set(cloneAnyValue(value.Index(index)))
		}

		return clone
	case reflect.Array:
		clone := reflect.New(value.Type()).Elem()
		for index := 0; index < value.Len(); index++ {
			clone.Index(index).Set(cloneAnyValue(value.Index(index)))
		}

		return clone
	default:
		return value
	}
}

//nolint:wsl // Recursive map cloning keeps the copy operation self-contained.
func cloneAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	clone := make(map[string]any, len(values))
	for key, value := range values {
		clone[key] = cloneAny(value)
	}

	return clone
}

func cloneExports(exports []StackExport) []StackExport {
	return append([]StackExport(nil), exports...)
}

func cloneImports(imports []StackImport) []StackImport {
	return append([]StackImport(nil), imports...)
}

func cloneExportStage(stage *ExportStage) *ExportStage {
	clone := *stage
	clone.Exports = cloneExports(stage.Exports)

	return &clone
}

func cloneImportStage(stage *ImportStage) *ImportStage {
	clone := *stage
	clone.Imports = cloneImports(stage.Imports)

	return &clone
}

func generateChangeSetID(name string) string {
	return "arn:aws:cloudformation:us-east-1:123456789012:changeSet/" + name + "/" + uuid.NewString()
}

func effectiveChangeSetType(value string) string {
	if value == "" {
		return "UPDATE"
	}

	return value
}

//nolint:wsl // Fingerprinting serializes the complete immutable review input.
func changeSetFingerprint(req *CreateChangeSetRequest, evaluation *template.Evaluation) (string, error) {
	value := struct {
		StackName        string
		ChangeSetName    string
		ChangeSetType    string
		Description      string
		TemplateBody     string
		UsePrevious      bool
		Evaluation       *template.Evaluation
		Capabilities     []string
		ResourceTypes    []string
		RoleARN          string
		Tags             []Tag
		NotificationARNs []string
	}{
		StackName:        req.StackName,
		ChangeSetName:    req.ChangeSetName,
		ChangeSetType:    effectiveChangeSetType(req.ChangeSetType),
		Description:      req.Description,
		TemplateBody:     req.TemplateBody,
		UsePrevious:      req.UsePreviousTemplate,
		Evaluation:       cloneEvaluation(evaluation),
		Capabilities:     append([]string(nil), req.Capabilities...),
		ResourceTypes:    append([]string(nil), req.ResourceTypes...),
		RoleARN:          req.RoleARN,
		Tags:             cloneTags(req.Tags),
		NotificationARNs: append([]string(nil), req.NotificationARNs...),
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("fingerprint change set request: %w", err)
	}
	hash := sha256.Sum256(data)

	return hex.EncodeToString(hash[:]), nil
}

//nolint:wsl // Resource diff traversal keeps action classification together.
func calculateResourceChanges(stack *Stack, evaluation *template.Evaluation) []ResourceChange {
	oldEvaluation := stack.EvaluatedState
	if oldEvaluation == nil {
		oldEvaluation = &template.Evaluation{Resources: map[string]template.Resource{}}
	}
	oldResources := stackResourceMap(stack.Resources)
	changes := make([]ResourceChange, 0)
	for logicalID, definition := range evaluation.Resources {
		oldDefinition, existed := oldEvaluation.Resources[logicalID]
		oldResource := oldResources[logicalID]
		if !existed {
			changes = append(changes, ResourceChange{Action: ChangeActionAdd, LogicalResourceID: logicalID, ResourceType: definition.Type, Replacement: ReplacementFalse})

			continue
		}
		if oldDefinition.Type != definition.Type {
			changes = append(changes, ResourceChange{Action: ChangeActionModify, LogicalResourceID: logicalID, PhysicalResourceID: oldResource.PhysicalResourceID, ResourceType: definition.Type, Replacement: ReplacementTrue})

			continue
		}
		adapter, found := resource.DefaultRegistry().Get(definition.Type)
		if !found {
			if !reflect.DeepEqual(oldDefinition, definition) {
				changes = append(changes, ResourceChange{Action: ChangeActionModify, LogicalResourceID: logicalID, PhysicalResourceID: oldResource.PhysicalResourceID, ResourceType: definition.Type, Replacement: ReplacementFalse})
			}

			continue
		}
		changed, replacement := classifyChanges(oldDefinition, definition, adapter.Schema())
		if changed {
			replacementValue := ReplacementFalse
			if replacement {
				replacementValue = ReplacementTrue
			}
			changes = append(changes, ResourceChange{Action: ChangeActionModify, LogicalResourceID: logicalID, PhysicalResourceID: oldResource.PhysicalResourceID, ResourceType: definition.Type, Replacement: replacementValue})
		}
	}
	for logicalID, definition := range oldEvaluation.Resources {
		if _, exists := evaluation.Resources[logicalID]; exists {
			continue
		}
		oldResource := oldResources[logicalID]
		changes = append(changes, ResourceChange{Action: ChangeActionRemove, LogicalResourceID: logicalID, PhysicalResourceID: oldResource.PhysicalResourceID, ResourceType: definition.Type, Replacement: ReplacementFalse})
	}
	sort.Slice(changes, func(left, right int) bool { return changes[left].LogicalResourceID < changes[right].LogicalResourceID })

	return changes
}

//nolint:wsl // Parameter comparison is intentionally a compact map traversal.
func changeSetParameterChanges(stack *Stack, evaluation *template.Evaluation) bool {
	if len(stack.Parameters) != len(evaluation.Parameters) {
		return true
	}
	for key, value := range stack.Parameters {
		if evaluation.Parameters[key] != value {
			return true
		}
	}

	return false
}

func exportMap(exports []StackExport) map[string]StackExport {
	result := make(map[string]StackExport, len(exports))
	for _, export := range exports {
		result[export.Name] = export
	}

	return result
}

//nolint:wsl // Sorting is the final step of constructing a deterministic snapshot.
func sortedExports(exports map[string]StackExport) []StackExport {
	result := make([]StackExport, 0, len(exports))
	for _, export := range exports {
		result = append(result, export)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })

	return result
}
