//go:build linux && integration

package firewall

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

const packetTimeout = 350 * time.Millisecond

type packetRequest struct {
	Action     string
	Network    string
	Address    string
	Source     string
	SourcePort int
}
type packetResponse struct {
	PID    int
	OK     bool
	Error  string
	Detail string
}

type packetProcess struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	output  io.ReadCloser
	encoder *json.Encoder
	decoder *json.Decoder
}

func privateIP(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("private fixture ip %v: %v: %s", args, err, out)
	}
}

func (p *packetProcess) request(t *testing.T, request packetRequest) packetResponse {
	t.Helper()
	if err := p.encoder.Encode(request); err != nil {
		t.Fatal(err)
	}
	var response packetResponse
	if err := p.decoder.Decode(&response); err != nil {
		t.Fatalf("packet actor response: %v", err)
	}
	if response.Error != "" {
		t.Fatalf("packet actor: %s", response.Error)
	}
	return response
}

func startPacketProcess(t *testing.T, role, link string) *packetProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	cmd := exec.CommandContext(ctx, "unshare", "--net", executable, "-test.run=^TestReloadInPrivateNamespace$", "-test.timeout=33s")
	cmd.Env = append(os.Environ(), "RUNNER_FIREWALL_ACTOR="+role, "RUNNER_FIREWALL_ACTOR_PARENT_NET="+namespaceIdentity(t, "net"), "RUNNER_FIREWALL_ACTOR_USER="+namespaceIdentity(t, "user"))
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		cancel()
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	if err := cmd.Start(); err != nil {
		_ = input.Close()
		_ = output.Close()
		cancel()
		t.Fatal(err)
	}
	p := &packetProcess{cmd: cmd, input: input, output: output, encoder: json.NewEncoder(input), decoder: json.NewDecoder(output)}
	t.Cleanup(func() {
		_ = input.Close()
		// The actor exits on EOF. Retain a short bounded cleanup even after a
		// failed exchange, then kill and wait the original process if needed.
		returned := make(chan error, 1)
		go func() { returned <- cmd.Wait() }()
		select {
		case err := <-returned:
			if err != nil {
				t.Errorf("packet actor %s exited: %v: %s", role, err, diagnostics.String())
			}
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			err := <-returned
			t.Errorf("packet actor %s did not exit on EOF: %v: %s", role, err, diagnostics.String())
		}
		cancel()
		_ = output.Close()
	})
	var ready packetResponse
	if err := p.decoder.Decode(&ready); err != nil || ready.PID != cmd.Process.Pid || !ready.OK {
		t.Fatalf("packet actor readiness %s: %+v %v", role, ready, err)
	}
	privateIP(t, "link", "add", link, "type", "veth", "peer", "name", link+"p")
	privateIP(t, "link", "set", link+"p", "netns", strconv.Itoa(cmd.Process.Pid))
	privateIP(t, "link", "set", link, "up")
	if role != "outside" {
		privateIP(t, "link", "set", link, "master", "nucleiron0")
	}
	// The peer name is fixed by this fixture, never a product input.
	response := p.request(t, packetRequest{Action: "setup", Address: link + "p"})
	if !response.OK {
		t.Fatal("packet actor setup did not complete")
	}
	return p
}

type echoServers struct {
	closers []io.Closer
	workers sync.WaitGroup
}

func (s *echoServers) tcp(network, address string) error {
	listener, err := net.Listen(network, address)
	if err != nil {
		return err
	}
	s.closers = append(s.closers, listener)
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			s.workers.Add(1)
			go func() {
				defer s.workers.Done()
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(time.Second))
				buffer := make([]byte, 64)
				n, err := connection.Read(buffer)
				if err == nil {
					_, _ = connection.Write(buffer[:n])
				}
			}()
		}
	}()
	return nil
}

func (s *echoServers) udp(address string) error {
	connection, err := net.ListenPacket("udp4", address)
	if err != nil {
		return err
	}
	s.closers = append(s.closers, connection)
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		buffer := make([]byte, 64)
		for {
			n, source, err := connection.ReadFrom(buffer)
			if err != nil {
				return
			}
			_, _ = connection.WriteTo(buffer[:n], source)
		}
	}()
	return nil
}

func (s *echoServers) close() {
	for _, closer := range s.closers {
		_ = closer.Close()
	}
	s.workers.Wait()
}

func probePacket(request packetRequest) bool {
	return probePacketError(request) == nil
}

func probePacketError(request packetRequest) error {
	dialer := net.Dialer{Timeout: packetTimeout}
	if request.Source != "" || request.SourcePort != 0 {
		if request.Network == "udp4" {
			dialer.LocalAddr = &net.UDPAddr{IP: net.ParseIP(request.Source), Port: request.SourcePort}
		} else {
			dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(request.Source), Port: request.SourcePort}
		}
	}
	connection, err := dialer.Dial(request.Network, request.Address)
	if err != nil {
		return err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(packetTimeout))
	const payload = "nucleiron-private-fixture"
	if _, err := io.WriteString(connection, payload); err != nil {
		return err
	}
	response := make([]byte, len(payload))
	_, err = io.ReadFull(connection, response)
	if err != nil {
		return err
	}
	if string(response) != payload {
		return fmt.Errorf("unexpected echo payload %q", response)
	}
	return nil
}

// Read only the disposable actor's network state after an unexpected result.
func privateNetworkState() string {
	var state bytes.Buffer
	for _, args := range [][]string{{"-6", "address", "show"}, {"-6", "route", "show", "table", "all"}, {"-6", "neigh", "show"}} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
		cancel()
		fmt.Fprintf(&state, "ip %v: %v\n%s\n", args, err, out)
	}
	for _, path := range []string{"/proc/sys/net/ipv6/conf/all/forwarding", "/proc/net/snmp6"} {
		value, err := os.ReadFile(path)
		fmt.Fprintf(&state, "%s: %v\n%s\n", path, err, value)
	}
	return state.String()
}

func packetActor(t *testing.T) {
	parentNet, parentUser := directParentNamespaces(t)
	if parentNet != os.Getenv("RUNNER_FIREWALL_ACTOR_PARENT_NET") || parentUser != os.Getenv("RUNNER_FIREWALL_ACTOR_USER") || parentNet == namespaceIdentity(t, "net") || parentUser != namespaceIdentity(t, "user") {
		t.Fatal("packet actor lacks an isolated parent network and shared private user namespace")
	}
	role := os.Getenv("RUNNER_FIREWALL_ACTOR")
	if role != "guest" && role != "peer" && role != "outside" {
		t.Fatal("invalid private packet actor role")
	}
	encoder, decoder := json.NewEncoder(os.Stdout), json.NewDecoder(os.Stdin)
	if err := encoder.Encode(packetResponse{PID: os.Getpid(), OK: true}); err != nil {
		t.Fatal(err)
	}
	var servers echoServers
	defer servers.close()
	configured := false
	for {
		var request packetRequest
		if err := decoder.Decode(&request); err == io.EOF {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		switch request.Action {
		case "setup":
			if configured {
				t.Fatal("duplicate packet actor setup")
			}
			privateIP(t, "link", "set", request.Address, "name", "guest0")
			privateIP(t, "link", "set", "lo", "up")
			privateIP(t, "link", "set", "guest0", "up")
			v4, v6 := "10.203.64.2/24", "fd00:203:64::2/64"
			if role == "peer" {
				v4, v6 = "10.203.64.3/24", "fd00:203:64::3/64"
			}
			if role == "outside" {
				v4, v6 = "10.204.0.2/24", "fd00:204::2/64"
			}
			privateIP(t, "address", "add", v4, "dev", "guest0")
			privateIP(t, "-6", "address", "add", v6, "dev", "guest0", "nodad")
			if role == "outside" {
				privateIP(t, "route", "add", "10.203.64.0/24", "via", "10.204.0.1")
				privateIP(t, "-6", "route", "add", "fd00:203:64::/64", "via", "fd00:204::1")
			} else {
				privateIP(t, "route", "add", "10.204.0.0/24", "via", "10.203.64.1")
				privateIP(t, "-6", "route", "add", "fd00:204::/64", "via", "fd00:203:64::1")
			}
			if role == "guest" {
				privateIP(t, "address", "add", "10.203.65.2/32", "dev", "guest0")
			}
			if err := servers.tcp("tcp4", "0.0.0.0:18080"); err != nil {
				t.Fatal(err)
			}
			if err := servers.tcp("tcp6", "[::]:18080"); err != nil {
				t.Fatal(err)
			}
			configured = true
			if err := encoder.Encode(packetResponse{OK: true}); err != nil {
				t.Fatal(err)
			}
		case "probe":
			if !configured {
				t.Fatal("packet actor not configured")
			}
			probeErr := probePacketError(request)
			response := packetResponse{OK: probeErr == nil}
			if probeErr != nil {
				response.Detail = probeErr.Error()
			}
			if err := encoder.Encode(response); err != nil {
				t.Fatal(err)
			}
		case "diagnose":
			if err := encoder.Encode(packetResponse{OK: true, Detail: privateNetworkState()}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("unsupported private packet actor action")
		}
	}
}

type packetFixture struct{ guest, peer, outside *packetProcess }

func setupPacketFixture(t *testing.T) packetFixture {
	t.Helper()
	privateIP(t, "link", "set", "lo", "up")
	privateIP(t, "link", "add", "nucleiron0", "type", "bridge")
	privateIP(t, "link", "set", "nucleiron0", "up")
	privateIP(t, "address", "add", "10.203.64.1/24", "dev", "nucleiron0")
	privateIP(t, "address", "add", "10.203.64.254/24", "dev", "nucleiron0")
	privateIP(t, "-6", "address", "add", "fd00:203:64::1/64", "dev", "nucleiron0", "nodad")
	// Forwarded packets need a usable router link-local source for neighbor
	// solicitation; automatically generated addresses are still tentative.
	privateIP(t, "-6", "address", "add", "fe80::1/64", "dev", "nucleiron0", "nodad")
	f := packetFixture{guest: startPacketProcess(t, "guest", "guestlink"), peer: startPacketProcess(t, "peer", "peerlink"), outside: startPacketProcess(t, "outside", "outsidelink")}
	privateIP(t, "address", "add", "10.204.0.1/24", "dev", "outsidelink")
	privateIP(t, "-6", "address", "add", "fd00:204::1/64", "dev", "outsidelink", "nodad")
	privateIP(t, "-6", "address", "add", "fe80::1/64", "dev", "outsidelink", "nodad")
	privateIP(t, "route", "add", "10.203.65.2/32", "via", "10.203.64.2", "dev", "nucleiron0")
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1\n"), 0); err != nil {
		t.Fatal(err)
	}
	servers := &echoServers{}
	t.Cleanup(servers.close)
	for _, port := range []int{53, 9443, 3128, 6514, 10080} {
		if err := servers.tcp("tcp4", fmt.Sprintf("0.0.0.0:%d", port)); err != nil {
			t.Fatal(err)
		}
	}
	if err := servers.tcp("tcp6", "[::]:9443"); err != nil {
		t.Fatal(err)
	}
	for _, port := range []int{53, 67} {
		if err := servers.udp(fmt.Sprintf("0.0.0.0:%d", port)); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f packetFixture) check(t *testing.T, stage string, filtered bool) {
	t.Helper()
	type packetCase struct {
		name    string
		actor   *packetProcess
		request packetRequest
		allowed bool
	}
	cases := []packetCase{}
	for _, port := range []int{53, 9443, 3128, 6514} {
		cases = append(cases, packetCase{fmt.Sprintf("TCP %d", port), f.guest, packetRequest{Action: "probe", Network: "tcp4", Address: fmt.Sprintf("10.203.64.1:%d", port)}, true})
	}
	cases = append(cases,
		packetCase{"UDP DNS", f.guest, packetRequest{Action: "probe", Network: "udp4", Address: "10.203.64.1:53"}, true},
		packetCase{"DHCP", f.guest, packetRequest{Action: "probe", Network: "udp4", Address: "10.203.64.1:67", SourcePort: 68}, true},
		packetCase{"DHCP outside subnet", f.guest, packetRequest{Action: "probe", Network: "udp4", Address: "10.203.64.1:67", Source: "10.203.65.2", SourcePort: 68}, true},
		packetCase{"other host port", f.guest, packetRequest{Action: "probe", Network: "tcp4", Address: "10.203.64.1:10080"}, false},
		packetCase{"source outside subnet", f.guest, packetRequest{Action: "probe", Network: "tcp4", Address: "10.203.64.1:9443", Source: "10.203.65.2"}, false},
		packetCase{"destination outside gateway", f.guest, packetRequest{Action: "probe", Network: "tcp4", Address: "10.203.64.254:9443"}, false},
		packetCase{"peer forward", f.guest, packetRequest{Action: "probe", Network: "tcp4", Address: "10.203.64.3:18080"}, false},
		packetCase{"reverse peer forward", f.peer, packetRequest{Action: "probe", Network: "tcp4", Address: "10.203.64.2:18080"}, false},
		packetCase{"outbound routed forward", f.guest, packetRequest{Action: "probe", Network: "tcp4", Address: "10.204.0.2:18080"}, false},
		packetCase{"inbound routed forward", f.outside, packetRequest{Action: "probe", Network: "tcp4", Address: "10.203.64.2:18080"}, false},
		packetCase{"IPv6 bridge input", f.guest, packetRequest{Action: "probe", Network: "tcp6", Address: "[fd00:203:64::1]:9443"}, false},
		packetCase{"IPv6 peer forward", f.guest, packetRequest{Action: "probe", Network: "tcp6", Address: "[fd00:203:64::3]:18080"}, false},
		packetCase{"IPv6 reverse peer forward", f.peer, packetRequest{Action: "probe", Network: "tcp6", Address: "[fd00:203:64::2]:18080"}, false},
		packetCase{"IPv6 outbound routed forward", f.guest, packetRequest{Action: "probe", Network: "tcp6", Address: "[fd00:204::2]:18080"}, false},
		packetCase{"IPv6 inbound routed forward", f.outside, packetRequest{Action: "probe", Network: "tcp6", Address: "[fd00:203:64::2]:18080"}, false},
		packetCase{"unrelated ingress", f.outside, packetRequest{Action: "probe", Network: "tcp4", Address: "10.204.0.1:10080"}, true},
		packetCase{"IPv6 unrelated ingress", f.outside, packetRequest{Action: "probe", Network: "tcp6", Address: "[fd00:204::1]:9443"}, true},
	)
	for _, c := range cases {
		want := !filtered || c.allowed
		response := c.actor.request(t, c.request)
		if response.OK != want {
			actor := c.actor.request(t, packetRequest{Action: "diagnose"})
			t.Fatalf("%s / %s: reachable=%t want=%t: %s\nactor:\n%s\nrouter:\n%s", stage, c.name, response.OK, want, response.Detail, actor.Detail, privateNetworkState())
		}
	}
	if !probePacket(packetRequest{Network: "tcp4", Address: "127.0.0.1:10080"}) {
		t.Fatalf("%s changed unrelated loopback traffic", stage)
	}
	t.Logf("%s: %d packet decisions and loopback matched", stage, len(cases))
}
