package watchdog

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/godbus/dbus/v5"
)

type paddingFailureReader struct {
	err       error
	readSizes []int
}

func (r *paddingFailureReader) Read(p []byte) (int, error) {
	r.readSizes = append(r.readSizes, len(p))
	return 0, r.err
}

func TestDBusHeaderPaddingReadFailure(t *testing.T) {
	// A fixed little-endian method call avoids the encoder's map iteration order.
	// Its header array ends at byte 76; four padding bytes align the empty body.
	wire, err := hex.DecodeString(
		"6c01000100000000010000003c000000" + // Fixed header: body length 0, serial 1, header array length 60.
			"01016f00010000002f00000000000000" + // Object path "/", then inter-field padding.
			"030173000400000050696e6700000000" + // Member "Ping", then inter-field padding.
			"06017300130000006f72672e6578616d706c652e536572766963650000000000", // Destination and body padding.
	)
	if err != nil {
		t.Fatal(err)
	}
	headerEnd := 16 + int(binary.LittleEndian.Uint32(wire[12:16]))
	if headerEnd != 76 || len(wire) != 80 {
		t.Fatalf("unexpected fixture layout: header end %d, total %d", headerEnd, len(wire))
	}

	t.Run("complete", func(t *testing.T) {
		reader := bytes.NewReader(wire)
		message, err := dbus.DecodeMessageWithFDs(reader, nil)
		if err != nil {
			t.Fatal(err)
		}
		if message == nil || message.Type != dbus.TypeMethodCall ||
			message.Headers[dbus.FieldPath].Value() != dbus.ObjectPath("/") ||
			message.Headers[dbus.FieldMember].Value() != "Ping" ||
			message.Headers[dbus.FieldDestination].Value() != "org.example.Service" ||
			len(message.Body) != 0 || reader.Len() != 0 {
			t.Fatalf("incomplete or unexpected decoded message: %#v, unread %d", message, reader.Len())
		}
	})

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"closed", net.ErrClosed},
		{"wrapped-close", &net.OpError{Op: "read", Net: "unix", Err: net.ErrClosed}},
		{"canceled", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := &paddingFailureReader{err: tc.err}
			reader := io.MultiReader(bytes.NewReader(wire[:headerEnd]), failure)
			// A closing transport reaches this decoder from the SDK's inWorker.
			// The read error must return to that worker, rather than panic out of it.
			message, err := dbus.DecodeMessageWithFDs(reader, nil)
			if message != nil || err != tc.err || !errors.Is(err, tc.err) {
				t.Fatalf("got message %#v and error %v, want original read error %v", message, err, tc.err)
			}
			if len(failure.readSizes) != 1 || failure.readSizes[0] != len(wire)-headerEnd {
				t.Fatalf("error was not read at the four-byte body padding: %v", failure.readSizes)
			}
		})
	}
}
