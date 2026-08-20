package ssh

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

var errAuthFailed = errors.New("public key not authorized")

// startTestSSHServer runs a minimal in-process SSH server that accepts one
// key and executes "exec" requests by writing a scripted reply to stdout,
// so the connector can be exercised over a real SSH protocol round-trip
// without depending on an external sshd.
func startTestSSHServer(t *testing.T, clientKey ssh.Signer, scripted map[string]string) (addr string, hostKey ssh.Signer) {
	t.Helper()

	hostPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}

	serverCfg := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) != string(clientKey.PublicKey().Marshal()) {
				return nil, errAuthFailed
			}
			return nil, nil
		},
	}
	serverCfg.AddHostKey(hostSigner)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			nConn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleConn(t, nConn, serverCfg, scripted)
		}
	}()

	return listener.Addr().String(), hostSigner
}

func handleConn(t *testing.T, nConn net.Conn, cfg *ssh.ServerConfig, scripted map[string]string) {
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
				// exec payload is a length-prefixed string; skip the 4-byte length.
				cmd := string(req.Payload[4:])
				if req.WantReply {
					req.Reply(true, nil)
				}
				reply, ok := scripted[cmd]
				if !ok {
					reply = "unscripted: " + cmd
				}
				channel.Write([]byte(reply))
				channel.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
				return
			}
		}()
	}
}

func TestConnectorRunShell_RealProtocolRoundTrip(t *testing.T) {
	clientPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPriv)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}

	addr, hostSigner := startTestSSHServer(t, clientSigner, map[string]string{
		"vent-ctl get-open-pct": "40",
	})

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	conn, err := New(Config{
		Host:      host,
		Port:      port,
		User:      "amh",
		Signer:    clientSigner,
		HostKeyCB: ssh.FixedHostKey(hostSigner.PublicKey()),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := conn.RunShell(ctx, "vent-ctl get-open-pct")
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	if out != "40" {
		t.Fatalf("expected '40', got %q", out)
	}
}
