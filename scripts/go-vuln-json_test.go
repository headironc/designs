package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type repeatingJSONReader struct{}

func (repeatingJSONReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

type failedJSONReader struct{}

func (failedJSONReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestVulnerabilityJSONInputFailure(t *testing.T) {
	if _, err := decodeVulnerabilityJSON(repeatingJSONReader{}, "scan"); err == nil || !strings.Contains(err.Error(), "exceeds 64 MiB") {
		t.Fatalf("size limit: %v", err)
	}
	if _, err := decodeVulnerabilityJSON(failedJSONReader{}, "scan"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error: %v", err)
	}
}

func TestVulnerabilityJSONStream(t *testing.T) {
	for _, input := range []string{
		``, ` `, `{`, `{"config":{}} trailing`, `{"config":{}} {`,
		`[]`, `null`, `{"unknown":{}}`, `{"config":null}`,
		`{"config":{},"config":{}}`, `{"config":{},"SBOM":{}}`,
	} {
		if _, err := decodeVulnerabilityJSON(strings.NewReader(input), "scan"); err == nil {
			t.Errorf("accepted invalid stream %q", input)
		}
	}
	input := "{\n\"config\":{\"text\":\"literal }{ and \\\"quoted\\\"\"}}\n{\"SBOM\":{}}"
	messages, err := decodeVulnerabilityJSON(strings.NewReader(input), "scan")
	if err != nil || len(messages) != 2 {
		t.Fatalf("valid stream: %d messages, %v", len(messages), err)
	}
	if _, err := decodeVulnerabilityJSON(strings.NewReader(`{"ImportPath":"one"}{"ImportPath":"two"}`), "packages"); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeVulnerabilityJSON(strings.NewReader(`{}`), "other"); err == nil {
		t.Fatal("accepted unknown mode")
	}
}
