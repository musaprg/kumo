package cloudformation

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// StackEvent records a stack or resource state transition.
type StackEvent struct {
	EventID              string
	StackID              string
	StackName            string
	LogicalResourceID    string
	PhysicalResourceID   string
	ResourceType         string
	ResourceStatus       string
	ResourceStatusReason string
	ErrorCode            string
	Timestamp            time.Time
	ClientRequestToken   string
}

// EventLog stores a bounded chronological sequence of unique stack events.
type EventLog struct {
	mu       sync.RWMutex
	capacity int
	events   []StackEvent
	seen     map[string]struct{}
}

// NewEventLog creates an event log capped at capacity events.
func NewEventLog(capacity int) *EventLog {
	if capacity < 1 {
		capacity = 1
	}

	return &EventLog{capacity: capacity, seen: make(map[string]struct{})}
}

// Append adds an event, rejecting missing tokens and duplicate event IDs.
//
//nolint:gocritic // hugeParam: value copy preserves the event snapshot semantics.
func (l *EventLog) Append(event StackEvent) error {
	if event.EventID == "" {
		return fmt.Errorf("event ID is required")
	}

	if event.ClientRequestToken == "" {
		return fmt.Errorf("client request token is required")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if _, exists := l.seen[event.EventID]; exists {
		return fmt.Errorf("event ID %q already exists", event.EventID)
	}

	l.seen[event.EventID] = struct{}{}
	l.events = append(l.events, event)

	if len(l.events) > l.capacity {
		removed := l.events[0]
		l.events = l.events[1:]
		delete(l.seen, removed.EventID)
	}

	return nil
}

// List returns events for stackID, optionally filtered by client request token.
// Results are ordered chronologically, with event ID as the tie-breaker.
func (l *EventLog) List(stackID, clientRequestToken string) []StackEvent {
	l.mu.RLock()
	defer l.mu.RUnlock()

	result := make([]StackEvent, 0, len(l.events))

	for index := range l.events {
		event := &l.events[index]
		if event.StackID != stackID || clientRequestToken != "" && event.ClientRequestToken != clientRequestToken {
			continue
		}

		result = append(result, *event)
	}

	sort.SliceStable(result, func(left, right int) bool {
		if result[left].Timestamp.Equal(result[right].Timestamp) {
			return result[left].EventID < result[right].EventID
		}

		return result[left].Timestamp.Before(result[right].Timestamp)
	})

	return result
}
