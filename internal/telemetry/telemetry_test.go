package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func TestResourceNamespace(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.namespace=sentinel")
	res, err := newResource(context.Background(), "test-gateway")
	if err != nil {
		t.Fatal(err)
	}
	attrs := res.Set()
	for key, want := range map[attribute.Key]string{"service.namespace": "omniswitch", "service.name": "test-gateway"} {
		got, ok := attrs.Value(key)
		if !ok || got.AsString() != want {
			t.Errorf("%s = %s, want %s", key, got.AsString(), want)
		}
	}
}
