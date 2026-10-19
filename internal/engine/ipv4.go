package engine

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// ipv4Only makes SFTP connections use IPv4 only. It is off unless the operator
// turns it on (VB_IPV4_ONLY): a host without IPv6 whose resolver sometimes
// answers with only an AAAA record fails with "network is unreachable" instead
// of using IPv4.
var ipv4Only atomic.Bool

// SetIPv4Only turns the IPv4-only SFTP switch on or off for this process.
func SetIPv4Only(on bool) { ipv4Only.Store(on) }

// tcpNetwork is the network our own SFTP dials ask for.
func tcpNetwork() string {
	if ipv4Only.Load() {
		return "tcp4"
	}
	return "tcp"
}

// sftpEndpoint is the host Kopia's SFTP storage should dial, and the
// known_hosts content to check it against. Kopia dials "tcp" with no way to
// pick the family, so with the switch on we look the name up for IPv4 ourselves
// and hand Kopia the address, and add the pinned host key under that address
// too: the key is the same, only the name it was recorded under differs. Off,
// both come back unchanged.
func sftpEndpoint(ctx context.Context, host string, port int, knownHosts string) (string, string, error) {
	if !ipv4Only.Load() {
		return host, knownHosts, nil
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if !a.Is4() {
			return "", "", fmt.Errorf("IPv4 only is on, but %s is an IPv6 address", host)
		}
		return host, knownHosts, nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return "", "", fmt.Errorf("IPv4 only is on, and looking up an IPv4 address for %s failed: %w", host, err)
	}
	if len(addrs) == 0 {
		return "", "", fmt.Errorf("IPv4 only is on, but %s has no IPv4 address", host)
	}
	known, err := withAddrLines(knownHosts, addrs[0], port)
	if err != nil {
		return "", "", err
	}
	return addrs[0].String(), known, nil
}

// withAddrLines returns known_hosts content with every key in it also listed
// under addr:port.
func withAddrLines(content string, addr netip.Addr, port int) (string, error) {
	var b strings.Builder
	b.WriteString(content)
	if content != "" && !strings.HasSuffix(content, "\n") {
		b.WriteByte('\n')
	}
	host := knownhosts.Normalize(net.JoinHostPort(addr.String(), strconv.Itoa(port)))
	rest := []byte(content)
	for len(rest) > 0 {
		_, _, key, _, r, err := ssh.ParseKnownHosts(rest)
		if err != nil {
			break // no more entries: io.EOF ends the loop; an unreadable one is the storage's to report
		}
		rest = r
		b.WriteString(knownhosts.Line([]string{host}, key) + "\n")
	}
	return b.String(), nil
}
