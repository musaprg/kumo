package resource

import (
	"sort"
	"sync"
)

// Registry maps a CloudFormation resource type to its adapter.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry returns an empty adapter registry.
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]Adapter)}
}

// Register installs an adapter for its declared resource type.
func (r *Registry) Register(adapter Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.adapters[adapter.Schema().TypeName] = adapter
}

// Get returns the adapter for typeName.
func (r *Registry) Get(typeName string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	adapter, ok := r.adapters[typeName]

	return adapter, ok
}

// TypeNames returns all registered type names in lexical order.
func (r *Registry) TypeNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	typeNames := make([]string, 0, len(r.adapters))
	for typeName := range r.adapters {
		typeNames = append(typeNames, typeName)
	}

	sort.Strings(typeNames)

	return typeNames
}

var defaultRegistry = newDefaultRegistry()

// DefaultRegistry returns the production registry of resource adapters.
func DefaultRegistry() *Registry {
	return defaultRegistry
}

func newDefaultRegistry() *Registry {
	registry := NewRegistry()
	for _, adapter := range []Adapter{
		&s3BucketAdapter{},
		&dynamoDBTableAdapter{},
		&sqsQueueAdapter{},
		&snsTopicAdapter{},
		&iamRoleAdapter{},
		&iamPolicyAdapter{},
		&lambdaFunctionAdapter{},
		&lambdaPermissionAdapter{},
		&lambdaEventSourceMappingAdapter{},
		&snsSubscriptionAdapter{},
		&sqsQueuePolicyAdapter{},
	} {
		registry.Register(adapter)
	}

	return registry
}
