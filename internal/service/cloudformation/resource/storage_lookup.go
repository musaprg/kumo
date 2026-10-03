package resource

import (
	"fmt"

	"github.com/sivchari/kumo/internal/service"
)

// lookupStorage finds a registered service exposing Storage() T. Adapters use
// the production service registry so they share the real in-process backends.
func lookupStorage[T any](serviceName string) (T, error) {
	var zero T

	for _, svc := range service.Services() {
		if svc.Name() != serviceName {
			continue
		}

		provider, ok := svc.(interface{ Storage() T })
		if !ok {
			return zero, fmt.Errorf("%s service does not expose Storage() returning the expected type", serviceName)
		}

		return provider.Storage(), nil
	}

	return zero, fmt.Errorf("%s service is not registered", serviceName)
}
