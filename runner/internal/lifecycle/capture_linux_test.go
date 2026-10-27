package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/headironc/designs/runner/internal/provider"
)

const requestJSON = `{"version":1,"operation_id":"aaaaaaaa-0000-4000-8000-000000000001","repository_id":9007199254740993,"owner":"example","repository":"consumer","run_id":9007199254740995,"attempt":2,"job_id":9007199254740997,"commit":"0123456789012345678901234567890123456789","controller_id":"bbbbbbbb-0000-4000-8000-000000000002","garm_repository_id":"cccccccc-0000-4000-8000-000000000003","garm_scaleset_id":27,"garm_instance_id":"dddddddd-0000-4000-8000-000000000004"}`
const vmUUID = "eeeeeeee-0000-4000-8000-000000000005"
const volumeUUID = "ffffffff-0000-4000-8000-000000000006"
const bootUUID = "11111111-0000-4000-8000-000000000007"
const runnerID int64 = 9007199254740999

func binding() provider.ExecutionBinding {
	return provider.ExecutionBinding{Controller: "bbbbbbbb-0000-4000-8000-000000000002", Repository: "cccccccc-0000-4000-8000-000000000003", ScaleSet: "build", Pool: "build-cccccccc-0000-4000-8000-000000000003", Image: strings.Repeat("1", 64), Name: "runner-one", UUID: vmUUID, AgentID: runnerID, StateSHA: strings.Repeat("2", 64), PolicySHA: strings.Repeat("3", 64), ProviderSHA: strings.Repeat("4", 64), ConfigSHA: strings.Repeat("5", 64)}
}

func testWrite(t *testing.T, path string, raw []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func captureFixture(t *testing.T) (capture, string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(root, "trusted.json")
	testWrite(t, requestPath, []byte(requestJSON), 0600)
	operations := filepath.Join(root, "operations")
	if err := os.Mkdir(operations, 0700); err != nil {
		t.Fatal(err)
	}
	vm := machine{4321, "runner-one", volumeUUID, "virtual-machine", "halo-ci", "halo-ci", "block", "", "/1.0/instances/runner-one?project=halo-ci"}
	host := process{bootUUID, 4321, 876543, "/snap/lxd/12345/bin/qemu-system-x86_64", 42, 55, "/lxd/runner-one", 43, 56}
	c := capture{files: protectedFiles{uid: uint32(os.Geteuid()), boundary: root}, guard: func(_ context.Context, f func(provider.ExecutionBinding, func() error) (func() error, error)) error {
		return runFixtureGuard(binding(), func() error { return nil }, f)
	}, platform: func(context.Context, request, provider.ExecutionBinding) (int, error) { return 99, nil }, machine: func(context.Context, provider.ExecutionBinding) (machine, error) { return vm, nil }, process: func(int64, string) (process, error) { return host, nil }, now: func() time.Time { return time.Date(2026, 10, 28, 1, 2, 3, 0, time.UTC) }, producerSHA: strings.Repeat("6", 64)}
	return c, requestPath, operations
}

func runFixtureGuard(b provider.ExecutionBinding, recheck func() error, f func(provider.ExecutionBinding, func() error) (func() error, error)) error {
	publish, err := f(b, recheck)
	if err != nil {
		return err
	}
	return publish()
}

func TestCaptureOriginalIdentityAndReadback(t *testing.T) {
	c, path, operations := captureFixture(t)
	if stage, err := c.execute(context.Background(), path, operations); err != nil {
		t.Fatalf("%s: %v", stage, err)
	}
	dir := filepath.Join(operations, "aaaaaaaa-0000-4000-8000-000000000001")
	stored, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil || string(stored) != requestJSON {
		t.Fatal("original bytes lost")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Independent literal expected wire identities include values above 2^53.
	for _, expected := range []string{`"repository_id":9007199254740993`, `"run_id":9007199254740995`, `"job_id":9007199254740997`, `"runner_id":9007199254740999`, `"github_scaleset_id":99`, `"garm_scaleset_id":27`, `"vm_uuid":"` + vmUUID + `"`, `"volume_uuid":"` + volumeUUID + `"`, `"boot_id":"` + bootUUID + `"`, `"starttime_ticks":876543`, `"producer_sha256":"` + strings.Repeat("6", 64) + `"`, `"captured_at":"2026-10-28T01:02:03Z"`} {
		if !bytes.Contains(raw, []byte(expected)) {
			t.Fatalf("missing independent expectation %s", expected)
		}
	}
	var record receipt
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.RequestSHA != fmt.Sprintf("%x", sha256.Sum256([]byte(requestJSON))) {
		t.Fatal("incorrect request hash")
	}
	c.guard = func(context.Context, func(provider.ExecutionBinding, func() error) (func() error, error)) error {
		t.Fatal("readback performed new observation")
		return errRefused
	}
	if stage, err := c.execute(context.Background(), path, operations); err != nil || stage != "existing" {
		t.Fatalf("%s: %v", stage, err)
	}
	again, _ := os.ReadFile(filepath.Join(dir, "execution.json"))
	if !bytes.Equal(raw, again) {
		t.Fatal("original overwritten")
	}
}

func TestCaptureSDKAndHostIntegration(t *testing.T) {
	c, path, operations := captureFixture(t)
	p := platformFixture(t, platformBodies())
	socket := machineFixture(t, machineBodies())
	host, _ := processFixture(t)
	c.platform = p.observe
	c.machine = func(ctx context.Context, b provider.ExecutionBinding) (machine, error) {
		return lxdMachine(ctx, socket, b)
	}
	c.process = host.observe
	if stage, err := c.execute(context.Background(), path, operations); err != nil {
		t.Fatalf("%s: %v", stage, err)
	}
	raw, err := os.ReadFile(filepath.Join(operations, "aaaaaaaa-0000-4000-8000-000000000001", "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record receipt
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Expected.JobID != 9007199254740997 || record.Observed.RunnerID != 9007199254740999 || record.Observed.GitHubScaleSetID != 99 || record.Observed.VM.VolumeUUID != volumeUUID || record.Observed.Host.StartTime != 876543 || record.Observed.Host.Cgroup != "/lxd/runner-one" {
		t.Fatal("SDK allocation identity did not reach durable record")
	}
}

func TestCaptureChangesAndFailureRemainIncomplete(t *testing.T) {
	for _, kind := range []string{"platform", "controller", "volume uuid", "pid reuse", "boot", "cgroup", "exit", "guard", "request", "cancel before seal"} {
		t.Run(kind, func(t *testing.T) {
			c, path, operations := captureFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			originalMachine, originalProcess := c.machine, c.process
			switch kind {
			case "platform":
				c.platform = func(context.Context, request, provider.ExecutionBinding) (int, error) { return 0, errRefused }
			case "controller":
				c.guard = func(_ context.Context, f func(provider.ExecutionBinding, func() error) (func() error, error)) error {
					b := binding()
					b.Controller = "bad"
					return runFixtureGuard(b, func() error { return nil }, f)
				}
			case "volume uuid":
				c.machine = func(ctx context.Context, b provider.ExecutionBinding) (machine, error) {
					vm, err := originalMachine(ctx, b)
					calls++
					if calls > 1 {
						vm.VolumeUUID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
					}
					return vm, err
				}
			case "pid reuse", "boot", "cgroup", "exit":
				c.process = func(pid int64, id string) (process, error) {
					p, err := originalProcess(pid, id)
					calls++
					if calls > 1 {
						switch kind {
						case "pid reuse":
							p.StartTime++
						case "boot":
							p.BootID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
						case "cgroup":
							p.CgroupInode++
						case "exit":
							return process{}, errRefused
						}
					}
					return p, err
				}
			case "guard", "cancel before seal":
				c.guard = func(_ context.Context, f func(provider.ExecutionBinding, func() error) (func() error, error)) error {
					return runFixtureGuard(binding(), func() error {
						calls++
						if calls == 3 {
							if kind == "cancel before seal" {
								cancel()
								return nil
							}
							return errRefused
						}
						return nil
					}, f)
				}
			case "request":
				c.process = func(pid int64, id string) (process, error) {
					testWrite(t, path, []byte(requestJSON+"\n"), 0600)
					return originalProcess(pid, id)
				}
			}
			if _, err := c.execute(ctx, path, operations); err == nil {
				t.Fatal("accepted incomplete capture")
			}
			dir := filepath.Join(operations, "aaaaaaaa-0000-4000-8000-000000000001")
			if _, err := os.Stat(filepath.Join(dir, "complete.json")); !os.IsNotExist(err) {
				t.Fatal("published completion for failed capture")
			}
			if _, err := c.execute(context.Background(), path, operations); err == nil {
				t.Fatal("partial attempt was replayed")
			}
		})
	}
}

func TestConcurrentCaptureDoesNotRebind(t *testing.T) {
	c, path, operations := captureFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var count atomic.Int32
	c.platform = func(context.Context, request, provider.ExecutionBinding) (int, error) {
		if count.Add(1) == 1 {
			close(started)
			<-release
		}
		return 99, nil
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var first error
	go func() { defer wg.Done(); _, first = c.execute(context.Background(), path, operations) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not reach observation")
	}
	if _, err := c.execute(context.Background(), path, operations); err == nil {
		t.Fatal("concurrent capture accepted partial operation")
	}
	close(release)
	wg.Wait()
	if first != nil {
		t.Fatal(first)
	}
	if count.Load() != 2 {
		t.Fatal("duplicate observations")
	}
}

func TestRequestRefusesAmbiguousOrUnboundedIdentity(t *testing.T) {
	for name, raw := range map[string]string{"null": "null", "duplicate": strings.Replace(requestJSON, `"version":1`, `"version":1,"version":1`, 1), "case alias": strings.Replace(requestJSON, `"version":1`, `"version":1,"Version":1`, 1), "fraction": strings.Replace(requestJSON, "9007199254740993", "1.5", 1), "overflow": strings.Replace(requestJSON, "9007199254740993", "9223372036854775808", 1), "null id": strings.Replace(requestJSON, "9007199254740993", "null", 1), "scaleset overflow": strings.Replace(requestJSON, `"garm_scaleset_id":27`, `"garm_scaleset_id":4294967296`, 1), "attempt overflow": strings.Replace(requestJSON, `"attempt":2`, `"attempt":2147483648`, 1), "unknown": strings.Replace(requestJSON, `"version":1`, `"version":1,"command":"evil"`, 1), "trailing": requestJSON + "{}"} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRequest([]byte(raw)); err == nil {
				t.Fatal("accepted malformed request")
			}
		})
	}
}

func TestReadbackRejectsChangedProvenanceAndPartialReceipt(t *testing.T) {
	for _, kind := range []string{"request bytes", "producer", "receipt", "seal", "missing seal", "hardlink", "mode"} {
		t.Run(kind, func(t *testing.T) {
			c, path, operations := captureFixture(t)
			if _, err := c.execute(context.Background(), path, operations); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(operations, "aaaaaaaa-0000-4000-8000-000000000001")
			switch kind {
			case "request bytes":
				testWrite(t, path, []byte(requestJSON+"\n"), 0600)
			case "producer":
				c.producerSHA = strings.Repeat("7", 64)
			case "receipt":
				testWrite(t, filepath.Join(dir, "execution.json"), []byte("{}"), 0600)
			case "seal":
				testWrite(t, filepath.Join(dir, "complete.json"), []byte("{}"), 0600)
			case "missing seal":
				if err := os.Remove(filepath.Join(dir, "complete.json")); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(dir, "request.json"), filepath.Join(filepath.Dir(operations), "link")); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(filepath.Join(dir, "execution.json"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.execute(context.Background(), path, operations); err == nil {
				t.Fatal("readback accepted changed record")
			}
		})
	}
}
