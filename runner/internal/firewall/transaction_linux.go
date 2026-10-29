package firewall

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/google/nftables"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

var errUnknown = errors.New("original firewall transaction outcome remains unknown; no batch was resent")

type kernelBinding struct {
	Producer string `json:"producer_sha256"`
	Boot     string `json:"boot_id"`
	Device   uint64 `json:"network_namespace_device"`
	Inode    uint64 `json:"network_namespace_inode"`
}

type firewallIntent struct {
	Version       int           `json:"version"`
	Operation     string        `json:"operation_id"`
	Binding       kernelBinding `json:"binding"`
	Configuration []byte        `json:"configuration"`
	Expected      string        `json:"expected_policy_sha256"`
	Before        beforePolicy  `json:"before"`
}

type firewallResult struct {
	Version int    `json:"version"`
	Intent  string `json:"intent_sha256"`
	Outcome string `json:"outcome"`
	Policy  string `json:"policy_sha256"`
	Errno   int    `json:"kernel_errno"`
	Stage   string `json:"stage"`
}

type transactionKernel interface {
	Before() (beforePolicy, []*nftables.Table, error)
	Generation() (uint32, error)
	Snapshot() ([]tablePolicy, error)
	Queue([]tablePolicy, []*nftables.Table) error
	Flush() error
	Close() error
}

func (k *kernelSocket) Snapshot() ([]tablePolicy, error) { return readStablePolicy(k.kernelReader) }
func (k *kernelSocket) Queue(policy []tablePolicy, tables []*nftables.Table) error {
	return queuePolicy(k.Conn, policy, tables)
}
func (k *kernelSocket) Flush() error { return k.Conn.Flush() }

func digestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func lowerHex(value string, size int) bool {
	if len(value) != size || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (b kernelBinding) valid() bool {
	return lowerHex(b.Producer, 64) && len(b.Boot) == 36 && b.Boot[8] == '-' && b.Boot[13] == '-' && b.Boot[18] == '-' && b.Boot[23] == '-' && lowerHex(strings.ReplaceAll(b.Boot, "-", ""), 32) && b.Device != 0 && b.Inode != 0
}

func intentPolicy(intent firewallIntent) ([]tablePolicy, error) {
	if intent.Version != 1 || !lowerHex(intent.Operation, 32) || !intent.Binding.valid() {
		return nil, errState
	}
	configuration, err := ParseConfiguration(intent.Configuration)
	if err != nil {
		return nil, err
	}
	marker := []byte("nucleiron/v1:" + intent.Operation + ":" + digestBytes(intent.Configuration) + ":" + intent.Binding.Producer)
	policy, err := configuredPolicy(configuration, marker)
	if err != nil {
		return nil, err
	}
	if intent.Expected != "" {
		expected, err := policyDigest(policy)
		if err != nil || expected != intent.Expected {
			return nil, errState
		}
	}
	return policy, nil
}

func encodeRecord(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// These are immutable records from this producer, not user JSON. Exact
// reconstruction rejects omitted, null, aliased, duplicate and unknown fields.
func decodeRecord(raw []byte, value any) error {
	if json.Unmarshal(raw, value) != nil {
		return errState
	}
	restored, err := encodeRecord(value)
	if err != nil || !bytes.Equal(raw, restored) {
		return errState
	}
	return nil
}

func validateBefore(before beforePolicy) error {
	if len(before.Facts) > 256 {
		return errState
	}
	size := 0
	for _, fact := range before.Facts {
		switch fact.Operation {
		case unix.NFT_MSG_NEWTABLE, unix.NFT_MSG_NEWCHAIN, unix.NFT_MSG_NEWRULE, unix.NFT_MSG_NEWSET, unix.NFT_MSG_NEWSETELEM, unix.NFT_MSG_NEWOBJ, unix.NFT_MSG_NEWFLOWTABLE:
		default:
			return errState
		}
		message := netlink.Message{Header: netlink.Header{Type: netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8 | fact.Operation)}, Data: fact.Data}
		name, err := factName(message, fact.Operation, 1)
		if err != nil || !ownedTable(&nftables.Table{Name: name, Family: nftables.TableFamily(fact.Data[0])}) {
			return errState
		}
		size += len(fact.Data)
		if size > 32*1024 {
			return errState
		}
	}
	return nil
}

func decodeTransaction(raw, resultRaw []byte) (firewallIntent, *firewallResult, error) {
	var intent firewallIntent
	if decodeRecord(raw, &intent) != nil || !lowerHex(intent.Expected, 64) || validateBefore(intent.Before) != nil {
		return intent, nil, errState
	}
	if _, err := intentPolicy(intent); err != nil {
		return intent, nil, err
	}
	if resultRaw == nil {
		return intent, nil, nil
	}
	var result firewallResult
	if decodeRecord(resultRaw, &result) != nil || result.Version != 1 || result.Intent != digestBytes(raw) {
		return intent, nil, errState
	}
	switch result.Outcome {
	case "applied":
		if result.Policy != intent.Expected || result.Errno != 0 || result.Stage != "readback" {
			return intent, nil, errState
		}
	case "rejected":
		if result.Policy != "" || result.Errno < 1 || result.Errno > 4095 || result.Stage != "commit" {
			return intent, nil, errState
		}
	case "not_sent":
		if result.Policy != "" || result.Errno != 0 || result.Stage != "before_commit" {
			return intent, nil, errState
		}
	default:
		return intent, nil, errState
	}
	return intent, &result, nil
}

func loadTransaction(s *firewallFiles) (firewallIntent, []byte, *firewallResult, error) {
	var empty firewallIntent
	entries, err := directoryEntries(s.active)
	if err != nil || len(entries) < 1 || len(entries) > 2 {
		return empty, nil, nil, errState
	}
	for _, entry := range entries {
		if !recordName(entry.Name()) {
			return empty, nil, nil, errState
		}
	}
	raw, err := s.read("intent.json")
	if err != nil {
		return empty, nil, nil, err
	}
	resultRaw, err := s.read("result.json")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return empty, nil, nil, err
	}
	intent, result, err := decodeTransaction(raw, resultRaw)
	if err != nil {
		return intent, nil, nil, err
	}
	// Recovery must make retained complete bytes durable before allowing a
	// subsequent archive or operation. Partial records never authorize reuse.
	return intent, raw, result, syncRecords(s)
}

func validateArchives(s *firewallFiles) error {
	entries, err := directoryEntries(s.parent)
	if err != nil {
		return err
	}
	if len(entries) > 4096 {
		return errors.New("firewall history exceeds observation bound")
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "operation.lock" || name == "active" {
			continue
		}
		id, ok := strings.CutPrefix(name, "completed-")
		if !ok || !lowerHex(id, 32) {
			return errState
		}
		if err := validateArchive(s, name, id); err != nil {
			return err
		}
	}
	return nil
}

func validateArchive(s *firewallFiles, name, id string) error {
	dir, err := protectedAt(s.parent, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer dir.Close()
	if !protectedMetadata(dir, s.uid, unix.S_IFDIR|0700) {
		return errState
	}
	entries, err := directoryEntries(dir)
	if err != nil || len(entries) != 2 {
		return errState
	}
	for _, entry := range entries {
		if !recordName(entry.Name()) {
			return errState
		}
	}
	read := func(name string) ([]byte, error) {
		f, err := protectedAt(dir, name, unix.O_RDONLY, 0)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return protectedBytes(f, s.uid, 64*1024)
	}
	raw, err := read("intent.json")
	if err != nil {
		return err
	}
	resultRaw, err := read("result.json")
	if err != nil {
		return err
	}
	intent, result, err := decodeTransaction(raw, resultRaw)
	if err != nil {
		return err
	}
	if result == nil || intent.Operation != id {
		return errState
	}
	return nil
}

func syncRecords(s *firewallFiles) error {
	for _, retained := range s.retained {
		if err := s.sync(retained.file); err != nil {
			return err
		}
	}
	if err := s.sync(s.active); err != nil {
		return err
	}
	if err := s.sync(s.parent); err != nil {
		return err
	}
	if !s.unchanged() {
		return errState
	}
	return nil
}

// A raw errno in netlink.OpError is a kernel NLMSG_ERROR; syscall failures
// carry *os.SyscallError instead. Buffer loss and interrupted observation do
// not prove rejection. The socket has no other outstanding write requests.
func kernelRejection(err error) syscall.Errno {
	if op, ok := err.(*netlink.OpError); ok && op.Op == "receive" {
		if errno, ok := op.Err.(syscall.Errno); ok && errno > 0 && errno <= 4095 && errno != unix.ENOBUFS && errno != unix.EINTR && errno != unix.EAGAIN {
			return errno
		}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if errno := kernelRejection(cause); errno != 0 {
				return errno
			}
		}
	} else if cause := errors.Unwrap(err); cause != nil {
		return kernelRejection(cause)
	}
	return 0
}

func retainResult(s *firewallFiles, intentRaw []byte, outcome, policy string, errno int) error {
	stage := map[string]string{"applied": "readback", "rejected": "commit", "not_sent": "before_commit"}[outcome]
	raw, err := encodeRecord(firewallResult{Version: 1, Intent: digestBytes(intentRaw), Outcome: outcome, Policy: policy, Errno: errno, Stage: stage})
	if err != nil {
		return err
	}
	return s.create("result.json", raw)
}

func reloadTransaction(ctx context.Context, configuration []byte, binding kernelBinding, s *firewallFiles, open func(context.Context) (transactionKernel, error), admit func() error) (outcome error) {
	if _, err := ParseConfiguration(configuration); err != nil {
		return err
	}
	if !binding.valid() || !s.unchanged() {
		return errState
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateArchives(s); err != nil {
		return err
	}
	exists, err := s.openActive()
	if err != nil {
		return err
	}
	var intent firewallIntent
	var intentRaw []byte
	if exists {
		var result *firewallResult
		intent, intentRaw, result, err = loadTransaction(s)
		if err != nil {
			return err
		}
		if result != nil {
			if err := s.archive(intent.Operation); err != nil {
				return err
			}
		} else {
			// A changed source, configuration, boot or namespace cannot reserve
			// another slot or resend this original unresolved batch.
			if intent.Binding != binding || !bytes.Equal(intent.Configuration, configuration) {
				return errUnknown
			}
			k, err := open(ctx)
			if err != nil {
				return err
			}
			defer func() { outcome = errors.Join(outcome, k.Close()) }()
			policy, err := k.Snapshot()
			if err != nil {
				return errors.Join(errUnknown, err)
			}
			got, err := policyDigest(policy)
			if err != nil || got != intent.Expected {
				return errors.Join(errUnknown, err)
			}
			return retainResult(s, intentRaw, "applied", got, 0)
		}
	}
	if err := admit(); err != nil {
		return err
	}
	k, err := open(ctx)
	if err != nil {
		return err
	}
	defer func() { outcome = errors.Join(outcome, k.Close()) }()
	before, tables, err := k.Before()
	if err != nil {
		return err
	}
	if err := validateBefore(before); err != nil {
		return err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	intent = firewallIntent{Version: 1, Operation: hex.EncodeToString(id[:]), Binding: binding, Configuration: append([]byte(nil), configuration...), Before: before}
	policy, err := intentPolicy(intent)
	if err != nil {
		return err
	}
	intent.Expected, err = policyDigest(policy)
	if err != nil {
		return err
	}
	if err := k.Queue(policy, tables); err != nil {
		return err
	}
	if err := s.reserve(); err != nil {
		return err
	}
	intentRaw, err = encodeRecord(intent)
	if err != nil {
		return err
	}
	if err := s.create("intent.json", intentRaw); err != nil {
		return err
	}
	notSent := func(err error) error { return errors.Join(err, retainResult(s, intentRaw, "not_sent", "", 0)) }
	if err := ctx.Err(); err != nil {
		return notSent(err)
	}
	if !s.unchanged() {
		return notSent(errState)
	}
	gen, err := k.Generation()
	if err != nil {
		return notSent(err)
	}
	if gen != before.Generation {
		return notSent(errors.New("firewall changed before commit; original intent retained without sending"))
	}
	if err := ctx.Err(); err != nil {
		return notSent(err)
	}
	if err := admit(); err != nil {
		return notSent(err)
	}
	if !s.unchanged() {
		return notSent(errState)
	}
	if err := k.Flush(); err != nil {
		if errno := kernelRejection(err); errno != 0 {
			return errors.Join(fmt.Errorf("kernel rejected firewall transaction: %w", err), retainResult(s, intentRaw, "rejected", "", int(errno)))
		}
		return errors.Join(errUnknown, err)
	}
	observed, err := k.Snapshot()
	if err != nil {
		return errors.Join(errUnknown, err)
	}
	got, err := policyDigest(observed)
	if err != nil || got != intent.Expected {
		return errors.Join(errUnknown, err)
	}
	return retainResult(s, intentRaw, "applied", got, 0)
}
