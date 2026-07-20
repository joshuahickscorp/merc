package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func decodeStrictJSONObject(raw []byte, dst any) error {
	if dst == nil {
		return errors.New("strict JSON object: destination is nil")
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return err
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("strict JSON object: decode: %w", err)
	}
	if err := requireJSONEOF(dec); err != nil {
		return err
	}
	return nil
}

func rejectDuplicateJSONKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := walkStrictJSONValue(dec, true); err != nil {
		return err
	}
	return requireJSONEOF(dec)
}

func walkStrictJSONValue(dec *json.Decoder, requireObject bool) error {
	tok, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("strict JSON object: empty input")
		}
		return fmt.Errorf("strict JSON object: malformed JSON: %w", err)
	}

	delim, isDelim := tok.(json.Delim)
	if requireObject && (!isDelim || delim != '{') {
		return errors.New("strict JSON object: top-level value must be an object")
	}
	if !isDelim {
		return nil
	}

	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return fmt.Errorf("strict JSON object: malformed object key: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("strict JSON object: object member name must be a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("strict JSON object: duplicate key %q", key)
			}
			seen[key] = struct{}{}
			if err := walkStrictJSONValue(dec, false); err != nil {
				return err
			}
		}
		return consumeClosingDelimiter(dec, '}')

	case '[':
		for dec.More() {
			if err := walkStrictJSONValue(dec, false); err != nil {
				return err
			}
		}
		return consumeClosingDelimiter(dec, ']')

	default:
		return fmt.Errorf("strict JSON object: unexpected delimiter %q", delim)
	}
}

func consumeClosingDelimiter(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("strict JSON object: malformed JSON: %w", err)
	}
	got, ok := tok.(json.Delim)
	if !ok || got != want {
		return fmt.Errorf("strict JSON object: expected closing delimiter %q", want)
	}
	return nil
}

func requireJSONEOF(dec *json.Decoder) error {
	if _, err := dec.Token(); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("strict JSON object: malformed trailing data: %w", err)
	}
	return errors.New("strict JSON object: multiple JSON values are not allowed")
}
