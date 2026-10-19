package engine

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"net/netip"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func testKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSFTPEndpointIsUntouchedUnlessIPv4OnlyIsOn(t *testing.T) {
	SetIPv4Only(false)
	known := knownhosts.Line([]string{"[box.example]:23"}, testKey(t)) + "\n"
	host, got, err := sftpEndpoint(context.Background(), "box.example", 23, known)
	if err != nil || host != "box.example" || got != known {
		t.Fatalf("got %q %q %v, want the host and known_hosts unchanged", host, got, err)
	}
	if tcpNetwork() != "tcp" {
		t.Fatalf("network %q, want tcp", tcpNetwork())
	}
}

func TestIPv4OnlyPinsTheKeyUnderTheAddress(t *testing.T) {
	SetIPv4Only(true)
	t.Cleanup(func() { SetIPv4Only(false) })
	if tcpNetwork() != "tcp4" {
		t.Fatalf("network %q, want tcp4", tcpNetwork())
	}
	key := testKey(t)
	known := knownhosts.Line([]string{"[box.example]:23"}, key) + "\n"

	// An address stands for itself; the known_hosts content is kept as given.
	host, got, err := sftpEndpoint(context.Background(), "192.0.2.7", 23, known)
	if err != nil || host != "192.0.2.7" || got != known {
		t.Fatalf("literal address: %q %q %v", host, got, err)
	}
	if _, _, err := sftpEndpoint(context.Background(), "2001:db8::1", 23, known); err == nil {
		t.Fatal("an IPv6 literal was accepted with IPv4 only on")
	}

	// A key recorded under the name also checks out under the address Kopia dials.
	withIP, err := withAddrLines(known, netip.MustParseAddr("192.0.2.7"), 23)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withIP, "[box.example]:23") {
		t.Fatalf("the original line is gone: %q", withIP)
	}
	cb, err := hostKeyCallback(withIP)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("192.0.2.7:23", &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 23}, key); err != nil {
		t.Fatalf("the pinned key was refused under the address: %v", err)
	}
	if err := cb("192.0.2.7:23", &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 23}, testKey(t)); err == nil {
		t.Fatal("a different key was accepted under the address")
	}
}
