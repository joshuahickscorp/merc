package main

import (
	"errors"
	"io"
	"math"
)

var errRemoteResponseTooLarge = errors.New("remote response exceeds configured size limit")

func readBoundedRemoteBody(r io.Reader, maxBytes int64) ([]byte, error) {
	if r == nil {
		return nil, errors.New("remote response body is nil")
	}
	if maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return nil, errors.New("remote response size limit is invalid")
	}
	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, errRemoteResponseTooLarge
	}
	return body, nil
}
