package firewall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type invocation struct {
	Args  []string
	Input string
}

func TestMain(m *testing.M) {
	if os.Getenv("RUNNER_NFT_FIXTURE") == "1" {
		fixtureProcess()
		return
	}
	os.Exit(m.Run())
}

// The test executable acts as nft so tests cover real process arguments and stdin.
func fixtureProcess() {
	directory := os.Getenv("RUNNER_NFT_DIRECTORY")
	input, err := os.ReadFile("/dev/stdin")
	if err != nil {
		os.Exit(90)
	}
	log, err := os.OpenFile(filepath.Join(directory, "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(91)
	}
	err = json.NewEncoder(log).Encode(invocation{Args: os.Args[1:], Input: string(input)})
	log.Close()
	if err != nil {
		os.Exit(92)
	}
	var stage string
	switch {
	case reflect.DeepEqual(os.Args[1:], []string{"-j", "list", "tables"}):
		stage = "list"
	case reflect.DeepEqual(os.Args[1:], []string{"--check", "-f", "-"}):
		stage = "check"
	case reflect.DeepEqual(os.Args[1:], []string{"-f", "-"}):
		stage = "apply"
	default:
		os.Exit(93)
	}
	if os.Getenv("RUNNER_NFT_FAIL") == stage {
		fmt.Fprintln(os.Stderr, "fixture rejection")
		os.Exit(7)
	}
	if stage == "list" {
		fmt.Print(os.Getenv("RUNNER_NFT_LIST"))
	}
	if stage == "check" && os.Getenv("RUNNER_NFT_CHANGE_RULES") == "1" {
		if err := os.WriteFile(filepath.Join(directory, "rules.nft"), []byte("changed after check\n"), 0600); err != nil {
			os.Exit(94)
		}
	}
	if stage == "check" && os.Getenv("RUNNER_NFT_BLOCK_CHECK") == "1" {
		if err := os.WriteFile(filepath.Join(directory, "check-started"), nil, 0600); err != nil {
			os.Exit(95)
		}
		time.Sleep(time.Minute)
	}
}

func fixture(t *testing.T, list string) (string, string, func() []invocation) {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("RUNNER_NFT_FIXTURE", "1")
	t.Setenv("RUNNER_NFT_DIRECTORY", directory)
	t.Setenv("RUNNER_NFT_LIST", list)
	t.Setenv("RUNNER_NFT_FAIL", "")
	t.Setenv("RUNNER_NFT_CHANGE_RULES", "")
	t.Setenv("RUNNER_NFT_BLOCK_CHECK", "")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(directory, "rules.nft")
	if err := os.WriteFile(rules, []byte("table inet nucleiron {}\ntable bridge nucleiron {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return executable, rules, func() []invocation {
		data, err := os.ReadFile(filepath.Join(directory, "calls.jsonl"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		var calls []invocation
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var call invocation
			if err := json.Unmarshal([]byte(line), &call); err != nil {
				t.Fatal(err)
			}
			calls = append(calls, call)
		}
		return calls
	}
}

func TestReloadChecksAndAppliesOnlyOwnedTables(t *testing.T) {
	for _, test := range []struct {
		name, list, prefix string
	}{
		{"none", `{"nftables":[{"metainfo":{"json_schema_version":1}}]}`, ""},
		{"partial", `{"nftables":[{"table":{"family":"bridge","name":"nucleiron"}}]}`, "delete table bridge nucleiron\n"},
		{"both_and_unrelated", `{"nftables":[{"table":{"family":"bridge","name":"nucleiron"}},{"table":{"family":"inet","name":"other"}},{"table":{"family":"ip","name":"nucleiron"}},{"table":{"family":"inet","name":"nucleiron"}}]}`, "delete table inet nucleiron\ndelete table bridge nucleiron\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			nft, rules, calls := fixture(t, test.list)
			t.Setenv("RUNNER_NFT_CHANGE_RULES", "1")
			if err := Reload(context.Background(), nft, rules); err != nil {
				t.Fatal(err)
			}
			batch := test.prefix + "table inet nucleiron {}\ntable bridge nucleiron {}\n"
			want := []invocation{{[]string{"-j", "list", "tables"}, ""}, {[]string{"--check", "-f", "-"}, batch}, {[]string{"-f", "-"}, batch}}
			if got := calls(); !reflect.DeepEqual(got, want) {
				t.Fatalf("calls = %#v; want %#v", got, want)
			}
		})
	}
}

func TestCommandFailureStopsWithoutRetry(t *testing.T) {
	for index, stage := range []string{"list", "check", "apply"} {
		t.Run(stage, func(t *testing.T) {
			nft, rules, calls := fixture(t, `{"nftables":[]}`)
			t.Setenv("RUNNER_NFT_FAIL", stage)
			err := Reload(context.Background(), nft, rules)
			if err == nil || !strings.Contains(err.Error(), stage+" firewall") || !strings.Contains(err.Error(), "fixture rejection") {
				t.Fatalf("failure lost stage or cause: %v", err)
			}
			if got := len(calls()); got != index+1 {
				t.Fatalf("%d calls after %s failure; want %d", got, stage, index+1)
			}
			if stage == "apply" && !strings.Contains(err.Error(), "outcome may be unknown") {
				t.Fatalf("application failure asserted a known outcome: %v", err)
			}
		})
	}
}

func TestInvalidListNeverChecksOrApplies(t *testing.T) {
	for _, raw := range []string{`invalid`, `{}`, `{"nftables":null}`, `{"nftables":[null]}`, `{"nftables":[{"table":{"name":"nucleiron"}}]}`, `{"nftables":[{"table":{"family":2,"name":"nucleiron"}}]}`} {
		t.Run(raw, func(t *testing.T) {
			nft, rules, calls := fixture(t, raw)
			if err := Reload(context.Background(), nft, rules); err == nil {
				t.Fatal("invalid list accepted")
			}
			if got := len(calls()); got != 1 {
				t.Fatalf("invalid list led to %d calls", got)
			}
		})
	}
}

func TestMissingRulesNeverChecksOrApplies(t *testing.T) {
	nft, rules, calls := fixture(t, `{"nftables":[]}`)
	if err := os.Remove(rules); err != nil {
		t.Fatal(err)
	}
	if err := Reload(context.Background(), nft, rules); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing rules cause lost: %v", err)
	}
	if got := len(calls()); got != 1 {
		t.Fatalf("missing rules led to %d calls", got)
	}
}

func TestCanceledContextStartsNoCommand(t *testing.T) {
	nft, rules, calls := fixture(t, `{"nftables":[]}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Reload(ctx, nft, rules); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if len(calls()) != 0 {
		t.Fatal("command started after cancellation")
	}
}

func TestCanceledCheckIsReapedWithoutApplication(t *testing.T) {
	nft, rules, calls := fixture(t, `{"nftables":[]}`)
	t.Setenv("RUNNER_NFT_BLOCK_CHECK", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- Reload(ctx, nft, rules) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(filepath.Dir(rules), "check-started")); err == nil {
			break
		}
		select {
		case err := <-finished:
			t.Fatalf("reload ended before the check started: %v", err)
		case <-ctx.Done():
			t.Fatal("check did not start within the test deadline")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled child was not reaped")
	}
	if got := len(calls()); got != 2 {
		t.Fatalf("canceled check led to %d commands; want list and check only", got)
	}
}
