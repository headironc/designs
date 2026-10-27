package bootstrap

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

func TestRenderedTemplateKeepsIdentityOutOfShellEvaluationAndTrace(t *testing.T) {
	source, err := os.ReadFile("../../templates/runner-install.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	// These are the fields available on the native InstallRunnerParams consumer.
	params := struct {
		MetadataURL, CallbackURL, CallbackToken, RunnerName string
		UseJITConfig                                        bool
	}{metadataEndpoint, callbackEndpoint, "$(id)`id`\"$HOME\"", testInstance, true}
	tmpl, err := template.New("install").Option("missingkey=error").Parse(string(source))
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, params); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sink := filepath.Join(dir, "capture")
	capture := filepath.Join(dir, "input.json")
	if err := os.WriteFile(sink, []byte("#!/bin/sh\ncat > \"$BOOTSTRAP_CAPTURE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	script := strings.Replace(rendered.String(), "/usr/local/bin/runner-bootstrap", "'"+strings.ReplaceAll(sink, "'", "'\"'\"'")+"'", 1)
	cmd := exec.Command("bash", "-xv")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "BOOTSTRAP_CAPTURE="+capture)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("template execution: %v", err)
	}
	if strings.Contains(string(output), params.CallbackToken) || strings.Contains(string(output), "callback_token") {
		t.Fatal("shell tracing exposed the rendered input")
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	config, err := Decode(bytes.NewReader(raw))
	if err != nil || config.token != params.CallbackToken || config.instance != params.RunnerName {
		t.Fatal("shell changed the instance identity")
	}
}

func TestReadyReceiptNeverOverwritesAnExistingResult(t *testing.T) {
	home := t.TempDir()
	if err := publishReady(home, []byte(`{"agent_id":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := publishReady(home, []byte(`{"agent_id":2}`)); err == nil {
		t.Fatal("existing receipt overwritten")
	}
	raw, err := os.ReadFile(filepath.Join(home, ".nucleiron-bootstrap-ready"))
	if err != nil || string(raw) != `{"agent_id":1}` {
		t.Fatal("original receipt changed")
	}
}
