package cloudformation

import (
	"reflect"
	"testing"
	"time"
)

const (
	testStackID      = "stack-id"
	testRequestToken = "token"
	testEventA       = "event-a"
	testEventZ       = "event-z"
)

func TestEventLogCarriesTokensDeduplicatesAndCaps(t *testing.T) {
	t.Parallel()

	log := NewEventLog(2)
	base := StackEvent{
		EventID:            "event-1",
		StackID:            testStackID,
		StackName:          testApplicationStackName,
		LogicalResourceID:  testBucketLogicalID,
		ResourceType:       testS3BucketResourceType,
		ResourceStatus:     ResourceStatusCreateComplete,
		ClientRequestToken: "request-1",
		Timestamp:          time.Unix(1, 0),
	}

	if err := log.Append(base); err != nil {
		t.Fatalf("Append(base) error = %v", err)
	}

	if err := log.Append(base); err == nil {
		t.Fatal("Append(duplicate) error = nil, want duplicate error")
	}

	second := base
	second.EventID = testSecondEventID
	second.ClientRequestToken = "request-2"
	second.Timestamp = time.Unix(2, 0)

	if err := log.Append(second); err != nil {
		t.Fatalf("Append(second) error = %v", err)
	}

	third := second
	third.EventID = "event-3"
	third.ClientRequestToken = "request-1"
	third.Timestamp = time.Unix(3, 0)

	if err := log.Append(third); err != nil {
		t.Fatalf("Append(third) error = %v", err)
	}

	if got, want := log.List(testStackID, "request-1"), []StackEvent{third}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List(token) = %#v, want %#v", got, want)
	}

	if got := log.List(testStackID, "request-2"); len(got) != 1 || got[0].EventID != "event-2" {
		t.Fatalf("List(second token) = %#v, want event-2", got)
	}

	if got := log.List(testStackID, ""); len(got) != 2 || got[0].EventID != "event-2" || got[1].EventID != "event-3" {
		t.Fatalf("List(all) = %#v, want capped event-2/event-3", got)
	}
}

func TestEventLogRequiresEventIDAndClientRequestToken(t *testing.T) {
	t.Parallel()

	log := NewEventLog(1)
	for _, event := range []StackEvent{{ClientRequestToken: testRequestToken}, {EventID: "event"}} {
		if err := log.Append(event); err == nil {
			t.Fatalf("Append(%#v) error = nil, want validation error", event)
		}
	}
}

func TestEventLogListsEventsChronologicallyWithStableTieBreaking(t *testing.T) {
	t.Parallel()

	log := NewEventLog(4)
	events := []StackEvent{
		{EventID: testEventZ, StackID: testStackID, ClientRequestToken: testRequestToken, Timestamp: time.Unix(3, 0)},
		{EventID: "event-b", StackID: testStackID, ClientRequestToken: testRequestToken, Timestamp: time.Unix(1, 0)},
		{EventID: testEventA, StackID: testStackID, ClientRequestToken: testRequestToken, Timestamp: time.Unix(1, 0)},
		{EventID: "event-c", StackID: testStackID, ClientRequestToken: testRequestToken, Timestamp: time.Unix(2, 0)},
	}

	for _, event := range events {
		if err := log.Append(event); err != nil {
			t.Fatalf("Append(%q) error = %v", event.EventID, err)
		}
	}

	got := log.List(testStackID, "")
	gotIDs := make([]string, len(got))

	for index := range got {
		gotIDs[index] = got[index].EventID
	}

	if want := []string{testEventA, "event-b", "event-c", testEventZ}; !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("List() event IDs = %v, want %v", gotIDs, want)
	}
}
