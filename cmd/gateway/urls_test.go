package main

import "testing"

func TestGatewayLocalURL(t *testing.T) {
	for _, test := range []struct {
		listen string
		want   string
	}{
		{":18130", "http://localhost:18130"},
		{"127.0.0.1:18130", "http://127.0.0.1:18130"},
		{"localhost:18130", "http://localhost:18130"},
		{"gateway.internal:18130", "http://gateway.internal:18130"},
		{"0.0.0.0:18130", "http://localhost:18130"},
		{"[::]:18130", "http://localhost:18130"},
		{"[::1]:18130", "http://[::1]:18130"},
		{"[2001:db8::1]:18130", "http://[2001:db8::1]:18130"},
		{"[fe80::1%lo0]:18130", "http://[fe80::1%25lo0]:18130"},
	} {
		t.Run(test.listen, func(t *testing.T) {
			for _, path := range []string{"", "/v1/chat/completions", "/mcp"} {
				if got := gatewayLocalURL(test.listen, path); got != test.want+path {
					t.Fatalf("gatewayLocalURL(%q, %q) = %q, want %q", test.listen, path, got, test.want+path)
				}
			}
		})
	}
}
