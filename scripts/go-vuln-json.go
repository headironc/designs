// Decode the two JSON streams consumed by the Go vulnerability entrypoint.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func decodeVulnerabilityJSON(input io.Reader, mode string) ([]json.RawMessage, error) {
	if mode != "scan" && mode != "packages" {
		return nil, fmt.Errorf("expected scan or packages mode")
	}
	const limit = 64 * 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read JSON input: %w", err)
	}
	if len(data) > limit {
		return nil, fmt.Errorf("JSON input exceeds 64 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	messages := []json.RawMessage{}
	for {
		var message json.RawMessage
		if err := decoder.Decode(&message); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("incomplete JSON stream: %w", err)
		}
		if len(message) == 0 || message[0] != '{' {
			return nil, fmt.Errorf("expected JSON object")
		}
		if mode == "scan" {
			// The protocol requires exactly one event per message. Token decoding
			// also refuses duplicate event keys rather than silently overwriting.
			event := json.NewDecoder(bytes.NewReader(message))
			_, _ = event.Token()
			count := 0
			for event.More() {
				key, err := event.Token()
				if err != nil {
					return nil, err
				}
				switch key {
				case "config", "SBOM", "osv", "finding", "progress":
				default:
					return nil, fmt.Errorf("unknown scan event %v", key)
				}
				var value json.RawMessage
				if err := event.Decode(&value); err != nil || len(value) == 0 || value[0] != '{' {
					return nil, fmt.Errorf("scan event must contain an object")
				}
				count++
			}
			if count != 1 {
				return nil, fmt.Errorf("expected exactly one scan event")
			}
		}
		messages = append(messages, message)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("empty JSON stream")
	}
	return messages, nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go-vuln-json scan|packages")
		os.Exit(1)
	}
	messages, err := decodeVulnerabilityJSON(os.Stdin, os.Args[1])
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(messages)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
