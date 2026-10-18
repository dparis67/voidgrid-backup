package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

var errGotKey = errors.New("host key captured")

// FetchHostKey connects to an SSH server just far enough to read its host
// key and returns it as a known_hosts line (trust on first use).
func FetchHostKey(ctx context.Context, host string, port int) (string, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("fetch host key: %w", err)
	}
	defer tidy.CloseWarn("host key connection", conn)
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return "", fmt.Errorf("fetch host key: %w", err)
	}

	var key ssh.PublicKey
	_, _, _, err = ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User: "voidgrid-backup",
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			key = k
			return errGotKey
		},
	})
	if key == nil {
		return "", fmt.Errorf("fetch host key from %s: %w", addr, err)
	}
	return knownhosts.Line([]string{knownhosts.Normalize(addr)}, key) + "\n", nil
}
