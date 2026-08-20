// Package ftps talks to the printer's file store.
//
// Three things about the printer's vsFTPd make a stock client useless. It
// speaks implicit TLS on 990, so there is no plaintext greeting to upgrade
// from. It sets require_ssl_reuse, so the data connection must resume the
// control connection's TLS session or it is reset without explanation. And it
// starts the data handshake only once the transfer command has been sent, so
// a client that handshakes on connect hangs waiting for a server that is
// waiting for it.
//
// What is here is the little that this service needs — list a directory, read
// one file — rather than a general FTP client.
package ftps

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	// port is the implicit-TLS listener. The printer runs no plaintext one.
	port = "990"
	// user is fixed on every Bambu printer; the access code is the password.
	user = "bblp"

	// pasvFields is the (h1,h2,h3,h4,p1,p2) tuple in a 227 reply.
	pasvFields = 6
	// portShift reassembles the port from that tuple's last two numbers.
	portShift = 256

	// sessionCacheSize only ever holds the control session the data
	// connections resume.
	sessionCacheSize = 4

	// replyCodeLen is the three-digit status; the fourth byte is a space on
	// the final line of a reply and a hyphen on every continuation.
	replyCodeLen = 3

	// Reply classes, by first digit: 1 opens a transfer, 2 completed it, 3
	// wants another command first.
	starting = 1
	ready    = 2
	needMore = 3
)

// Client is one control connection.
type Client struct {
	conn net.Conn
	r    *bufio.Reader
	tls  *tls.Config
	host string
}

// Dial opens and authenticates a control connection.
func Dial(ctx context.Context, host, accessCode string, timeout time.Duration) (*Client, error) {
	cfg := &tls.Config{
		// The printer issues its own certificate for its own LAN address and
		// there is no CA to check it against; the peer is pinned by IP on the
		// local network.
		InsecureSkipVerify: true, //nolint:gosec // see above
		// ServerName is what crypto/tls keys the session cache by. Without it
		// the data connection — a different port — would look like a different
		// server and could not resume, which is the one thing this server
		// insists on.
		ServerName:         host,
		ClientSessionCache: tls.NewLRUClientSessionCache(sessionCacheSize),
		MinVersion:         tls.VersionTLS12,
		// The printer's OpenSSL offers no TLS 1.3, and pinning the ceiling
		// keeps resumption on the ticket path that is known to work here.
		MaxVersion: tls.VersionTLS12,
	}

	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: timeout}, Config: cfg}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("ftps dial: %w", err)
	}
	c := &Client{conn: conn, r: bufio.NewReader(conn), tls: cfg, host: host}
	_ = conn.SetDeadline(time.Now().Add(timeout))

	if _, err := c.expect("", ready); err != nil { // the 220 greeting
		return nil, c.fail(err)
	}
	for _, step := range []struct {
		cmd     string
		classes []byte
	}{
		// 331 asks for the password; a server that wants none answers 230,
		// and both mean the same thing to a client that has one either way.
		{"USER " + user, []byte{ready, needMore}},
		{"PASS " + accessCode, []byte{ready}},
		// Protect the data channel too: PROT C would send file bytes in the
		// clear, and this server would rather not.
		{"PBSZ 0", []byte{ready}},
		{"PROT P", []byte{ready}},
		{"TYPE I", []byte{ready}},
	} {
		if _, err := c.expect(step.cmd, step.classes...); err != nil {
			return nil, c.fail(err)
		}
	}
	return c, nil
}

// Close ends the session, politely enough for the log on the other end.
func (c *Client) Close() error {
	_, _ = c.command("QUIT")
	return c.conn.Close()
}

// List returns the raw LIST lines for a directory. A directory that does not
// exist reads as an empty one: this server reports both the same way.
func (c *Client) List(ctx context.Context, dir string, timeout time.Duration) ([]string, error) {
	body, err := c.transfer(ctx, "LIST "+dir, timeout)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(string(body), "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// Retrieve reads one file whole. Callers fetch small things — a preview, a
// manifest — so there is no streaming variant to get wrong.
func (c *Client) Retrieve(ctx context.Context, path string, timeout time.Duration) ([]byte, error) {
	return c.transfer(ctx, "RETR "+path, timeout)
}

// transfer runs one data-channel command, in the order this server requires:
// PASV, connect, send the command, and only then handshake.
func (c *Client) transfer(ctx context.Context, cmd string, timeout time.Duration) ([]byte, error) {
	_ = c.conn.SetDeadline(time.Now().Add(timeout))

	reply, err := c.expect("PASV", ready)
	if err != nil {
		return nil, err
	}
	addr, err := pasvAddr(c.host, reply)
	if err != nil {
		return nil, err
	}

	plain, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ftps data dial: %w", err)
	}
	defer func() { _ = plain.Close() }()
	_ = plain.SetDeadline(time.Now().Add(timeout))

	// The command before the handshake. Reversed, both ends wait for the
	// other and the transfer times out with nothing to show for it.
	if _, err := fmt.Fprintf(c.conn, "%s\r\n", cmd); err != nil {
		return nil, fmt.Errorf("ftps %s: %w", cmd, err)
	}

	data := tls.Client(plain, c.tls)
	if err := data.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("ftps data handshake: %w", err)
	}
	if !data.ConnectionState().DidResume {
		// Worth naming rather than letting the read fail: this is the failure
		// that looks like a network fault and is not one.
		return nil, errors.New("ftps data channel did not resume the control session")
	}

	// 150, then the bytes, then 226.
	if _, err := c.expect("", starting); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(data)
	_ = data.Close()
	if err != nil {
		return nil, fmt.Errorf("ftps read: %w", err)
	}
	if _, err := c.expect("", ready); err != nil {
		return nil, err
	}
	return body, nil
}

// command sends a line and returns the reply, or reads a reply when the line
// is empty.
func (c *Client) command(cmd string) (string, error) {
	if cmd != "" {
		if _, err := fmt.Fprintf(c.conn, "%s\r\n", cmd); err != nil {
			return "", fmt.Errorf("ftps %s: %w", redact(cmd), err)
		}
	}
	var out strings.Builder
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return out.String(), fmt.Errorf("ftps %s: %w", redact(cmd), err)
		}
		out.WriteString(line)
		if final(line) {
			return out.String(), nil
		}
	}
}

// expect sends a command and requires a reply in one of the given classes, so
// a failure is reported where it happened rather than as a puzzling one later.
func (c *Client) expect(cmd string, classes ...byte) (string, error) {
	reply, err := c.command(cmd)
	if err != nil {
		return reply, err
	}
	for _, class := range classes {
		if reply != "" && reply[0] == '0'+class {
			return reply, nil
		}
	}
	return reply, fmt.Errorf("ftps %s: %s", redact(cmd), strings.TrimSpace(reply))
}

func (c *Client) fail(err error) error {
	_ = c.conn.Close()
	return err
}

// final reports whether a reply line ends the reply: continuations carry a
// hyphen where the last line carries a space.
func final(line string) bool {
	trimmed := strings.TrimRight(line, "\r\n")
	return len(trimmed) > replyCodeLen && trimmed[replyCodeLen] == ' '
}

// pasvAddr reads the data address out of a 227 reply. The host is taken from
// the connection rather than the reply, because a printer behind any kind of
// translation reports an address that is true for it and useless to us.
func pasvAddr(host, reply string) (string, error) {
	open := strings.Index(reply, "(")
	closing := strings.Index(reply, ")")
	if open < 0 || closing < open {
		return "", fmt.Errorf("ftps pasv: %s", strings.TrimSpace(reply))
	}
	fields := strings.Split(reply[open+1:closing], ",")
	if len(fields) != pasvFields {
		return "", fmt.Errorf("ftps pasv: %s", strings.TrimSpace(reply))
	}
	high, err := strconv.Atoi(strings.TrimSpace(fields[4]))
	if err != nil {
		return "", fmt.Errorf("ftps pasv port: %w", err)
	}
	low, err := strconv.Atoi(strings.TrimSpace(fields[5]))
	if err != nil {
		return "", fmt.Errorf("ftps pasv port: %w", err)
	}
	return net.JoinHostPort(host, strconv.Itoa(high*portShift+low)), nil
}

// redact keeps the access code out of error strings, which are logged.
func redact(cmd string) string {
	if strings.HasPrefix(cmd, "PASS ") {
		return "PASS"
	}
	return cmd
}
