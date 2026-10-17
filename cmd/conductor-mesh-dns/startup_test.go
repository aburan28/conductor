package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This exercises the shipped UDP daemon, including flag parsing and startup.
// Its separate binary is deliberately outside the unit coverage profile.
func TestDaemonExplicitIdentityStartsWithoutTailscale(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "conductor-mesh-dns")
	buildContext, cancelBuild := context.WithTimeout(context.Background(), time.Minute)
	defer cancelBuild()
	if output, err := exec.CommandContext(buildContext, "go", "build", "-o", binaryPath, ".").CombinedOutput(); err != nil {
		t.Fatalf("build DNS daemon: %v\n%s", err, output)
	}
	tripwire := filepath.Join(root, "tailscale")
	if err := os.WriteFile(tripwire, []byte("#!/bin/sh\nprintf 'unexpected detection\\n' >> \"$MESH_DNS_TRIPWIRE\"\nexit 91\n"), 0700); err != nil {
		t.Fatal(err)
	}
	probeMarker := filepath.Join(root, "tripwire-probe")
	probe := exec.Command(tripwire)
	probe.Env = append(os.Environ(), "MESH_DNS_TRIPWIRE="+probeMarker)
	if err := probe.Run(); err == nil {
		t.Fatal("Tailscale tripwire did not fail")
	}
	if marker, err := os.ReadFile(probeMarker); err != nil || string(marker) != "unexpected detection\n" {
		t.Fatalf("Tailscale tripwire is ineffective: %q, %v", marker, err)
	}
	for _, address := range []string{"10.88.0.1", "fd88::1"} {
		t.Run(address, func(t *testing.T) {
			// Reserve a dynamic port, then hand it to the native process. A bind
			// collision remains a startup failure rather than being retried away.
			reservation, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			listen := reservation.LocalAddr().String()
			if err := reservation.Close(); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "tailscale-called")
			logFile, err := os.CreateTemp(t.TempDir(), "daemon-log")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := logFile.Close(); err != nil {
					t.Errorf("close daemon log: %v", err)
				}
			}()
			daemonContext, cancelDaemon := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancelDaemon()
			cmd := exec.CommandContext(daemonContext, binaryPath, "--listen", listen, "--hostname", "Leader.INTERNAL", "--address", address, "--mesh-target", "Leader.INTERNAL.")
			cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "MESH_DNS_TRIPWIRE="+marker, "MESH_DNS_ONESHOT=", `MESH_DNS_SERVICES={"_conductor-mesh._tcp.mesh.internal.":[8443]}`)
			cmd.Stdout, cmd.Stderr = logFile, logFile
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var waitErr error
			go func() { waitErr = cmd.Wait(); close(done) }()
			defer func() {
				select {
				case <-done:
				default:
					if err := cmd.Process.Signal(os.Interrupt); err != nil {
						t.Errorf("stop DNS daemon: %v", err)
					}
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						_ = cmd.Process.Kill()
						<-done
						t.Error("DNS daemon required forced kill")
					}
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Errorf("explicit identity invoked Tailscale tripwire: %v", err)
				}
				// Once Wait returns, the process must have released its UDP socket.
				rebound, err := net.ListenPacket("udp4", listen)
				if err != nil {
					t.Errorf("daemon socket was not released: %v", err)
				} else if err := rebound.Close(); err != nil {
					t.Errorf("close rebound socket: %v", err)
				}
			}()
			query := buildQuery(0x6352, "_conductor-mesh._tcp.mesh.internal.", 33)
			deadline := time.Now().Add(5 * time.Second)
			var response []byte
			for {
				select {
				case <-done:
					logs, _ := os.ReadFile(logFile.Name())
					t.Fatalf("DNS daemon exited before readiness: %v\n%s", waitErr, logs)
				default:
				}
				response, err = exchangeDaemonUDP(listen, query)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					logs, _ := os.ReadFile(logFile.Name())
					t.Fatalf("DNS daemon not ready: %v\n%s", err, logs)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if rcode, count := parseHeader(t, response); rcode != 0 || count != 1 {
				t.Fatalf("native daemon SRV response = %d/%d", rcode, count)
			}
			records := parseSRVAnswers(t, response)
			if len(records) != 1 || records[0].target != "leader.internal." || records[0].port != 8443 {
				t.Fatalf("native daemon SRV identity = %+v", records)
			}
			for _, qtype := range []uint16{1, 28} {
				query := buildQuery(0x6353, records[0].target, qtype)
				response, err := exchangeDaemonUDP(listen, query)
				if err != nil {
					t.Fatalf("native address query type %d: %v", qtype, err)
				}
				wantCount := 0
				ip := net.ParseIP(address)
				var wantIP net.IP
				if qtype == 1 {
					wantIP = ip.To4()
				} else if ip.To4() == nil {
					wantIP = ip.To16()
				}
				if wantIP != nil {
					wantCount = 1
				}
				if rcode, count := parseHeader(t, response); rcode != 0 || count != wantCount {
					t.Fatalf("native address family %s type %d = %d/%d", address, qtype, rcode, count)
				}
				if wantIP != nil && (len(response) != len(query)+12+len(wantIP) || !bytes.Equal(response[len(response)-len(wantIP):], wantIP) || binary.BigEndian.Uint16(response[len(response)-len(wantIP)-2:]) != uint16(len(wantIP))) {
					t.Fatalf("native address data mismatch: %x", response)
				}
			}
		})
	}
}

func exchangeDaemonUDP(address string, query []byte) ([]byte, error) {
	conn, err := net.DialTimeout("udp4", address, 250*time.Millisecond)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		return nil, err
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	response := make([]byte, 4096)
	n, err := conn.Read(response)
	if err != nil {
		return nil, err
	}
	if n < 12 || !bytes.Equal(response[:2], query[:2]) {
		return nil, fmt.Errorf("invalid DNS response: %x", response[:n])
	}
	return response[:n], nil
}
