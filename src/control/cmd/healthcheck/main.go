package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const probeTimeout = 3 * time.Second

const readyRequest = "GET /readyz HTTP/1.0\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"

// parsePort preserves the dedicated probe's deliberately narrow contract: it
// ignores the host and reads only a trailing port from LISTEN_ADDR, defaulting
// to 8080 when no port is present.
func parsePort(address string) (int, error) {
	separator := strings.LastIndexByte(address, ':')
	if separator < 0 || separator+1 == len(address) {
		return 8080, nil
	}
	port, err := strconv.Atoi(strings.TrimLeft(address[separator+1:], " \t\r\n"))
	if err != nil || port < 1 || port > 65535 {
		if err == nil {
			err = strconv.ErrSyntax
		}
		return 0, err
	}
	return port, nil
}

func writeAll(conn net.Conn, data []byte) bool {
	for len(data) > 0 {
		written, err := conn.Write(data)
		if err != nil || written <= 0 {
			return false
		}
		data = data[written:]
	}
	return true
}

func probe(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), probeTimeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(probeTimeout)); err != nil {
		return false
	}
	if !writeAll(conn, []byte(readyRequest)) {
		return false
	}

	var response [12]byte
	received, err := io.ReadFull(conn, response[:])
	if err != nil || received < len(response) {
		return false
	}

	// Drain the close-delimited response before releasing the socket. This keeps
	// a prefix-only probe from sending a TCP reset while the server is writing
	// its readiness body.
	var drain [1024]byte
	for {
		if _, err := conn.Read(drain[:]); err != nil {
			break
		}
	}

	return bytes.Equal(response[:], []byte("HTTP/1.0 200")) ||
		bytes.Equal(response[:], []byte("HTTP/1.1 200"))
}

func main() {
	port, err := parsePort(os.Getenv("LISTEN_ADDR"))
	if err != nil {
		os.Exit(1)
	}
	if !probe(port) {
		os.Exit(1)
	}
}
