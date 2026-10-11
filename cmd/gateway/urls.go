package main

import (
	"net"
	"net/url"
)

// gatewayLocalURL turns a bind address into a locally usable HTTP URL.
// Wildcard bind addresses are not browser destinations; use localhost for them.
func gatewayLocalURL(listenAddr, path string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return (&url.URL{Scheme: "http", Host: listenAddr, Path: path}).String()
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "localhost"
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: path}).String()
}
