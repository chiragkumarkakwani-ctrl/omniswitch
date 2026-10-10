package provider

import "fmt"

// UnsupportedEndpointError identifies a provider capability mismatch rather
// than an upstream service failure. HTTP gateways can surface it as a 400.
type UnsupportedEndpointError struct {
	Provider string
	Endpoint string
}

func (e *UnsupportedEndpointError) Error() string {
	return fmt.Sprintf("provider %q does not support %s", e.Provider, e.Endpoint)
}
