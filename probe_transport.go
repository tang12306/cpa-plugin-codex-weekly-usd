package main

// Probes have to leave by the same route the credential's real traffic does.
//
// CLIProxyAPI gives each credential its own egress through the `proxy_url` in
// its auth document, and both the request path and the token refresh path
// honour it. The plugin host callback does not: `host.http.do` builds its
// client with a nil auth (internal/pluginhost/host_callbacks.go), so it falls
// back to the global proxy - in practice the host's own address. A probe sent
// that way authenticates the account from an address it otherwise never uses,
// which is exactly the shape of an automated-abuse signal.
//
// There is no way to ask the host for a different egress: pluginapi.HTTPRequest
// carries only method, URL, headers and body. So the plugin dials for itself.
// The SOCKS5 client below is deliberately small - it is the one thing this file
// needs and pulling in a dependency for it would complicate a c-shared build
// for no gain.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	socks5Version    = 0x05
	socks5NoAuth     = 0x00
	socks5UserPass   = 0x02
	socks5CmdConnect = 0x01
	socks5AddrDomain = 0x03
	socks5AddrIPv4   = 0x01
	socks5AddrIPv6   = 0x04
)

// socks5Dial opens a TCP connection to addr through a SOCKS5 proxy, asking the
// proxy to resolve the hostname. Resolving locally would leak the lookup onto
// the host's resolver and, worse, pin the connection to whatever address the
// host's DNS returns rather than the one the proxy's network would pick.
func socks5Dial(ctx context.Context, proxyAddr string, user, pass, addr string) (net.Conn, error) {
	host, portStr, errSplit := net.SplitHostPort(addr)
	if errSplit != nil {
		return nil, fmt.Errorf("socks5: bad target %q: %w", addr, errSplit)
	}
	port, errPort := strconv.Atoi(portStr)
	if errPort != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("socks5: bad port in %q", addr)
	}
	if len(host) > 255 {
		return nil, fmt.Errorf("socks5: hostname too long")
	}

	var d net.Dialer
	conn, errDial := d.DialContext(ctx, "tcp", proxyAddr)
	if errDial != nil {
		return nil, fmt.Errorf("socks5: dial proxy %s: %w", proxyAddr, errDial)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()

	methods := []byte{socks5NoAuth}
	if user != "" {
		methods = []byte{socks5UserPass, socks5NoAuth}
	}
	greeting := append([]byte{socks5Version, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return nil, fmt.Errorf("socks5: greeting: %w", err)
	}

	br := bufio.NewReader(conn)
	reply := make([]byte, 2)
	if _, err := io.ReadFull(br, reply); err != nil {
		return nil, fmt.Errorf("socks5: greeting reply: %w", err)
	}
	if reply[0] != socks5Version {
		return nil, fmt.Errorf("socks5: unexpected version %d", reply[0])
	}
	switch reply[1] {
	case socks5NoAuth:
	case socks5UserPass:
		if user == "" {
			return nil, fmt.Errorf("socks5: proxy demands a password and none is configured")
		}
		if len(user) > 255 || len(pass) > 255 {
			return nil, fmt.Errorf("socks5: credential too long")
		}
		auth := []byte{0x01, byte(len(user))}
		auth = append(auth, user...)
		auth = append(auth, byte(len(pass)))
		auth = append(auth, pass...)
		if _, err := conn.Write(auth); err != nil {
			return nil, fmt.Errorf("socks5: auth: %w", err)
		}
		authReply := make([]byte, 2)
		if _, err := io.ReadFull(br, authReply); err != nil {
			return nil, fmt.Errorf("socks5: auth reply: %w", err)
		}
		if authReply[1] != 0x00 {
			return nil, fmt.Errorf("socks5: proxy rejected the credential")
		}
	default:
		return nil, fmt.Errorf("socks5: proxy offered no method this client supports (%d)", reply[1])
	}

	request := []byte{socks5Version, socks5CmdConnect, 0x00, socks5AddrDomain, byte(len(host))}
	request = append(request, host...)
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err := conn.Write(request); err != nil {
		return nil, fmt.Errorf("socks5: connect: %w", err)
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(br, head); err != nil {
		return nil, fmt.Errorf("socks5: connect reply: %w", err)
	}
	if head[1] != 0x00 {
		return nil, fmt.Errorf("socks5: proxy refused the connection (code %d)", head[1])
	}
	var skip int
	switch head[3] {
	case socks5AddrIPv4:
		skip = 4
	case socks5AddrIPv6:
		skip = 16
	case socks5AddrDomain:
		n := make([]byte, 1)
		if _, err := io.ReadFull(br, n); err != nil {
			return nil, fmt.Errorf("socks5: connect reply address: %w", err)
		}
		skip = int(n[0])
	default:
		return nil, fmt.Errorf("socks5: unknown address type %d in reply", head[3])
	}
	if _, err := io.CopyN(io.Discard, br, int64(skip+2)); err != nil {
		return nil, fmt.Errorf("socks5: connect reply tail: %w", err)
	}
	// Anything the proxy sent past the handshake would now be sitting in br and
	// invisible to the caller. A SOCKS5 server has nothing to say after the
	// reply, so buffered bytes here mean the stream is not what we think it is.
	if br.Buffered() > 0 {
		return nil, fmt.Errorf("socks5: proxy sent %d unexpected bytes after the handshake", br.Buffered())
	}
	_ = conn.SetDeadline(time.Time{})
	ok = true
	return conn, nil
}

// probeClient builds an HTTP client whose egress matches proxyURL. An empty
// proxyURL means the credential has no proxy configured and is meant to leave
// by the host's own address.
func probeClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        2,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}

	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL != "" {
		u, errParse := url.Parse(proxyURL)
		if errParse != nil {
			return nil, fmt.Errorf("bad proxy_url %q: %w", proxyURL, errParse)
		}
		switch strings.ToLower(u.Scheme) {
		case "socks5", "socks5h":
			user := u.User.Username()
			pass, _ := u.User.Password()
			host := u.Host
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				if network != "tcp" && network != "tcp4" && network != "tcp6" {
					return nil, fmt.Errorf("socks5: unsupported network %q", network)
				}
				return socks5Dial(ctx, host, user, pass, addr)
			}
		case "http", "https":
			transport.Proxy = http.ProxyURL(u)
		default:
			// Refusing is the safe direction. Falling back to a direct dial
			// would send the probe from the host's address, which is the whole
			// failure this file exists to prevent.
			return nil, fmt.Errorf("unsupported proxy scheme %q in proxy_url", u.Scheme)
		}
	}

	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

// tokenFingerprint identifies which access token a probe result describes, so a
// rejection can be forgotten the moment someone logs the account back in. It is
// a hash: the token itself must never reach the state file or the panel.
func tokenFingerprint(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// probeTimeout bounds one probe. A probe that has not answered by now tells us
// nothing a longer wait would improve, and holding the tick open past the check
// interval would stack ticks on top of each other.
const probeTimeout = 30 * time.Second

// probeDo performs one probe request through the egress the credential's real
// traffic uses. Only the status and headers are returned: the quota reading
// lives entirely in the headers, and the body is a model response nobody asked
// for. It is drained and discarded so the connection can be reused.
func probeDo(proxyURL, method, url string, headers map[string][]string, body []byte) (int, map[string][]string, error) {
	client, errClient := probeClient(proxyURL, probeTimeout)
	if errClient != nil {
		return 0, nil, errClient
	}
	defer client.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	req, errReq := http.NewRequestWithContext(ctx, method, url, strings.NewReader(string(body)))
	if errReq != nil {
		return 0, nil, errReq
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, errDo := client.Do(req)
	if errDo != nil {
		return 0, nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	// Bounded: upstream streams a response, and reading it to the end would
	// keep a connection open for as long as the model wants to talk.
	_, _ = io.CopyN(io.Discard, resp.Body, 8<<10)

	return resp.StatusCode, resp.Header, nil
}
