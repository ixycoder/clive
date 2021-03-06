// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssh

import (
	"errors"
	"fmt"
	"net"
	"sync"
)

// Client implements a traditional SSH client that supports shells,
// subprocesses, port forwarding and tunneled dialing.
struct Client {
	Conn

	forwards        forwardList // forwarded tcpip connections from the remote side
	mu              sync.Mutex
	channelHandlers map[string]chan NewChannel
}

// HandleChannelOpen returns a channel on which NewChannel requests
// for the given type are sent. If the type already is being handled,
// nil is returned. The channel is closed when the connection is closed.
func (c *Client) HandleChannelOpen(channelType string) <-chan NewChannel {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.channelHandlers == nil {
		// The SSH channel has been closed.
		c := make(chan NewChannel)
		close(c)
		return c
	}

	ch := c.channelHandlers[channelType]
	if ch != nil {
		return nil
	}

	ch = make(chan NewChannel, 16)
	c.channelHandlers[channelType] = ch
	return ch
}

// NewClient creates a Client on top of the given connection.
func NewClient(c Conn, chans <-chan NewChannel, reqs <-chan *Request) *Client {
	conn := &Client{
		Conn:            c,
		channelHandlers: make(map[string]chan NewChannel, 1),
	}

	go conn.handleGlobalRequests(reqs)
	go conn.handleChannelOpens(chans)
	go func() {
		conn.Wait()
		conn.forwards.closeAll()
	}()
	go conn.forwards.handleChannels(conn.HandleChannelOpen("forwarded-tcpip"))
	return conn
}

// NewClientConn establishes an authenticated SSH connection using c
// as the underlying transport.  The Request and NewChannel channels
// must be serviced or the connection will hang.
func NewClientConn(c net.Conn, addr string, config *ClientConfig) (Conn, <-chan NewChannel, <-chan *Request, error) {
	fullConf := *config
	fullConf.SetDefaults()
	conn := &connection{
		sshConn: sshConn{conn: c},
	}

	if err := conn.clientHandshake(addr, &fullConf); err != nil {
		c.Close()
		return nil, nil, nil, fmt.Errorf("ssh: handshake failed: %v", err)
	}
	conn.mux = newMux(conn.transport)
	return conn, conn.mux.incomingChannels, conn.mux.incomingRequests, nil
}

// clientHandshake performs the client side key exchange. See RFC 4253 Section
// 7.
func (c *connection) clientHandshake(dialAddress string, config *ClientConfig) error {
	c.clientVersion = []byte(packageVersion)
	if config.ClientVersion != "" {
		c.clientVersion = []byte(config.ClientVersion)
	}

	var err error
	c.serverVersion, err = exchangeVersions(c.sshConn.conn, c.clientVersion)
	if err != nil {
		return err
	}

	c.transport = newClientTransport(
		newTransport(c.sshConn.conn, config.Rand, true /* is client */),
		c.clientVersion, c.serverVersion, config, dialAddress, c.sshConn.RemoteAddr())
	if err := c.transport.requestKeyChange(); err != nil {
		return err
	}

	if packet, err := c.transport.readPacket(); err != nil {
		return err
	} else if packet[0] != msgNewKeys {
		return unexpectedMessageError(msgNewKeys, packet[0])
	}
	return c.clientAuthenticate(config)
}

// verifyHostKeySignature verifies the host key obtained in the key
// exchange.
func verifyHostKeySignature(hostKey PublicKey, result *kexResult) error {
	sig, rest, ok := parseSignatureBody(result.Signature)
	if len(rest) > 0 || !ok {
		return errors.New("ssh: signature parse error")
	}

	return hostKey.Verify(result.H, sig)
}

// NewSession opens a new Session for this client. (A session is a remote
// execution of a program.)
func (c *Client) NewSession() (*Session, error) {
	ch, in, err := c.OpenChannel("session", nil)
	if err != nil {
		return nil, err
	}
	return newSession(ch, in)
}

func (c *Client) handleGlobalRequests(incoming <-chan *Request) {
	for r := range incoming {
		// This handles keepalive messages and matches
		// the behaviour of OpenSSH.
		r.Reply(false, nil)
	}
}

// handleChannelOpens channel open messages from the remote side.
func (c *Client) handleChannelOpens(in <-chan NewChannel) {
	for ch := range in {
		c.mu.Lock()
		handler := c.channelHandlers[ch.ChannelType()]
		c.mu.Unlock()

		if handler != nil {
			handler <- ch
		} else {
			ch.Reject(UnknownChannelType, fmt.Sprintf("unknown channel type: %v", ch.ChannelType()))
		}
	}

	c.mu.Lock()
	for _, ch := range c.channelHandlers {
		close(ch)
	}
	c.channelHandlers = nil
	c.mu.Unlock()
}

// parseTCPAddr parses the originating address from the remote into a *net.TCPAddr.
// RFC 4254 section 7.2 is mute on what to do if parsing fails but the forwardlist
// requires a valid *net.TCPAddr to operate, so we enforce that restriction here.
func parseTCPAddr(b []byte) (*net.TCPAddr, []byte, bool) {
	addr, b, ok := parseString(b)
	if !ok {
		return nil, b, false
	}
	port, b, ok := parseUint32(b)
	if !ok || port == 0 || port > 65535 {
		return nil, b, false
	}
	ip := net.ParseIP(string(addr))
	if ip == nil {
		return nil, b, false
	}
	return &net.TCPAddr{IP: ip, Port: int(port)}, b, true
}

// Dial starts a client connection to the given SSH server. It is a
// convenience function that connects to the given network address,
// initiates the SSH handshake, and then sets up a Client.  For access
// to incoming channels and requests, use net.Dial with NewClientConn
// instead.
func Dial(network, addr string, config *ClientConfig) (*Client, error) {
	conn, err := net.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := NewClientConn(conn, addr, config)
	if err != nil {
		return nil, err
	}
	return NewClient(c, chans, reqs), nil
}

// A ClientConfig structure is used to configure a Client. It must not be
// modified after having been passed to an SSH function.
struct ClientConfig {
	// Config contains configuration that is shared between clients and
	// servers.
	Config

	// User contains the username to authenticate as.
	User string

	// Auth contains possible authentication methods to use with the
	// server. Only the first instance of a particular RFC 4252 method will
	// be used during authentication.
	Auth []AuthMethod

	// HostKeyCallback, if not nil, is called during the cryptographic
	// handshake to validate the server's host key. A nil HostKeyCallback
	// implies that all host keys are accepted.
	HostKeyCallback func(hostname string, remote net.Addr, key PublicKey) error

	// ClientVersion contains the version identification string that will
	// be used for the connection. If empty, a reasonable default is used.
	ClientVersion string
}
