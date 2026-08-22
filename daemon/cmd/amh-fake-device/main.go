// Command amh-fake-device is a test/dev fixture only: an ephemeral SSH
// device simulator standing in for a real greenhouse vent actuator, so the
// end-to-end scenario (docs/AMH-SPECIFICATION.md Artifact H, steps 1-4)
// can exercise a genuine SSH protocol round-trip without physical hardware.
//
// SECURITY: this server accepts ANY client public key — there is no
// authentication. That is only acceptable because it is a local,
// ephemeral, test-only fixture; it must never be used as a template for a
// production connector.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

func main() {
	initialOpenPct := flag.Int("initial-open-pct", 40, "initial vent-actuator open_pct state")
	flag.Parse()

	hostPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate host key:", err)
		os.Exit(1)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "host signer:", err)
		os.Exit(1)
	}

	cfg := &ssh.ServerConfig{
		// Test fixture only — see package doc. Never do this in a real connector.
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	cfg.AddHostKey(hostSigner)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}

	state := &ventState{openPct: *initialOpenPct}

	// Machine-parseable startup lines for the orchestrating test/script.
	// HOSTKEY is in authorized_keys format so a caller can pin it directly
	// into connector.config.host_key_authorized_key for real host-key
	// verification — this fixture accepts any client key, but callers
	// should still exercise real host-key pinning against it, not just
	// InsecureIgnoreHostKey, since that's the security-relevant behavior
	// a real connector actually depends on.
	fmt.Printf("LISTEN %s\n", listener.Addr().String())
	fmt.Printf("HOSTKEY %s", string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey())))
	fmt.Println("READY")
	os.Stdout.Sync()

	for {
		nConn, err := listener.Accept()
		if err != nil {
			return
		}
		go handleConn(nConn, cfg, state)
	}
}

type ventState struct {
	mu        sync.Mutex
	openPct   int
	doseCount int
}

func handleConn(nConn net.Conn, cfg *ssh.ServerConfig, state *ventState) {
	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		channel, requests, err := newChan.Accept()
		if err != nil {
			return
		}
		go func() {
			defer channel.Close()
			for req := range requests {
				if req.Type != "exec" {
					if req.WantReply {
						req.Reply(false, nil)
					}
					continue
				}
				cmd := string(req.Payload[4:])
				if req.WantReply {
					req.Reply(true, nil)
				}
				reply := runVentCommand(state, cmd)
				channel.Write([]byte(reply))
				channel.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
				return
			}
		}()
	}
}

func runVentCommand(state *ventState, cmd string) string {
	state.mu.Lock()
	defer state.mu.Unlock()

	switch {
	case cmd == "vent-ctl get-open-pct":
		return strconv.Itoa(state.openPct)
	case strings.HasPrefix(cmd, "vent-ctl set-open-pct "):
		valStr := strings.TrimPrefix(cmd, "vent-ctl set-open-pct ")
		val, err := strconv.Atoi(strings.TrimSpace(valStr))
		if err != nil {
			return "error: invalid value"
		}
		state.openPct = val
		return "ok"
	case strings.HasPrefix(cmd, "dose ") && strings.HasSuffix(cmd, "ml"):
		// The reference irreversible action from
		// contracts/manifests/connector.manifest.yaml's worked example
		// (nutrient-doser.dispense_ml): a consuming action with no undo
		// endpoint. This fixture serves both the reversible vent scenario
		// and this irreversible one so ApprovalGate e2e tests don't need
		// a second device simulator binary.
		state.doseCount++
		return "ok"
	default:
		return "error: unknown command"
	}
}
