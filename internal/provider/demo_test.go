package provider

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func demoID(kind string, age, ttl time.Duration) string {
	return fmt.Sprintf("d1.%s.%d.%d.0123456789abcdef", kind, time.Now().Add(-age).UnixNano(), ttl.Nanoseconds())
}

func TestDemoSurvivesNewInstanceAndPreservesCodeLeadingZeroSemantics(t *testing.T) {
	ctx := context.Background()
	activation, err := NewDemo().Allocate(ctx, Request{Kind: "phone", TTL: 20 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	newInstance := NewDemo()
	result, err := newInstance.Poll(ctx, "phone", activation.ID)
	if err != nil || result.Status != "waiting" {
		t.Fatalf("restarted demo lost activation: %+v %v", result, err)
	}
	if err := newInstance.Cancel(ctx, "phone", activation.ID); err == nil {
		t.Fatal("demo cancellation must wait 10 seconds")
	}
	for _, kind := range []string{"phone", "email"} {
		id := demoID(kind, 16*time.Second, 20*time.Minute)
		result, err := NewDemo().Poll(ctx, kind, id)
		if err != nil || result.Status != "received" || result.Code != "628391" {
			t.Fatalf("restarted demo code = %+v %v", result, err)
		}
		if err := NewDemo().Complete(ctx, kind, id); err != nil {
			t.Fatal(err)
		}
		if err := NewDemo().Cancel(ctx, kind, id); err == nil {
			t.Fatal("cannot release an activation after its code has arrived")
		}
	}
}

func TestDemoExpiredWaitingCannotProduceCode(t *testing.T) {
	id := demoID("email", 10*time.Second, 5*time.Second)
	result, err := NewDemo().Poll(context.Background(), "email", id)
	if err != nil || result.Status != "expired" {
		t.Fatalf("result = %+v %v", result, err)
	}
}

func TestDemoRejectsMalformedOrWrongKindIDs(t *testing.T) {
	for _, id := range []string{"", "d1.phone.1.1.x", demoID("phone", time.Second, time.Hour), "d1.email.-1.1000.0123456789abcdef"} {
		if _, err := NewDemo().Poll(context.Background(), "email", id); err == nil {
			t.Errorf("accepted invalid ID %s", id)
		}
	}
}
