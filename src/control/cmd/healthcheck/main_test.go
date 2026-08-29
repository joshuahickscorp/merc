package main

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
)

func TestParsePort(t *testing.T) {
	tests := []struct {
		name    string
		address string
		want    int
		invalid bool
	}{
		{name: "default", address: "", want: 8080},
		{name: "hostless", address: ":8080", want: 8080},
		{name: "ipv4", address: "127.0.0.1:4312", want: 4312},
		{name: "ipv6", address: "[::]:1", want: 1},
		{name: "host without port", address: "127.0.0.1", want: 8080},
		{name: "leading whitespace", address: ": 8080", want: 8080},
		{name: "zero", address: ":0", invalid: true},
		{name: "too large", address: ":65536", invalid: true},
		{name: "not numeric", address: ":nope", invalid: true},
		{name: "trailing text", address: ":8080x", invalid: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parsePort(test.address)
			if test.invalid {
				if err == nil {
					t.Fatalf("parsePort(%q) succeeded with %d", test.address, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("parsePort(%q) = %d, %v; want %d, nil", test.address, got, err, test.want)
			}
		})
	}
}

func TestProbeAcceptsOnlyHTTP200Prefix(t *testing.T) {
	tests := []struct {
		name        string
		response    string
		wantHealthy bool
	}{
		{name: "http 1.0", response: "HTTP/1.0 200 OK\r\nContent-Length: 0\r\n\r\n", wantHealthy: true},
		{name: "http 1.1", response: "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n", wantHealthy: true},
		{name: "drained body", response: "HTTP/1.1 200 OK\r\nContent-Length: 2048\r\n\r\n" + strings.Repeat("x", 2048), wantHealthy: true},
		{name: "wrong status", response: "HTTP/1.1 503 Busy\r\n\r\n", wantHealthy: false},
		{name: "short response", response: "HTTP/1.1 20", wantHealthy: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()

			done := make(chan error, 1)
			go func() {
				conn, acceptErr := listener.Accept()
				if acceptErr != nil {
					done <- acceptErr
					return
				}
				defer conn.Close()
				request := make([]byte, len(readyRequest))
				if _, readErr := io.ReadFull(conn, request); readErr != nil {
					done <- readErr
					return
				}
				_, writeErr := io.WriteString(conn, test.response)
				done <- writeErr
			}()

			port := listener.Addr().(*net.TCPAddr).Port
			if got := probe(port); got != test.wantHealthy {
				t.Fatalf("probe(%d) = %v, want %v", port, got, test.wantHealthy)
			}
			if err := <-done; err != nil {
				t.Fatal(fmt.Errorf("raw server: %w", err))
			}
		})
	}
}
