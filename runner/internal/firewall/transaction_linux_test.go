package firewall

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/nftables"
	"github.com/mdlayher/netlink"
	"github.com/mdlayher/netlink/nltest"
	"golang.org/x/sys/unix"
)

type fixtureKernel struct {
	current, queued                    []tablePolicy
	sends, opens, closes, reads        int
	flushError                         error
	commit                             bool
	beforeGeneration, commitGeneration uint32
	onFlush                            func() error
}

func (k *fixtureKernel) Before() (beforePolicy, []*nftables.Table, error) {
	return beforePolicy{Generation: k.beforeGeneration}, nil, nil
}
func (k *fixtureKernel) Generation() (uint32, error)                      { return k.commitGeneration, nil }
func (k *fixtureKernel) Snapshot() ([]tablePolicy, error)                 { k.reads++; return k.current, nil }
func (k *fixtureKernel) Queue(p []tablePolicy, _ []*nftables.Table) error { k.queued = p; return nil }
func (k *fixtureKernel) Flush() error {
	k.sends++
	if k.onFlush != nil {
		if err := k.onFlush(); err != nil {
			return err
		}
	}
	if k.commit {
		k.current = k.queued
	}
	return k.flushError
}
func (k *fixtureKernel) Close() error                                    { k.closes++; return nil }
func (k *fixtureKernel) open(context.Context) (transactionKernel, error) { k.opens++; return k, nil }

func bindingFixture() kernelBinding {
	return kernelBinding{Producer: strings.Repeat("a", 64), Boot: "01234567-89ab-cdef-0123-456789abcdef", Device: 4, Inode: 12345}
}
func permitFixture() error { return nil }

func TestTransactionLostAcknowledgementRecoversOriginalWithoutAnotherFlush(t *testing.T) {
	path, s := stateFixture(t)
	k := &fixtureKernel{commit: true, flushError: io.ErrUnexpectedEOF}
	k.onFlush = func() error {
		// Independently read the durable write-ahead record at the send boundary.
		raw, err := os.ReadFile(filepath.Join(path, "active", "intent.json"))
		if err != nil || len(raw) == 0 {
			t.Fatal("Flush ran before original intent existed")
		}
		var intent firewallIntent
		if decodeRecord(raw, &intent) != nil || intent.Operation == "" || intent.Expected == "" {
			t.Fatal("Flush lacked original identity")
		}
		return nil
	}
	if err := reloadTransaction(context.Background(), []byte(finalConfiguration), bindingFixture(), s, k.open, permitFixture); !errors.Is(err, errUnknown) {
		t.Fatalf("lost ACK not unknown: %v", err)
	}
	if k.sends != 1 || k.closes != 1 {
		t.Fatal("original writer not finished exactly once")
	}
	original, err := os.ReadFile(filepath.Join(path, "active", "intent.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	recovered, err := openFirewallFiles(path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := reloadTransaction(context.Background(), []byte(finalConfiguration), bindingFixture(), recovered, k.open, permitFixture); err != nil {
		t.Fatal(err)
	}
	intent, raw, result, err := loadTransaction(recovered)
	if err != nil || result == nil || result.Outcome != "applied" || string(raw) != string(original) || result.Policy != intent.Expected || k.sends != 1 || k.reads != 1 || k.closes != 2 {
		t.Fatalf("recovery changed original or resent: sends=%d reads=%d err=%v", k.sends, k.reads, err)
	}
}

func TestTransactionUnknownCannotBeBypassedByChangedBindingOrConfiguration(t *testing.T) {
	_, s := stateFixture(t)
	k := &fixtureKernel{flushError: io.ErrUnexpectedEOF}
	if err := reloadTransaction(context.Background(), []byte(finalConfiguration), bindingFixture(), s, k.open, permitFixture); !errors.Is(err, errUnknown) {
		t.Fatal(err)
	}
	for _, change := range []func(*kernelBinding, *[]byte){
		func(b *kernelBinding, _ *[]byte) { b.Producer = strings.Repeat("b", 64) },
		func(b *kernelBinding, _ *[]byte) { b.Inode++ },
		func(_ *kernelBinding, c *[]byte) { *c = append(*c, ' ') },
	} {
		binding, configuration := bindingFixture(), []byte(finalConfiguration)
		change(&binding, &configuration)
		if err := reloadTransaction(context.Background(), configuration, binding, s, k.open, permitFixture); !errors.Is(err, errUnknown) {
			t.Fatalf("changed target bypassed original: %v", err)
		}
	}
	if k.opens != 1 || k.sends != 1 {
		t.Fatal("changed source opened or resent kernel transaction")
	}
	if err := reloadTransaction(context.Background(), []byte(finalConfiguration), bindingFixture(), s, k.open, permitFixture); !errors.Is(err, errUnknown) || k.sends != 1 {
		t.Fatal("unconfirmed original was resent")
	}
}

func TestTransactionInvalidInputAndDurabilityFailureNeverFlush(t *testing.T) {
	_, s := stateFixture(t)
	k := &fixtureKernel{commit: true}
	if err := reloadTransaction(context.Background(), []byte(`{}`), bindingFixture(), s, k.open, permitFixture); err == nil || k.opens != 0 {
		t.Fatal("invalid configuration reached kernel")
	}
	failure := errors.New("original file sync failed")
	s.sync = func(file *os.File) error {
		if file.Name() == "intent.json" {
			return failure
		}
		return file.Sync()
	}
	if err := reloadTransaction(context.Background(), []byte(finalConfiguration), bindingFixture(), s, k.open, permitFixture); !errors.Is(err, failure) || k.sends != 0 {
		t.Fatalf("unsynced intent reached Flush: %v", err)
	}
}

func TestTransactionPreCommitFailureIsKnownNotSentAndRetained(t *testing.T) {
	_, s := stateFixture(t)
	k := &fixtureKernel{beforeGeneration: 1, commitGeneration: 2}
	if err := reloadTransaction(context.Background(), []byte(finalConfiguration), bindingFixture(), s, k.open, permitFixture); err == nil {
		t.Fatal("changed precondition accepted")
	}
	_, _, result, err := loadTransaction(s)
	if err != nil || result == nil || result.Outcome != "not_sent" || k.sends != 0 {
		t.Fatalf("lost known not-sent result: %v", err)
	}
}

func TestTransactionKernelRejectionAndFreshExplicitReloadPreserveOriginal(t *testing.T) {
	path, s := stateFixture(t)
	k := &fixtureKernel{flushError: &netlink.OpError{Op: "receive", Err: unix.EINVAL}}
	if err := reloadTransaction(context.Background(), []byte(finalConfiguration), bindingFixture(), s, k.open, permitFixture); !errors.Is(err, unix.EINVAL) {
		t.Fatal("lost kernel rejection")
	}
	intent, raw, result, err := loadTransaction(s)
	if err != nil || result == nil || result.Outcome != "rejected" || result.Errno != int(unix.EINVAL) {
		t.Fatalf("lost confirmed rejection: %v", err)
	}
	if len(k.current) != 0 {
		t.Fatal("rejected fixture changed previous policy")
	}
	k.flushError, k.commit = nil, true
	if err := reloadTransaction(context.Background(), []byte(finalConfiguration), bindingFixture(), s, k.open, permitFixture); err != nil {
		t.Fatal(err)
	}
	archived, err := os.ReadFile(filepath.Join(path, "completed-"+intent.Operation, "intent.json"))
	if err != nil || string(archived) != string(raw) || k.sends != 2 {
		t.Fatal("new explicit reload lost or replayed original operation")
	}
	current, _, _, err := loadTransaction(s)
	if err != nil || current.Operation == intent.Operation {
		t.Fatal("new transaction reused original identity")
	}
}

func TestTransactionRejectsPartialOrChangedResultBeforeKernelAccess(t *testing.T) {
	path, s := stateFixture(t)
	k := &fixtureKernel{commit: true}
	if err := reloadTransaction(context.Background(), []byte(finalConfiguration), bindingFixture(), s, k.open, permitFixture); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "active", "result.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reloadTransaction(context.Background(), []byte(finalConfiguration), bindingFixture(), s, k.open, permitFixture); err == nil || k.opens != 1 {
		t.Fatal("changed result authorized another operation")
	}
}

func TestKernelRejectionClassificationUsesNativeSDKErrorShape(t *testing.T) {
	conn, err := nftables.New(nftables.WithTestDial(func(messages []netlink.Message) ([]netlink.Message, error) {
		if len(messages) == 0 {
			return nil, io.EOF
		}
		return nltest.Error(int(unix.EINVAL), messages[1:2])
	}))
	if err != nil {
		t.Fatal(err)
	}
	conn.AddTable(&nftables.Table{Name: "nucleiron", Family: nftables.TableFamilyINet})
	if errno := kernelRejection(conn.Flush()); errno != unix.EINVAL {
		t.Fatalf("native SDK NACK not classified: %v", errno)
	}
	for _, err := range []error{
		&netlink.OpError{Op: "receive", Err: os.NewSyscallError("recvmsg", unix.EINVAL)},
		&netlink.OpError{Op: "send", Err: unix.EINVAL},
		&netlink.OpError{Op: "receive", Err: unix.ENOBUFS},
		io.ErrUnexpectedEOF,
		context.Canceled,
	} {
		if kernelRejection(err) != 0 {
			t.Fatal("transport failure became confirmed rejection")
		}
	}
}

func TestTransactionUnknownCannotBeHiddenInAnArchiveDirectory(t *testing.T) {
	path, s := stateFixture(t)
	k := &fixtureKernel{flushError: io.ErrUnexpectedEOF}
	if err := reloadTransaction(context.Background(), []byte(finalConfiguration), bindingFixture(), s, k.open, permitFixture); !errors.Is(err, errUnknown) {
		t.Fatal(err)
	}
	intent, _, _, err := loadTransaction(s)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Rename(filepath.Join(path, "active"), filepath.Join(path, "completed-"+intent.Operation)); err != nil {
		t.Fatal(err)
	}
	reopened, err := openFirewallFiles(path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reloadTransaction(context.Background(), []byte(finalConfiguration), bindingFixture(), reopened, k.open, permitFixture); err == nil || k.opens != 1 || k.sends != 1 {
		t.Fatal("renamed unknown authorized a fresh operation")
	}
}

func TestTransactionCompleteResultAfterSyncFailureLoadsLocally(t *testing.T) {
	path, s := stateFixture(t)
	k := &fixtureKernel{commit: true}
	failure := errors.New("result sync interrupted")
	s.sync = func(f *os.File) error {
		if f.Name() == "result.json" {
			return failure
		}
		return f.Sync()
	}
	if err := reloadTransaction(context.Background(), []byte(finalConfiguration), bindingFixture(), s, k.open, permitFixture); !errors.Is(err, failure) || k.sends != 1 {
		t.Fatal("missing persistence failure")
	}
	s.Close()
	reopened, err := openFirewallFiles(path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	exists, err := reopened.openActive()
	if err != nil || !exists {
		t.Fatal("lost original slot")
	}
	_, _, result, err := loadTransaction(reopened)
	if err != nil || result == nil || result.Outcome != "applied" || k.opens != 1 || k.reads != 1 {
		t.Fatal("local receipt recovery reread or resent kernel operation")
	}
}

func TestTransactionEmptyResultNeverBecomesAnAbsentResultOnRepeatedRead(t *testing.T) {
	path, s := stateFixture(t)
	k := &fixtureKernel{flushError: io.ErrUnexpectedEOF}
	if err := reloadTransaction(context.Background(), []byte(finalConfiguration), bindingFixture(), s, k.open, permitFixture); !errors.Is(err, errUnknown) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "active", "result.json"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, _, _, err := loadTransaction(s); err == nil {
			t.Fatal("empty result accepted as absent")
		}
	}
}
