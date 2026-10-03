package cloudformation

import (
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/sivchari/kumo/internal/service"
	"github.com/sivchari/kumo/internal/service/cloudformation/resource"
)

const serviceName = "cloudformation"

// Compile-time check that Service implements io.Closer.
var _ io.Closer = (*Service)(nil)

func init() {
	var opts []Option
	if dir := os.Getenv("KUMO_DATA_DIR"); dir != "" {
		opts = append(opts, WithDataDir(dir))
	}

	storage, err := NewMemoryStorageWithError(opts...)
	if err != nil {
		panic(fmt.Errorf("initialize cloudformation storage: %w", err))
	}

	service.Register(New(storage))
}

// Service implements the CloudFormation service.
type Service struct {
	storage  Storage
	registry *resource.Registry
}

// New creates a new CloudFormation service.
func New(storage Storage) *Service {
	return NewWithRegistry(storage, nil)
}

// NewWithRegistry creates a CloudFormation service with an optional resource registry.
func NewWithRegistry(storage Storage, registry *resource.Registry) *Service {
	return &Service{
		storage:  storage,
		registry: registry,
	}
}

func (s *Service) executor() *Executor {
	return NewExecutor(s.storage, s.registry)
}

// Name returns the service name.
func (s *Service) Name() string {
	return serviceName
}

// TargetPrefix returns the X-Amz-Target prefix for the service.
func (s *Service) TargetPrefix() string {
	return "CloudFormation"
}

// Actions returns the list of actions supported by this service.
func (s *Service) Actions() []string {
	return []string{
		"CreateStack",
		"CreateChangeSet",
		"DeleteStack",
		"DescribeChangeSet",
		"DescribeStackEvents",
		"DescribeStackResource",
		"DescribeStackResources",
		"DescribeStacks",
		"ExecuteChangeSet",
		"ListExports",
		"ListStacks",
		"UpdateStack",
		"GetTemplate",
		"GetTemplateSummary",
		"ValidateTemplate",
	}
}

// ServiceIdentifier returns the SDK service identifier for User-Agent disambiguation.
func (s *Service) ServiceIdentifier() string {
	return serviceName
}

// QueryProtocol marks this service as using the Query protocol.
func (s *Service) QueryProtocol() {}

// RegisterRoutes registers HTTP routes for the service.
func (s *Service) RegisterRoutes(_ service.Router) {
	// CloudFormation uses Query protocol, routes are handled by the dispatcher.
}

// Compile-time check that Service implements the required interfaces.
var (
	_ service.Service              = (*Service)(nil)
	_ service.QueryProtocolService = (*Service)(nil)
)

// HandleRequest handles HTTP requests for the CloudFormation service.
func (s *Service) HandleRequest(w http.ResponseWriter, r *http.Request) {
	s.DispatchAction(w, r)
}

// Close saves the storage state if persistence is enabled.
func (s *Service) Close() error {
	if c, ok := s.storage.(io.Closer); ok {
		if err := c.Close(); err != nil {
			return fmt.Errorf("failed to close storage: %w", err)
		}
	}

	return nil
}

// Meta returns the service's documentation metadata.
func (s *Service) Meta() service.Meta {
	return service.Meta{
		Display:     "CloudFormation",
		Category:    "Management & Configuration",
		Description: "Infrastructure as code",
	}
}
