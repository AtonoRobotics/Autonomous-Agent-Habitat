// Package testssh is a test-only helper: an in-process SSH server for
// exercising real connectors against a real SSH protocol round-trip
// without spawning a subprocess. Used by tests that need a live SSH
// target (daemon/api, and originally daemon/connectors/ssh).
package testssh

import (
	"crypto/rand"
	"crypto/rsa"
	"net"
	"testing"

	"golang.org/x/crypto/ssh"
)

// Server is a running in-process SSH server. HostSigner's public key is
// what a real caller must pin via ssh.FixedHostKey to connect securely.
type Server struct {
	Addr       string
	HostSigner ssh.Signer
}

// CommandHandler maps one exec command to its stdout reply.
type CommandHandler func(cmd string) string

// Start launches a server accepting ANY client public key (this is a test
// fixture, not a template for a real connector) and running handle for
// every exec request. Torn down automatically via t.Cleanup.
func Start(t *testing.T, handle CommandHandler) *Server {
	t.Helper()

	hostPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("testssh: generate host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("testssh: host signer: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	cfg.AddHostKey(hostSigner)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("testssh: listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			nConn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleConn(nConn, cfg, handle)
		}
	}()

	return &Server{Addr: listener.Addr().String(), HostSigner: hostSigner}
}

func handleConn(nConn net.Conn, cfg *ssh.ServerConfig, handle CommandHandler) {
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
				channel.Write([]byte(handle(cmd)))
				channel.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
				return
			}
		}()
	}
}
