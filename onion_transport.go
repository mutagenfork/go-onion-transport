package oniontransport

import (
	"context"
	"crypto/rsa"
	"encoding/base32"
	"encoding/pem"
	"fmt"
	"github.com/libp2p/go-libp2p-peer"
	"github.com/yawning/bulb"
	"github.com/yawning/bulb/utils/pkcs1"
	"golang.org/x/net/proxy"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tpt "github.com/libp2p/go-libp2p-transport"
	tptu "github.com/libp2p/go-libp2p-transport-upgrader"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multiaddr-net"
	"github.com/whyrusleeping/mafmt"
)

// IsValidOnionMultiAddr is used to validate that a multiaddr
// is representing a Tor onion service
func IsValidOnionMultiAddr(a ma.Multiaddr) bool {
	if len(a.Protocols()) != 1 {
		return false
	}

	// check for correct network type
	if a.Protocols()[0].Name != "onion" {
		return false
	}

	// split into onion address and port
	addr, err := a.ValueForProtocol(ma.P_ONION)
	if err != nil {
		return false
	}
	split := strings.Split(addr, ":")
	if len(split) != 2 {
		return false
	}

	// onion address without the ".onion" substring
	if len(split[0]) != 16 {
		fmt.Println(split[0])
		return false
	}
	_, err = base32.StdEncoding.DecodeString(strings.ToUpper(split[0]))
	if err != nil {
		return false
	}

	// onion port number
	i, err := strconv.Atoi(split[1])
	if err != nil {
		return false
	}
	if i >= 65536 || i < 1 {
		return false
	}

	return true
}

// OnionTransport implements go-libp2p-transport's Transport interface
type OnionTransport struct {
	controlConn *bulb.Conn
	auth        *proxy.Auth
	keysDir     string
	keys        map[string]*rsa.PrivateKey
	onlyOnion   bool
	laddr       ma.Multiaddr

	// Connection upgrader for upgrading insecure stream connections to
	// secure multiplex connections.
	Upgrader *tptu.Upgrader
}

var _ tpt.Transport = &OnionTransport{}

// NewOnionTransport creates a OnionTransport
//
// controlNet and controlAddr contain the connecting information
// for the tor control port; either TCP or UNIX domain socket.
//
// controlPass contains the optional tor control password
//
// auth contains the socks proxy username and password
// keysDir is the key material for the Tor onion service.
//
// if onlyOnion is true the dialer will only be used to dial out on onion addresses
func NewOnionTransport(controlNet, controlAddr, controlPass string, auth *proxy.Auth, keysDir string, onlyOnion bool) (*OnionTransport, error) {
	conn, err := bulb.Dial(controlNet, controlAddr)
	if err != nil {
		return nil, err
	}
	if err := conn.Authenticate(controlPass); err != nil {
		return nil, fmt.Errorf("authentication failed: %v", err)
	}
	o := OnionTransport{
		controlConn: conn,
		auth:        auth,
		keysDir:     keysDir,
		onlyOnion:   onlyOnion,
	}
	keys, err := o.loadKeys()
	if err != nil {
		return nil, err
	}
	o.keys = keys
	return &o, nil
}

func (t *OnionTransport) Constructor(upgrader *tptu.Upgrader) (*OnionTransport, error) {
	t.Upgrader = upgrader
	return t, nil
}

// Returns a proxy dialer gathered from the control interface.
// This isn't needed for the IPFS transport but it provides
// easy access to Tor for other functions.
func (t *OnionTransport) TorDialer() (proxy.Dialer, error) {
	dialer, err := t.controlConn.Dialer(t.auth)
	if err != nil {
		return nil, err
	}
	return dialer, nil
}

// loadKeys loads keys into our keys map from files in the keys directory
func (t *OnionTransport) loadKeys() (map[string]*rsa.PrivateKey, error) {
	keys := make(map[string]*rsa.PrivateKey)
	absPath, err := filepath.EvalSymlinks(t.keysDir)
	if err != nil {
		return nil, err
	}
	walkpath := func(path string, f os.FileInfo, err error) error {
		if strings.HasSuffix(path, ".onion_key") {
			file, err := os.Open(path)
			defer file.Close()
			if err != nil {
				return err
			}

			key, err := ioutil.ReadFile(path)
			if err != nil {
				return err
			}
			onionName := strings.Replace(filepath.Base(file.Name()), ".onion_key", "", 1)
			block, _ := pem.Decode(key)
			privKey, _, err := pkcs1.DecodePrivateKeyDER(block.Bytes)
			if err != nil {
				return err
			}
			keys[onionName] = privKey
		}
		return nil
	}
	err = filepath.Walk(absPath, walkpath)
	return keys, err
}

// Dial dials a remote peer. It should try to reuse local listener
// addresses if possible but it may choose not to.
func (t *OnionTransport) Dial(ctx context.Context, raddr ma.Multiaddr, p peer.ID) (tpt.Conn, error) {
	dialer, err := t.controlConn.Dialer(t.auth)
	if err != nil {
		return nil, err
	}
	netaddr, err := manet.ToNetAddr(raddr)
	var onionAddress string
	if err != nil {
		onionAddress, err = raddr.ValueForProtocol(ma.P_ONION)
		if err != nil {
			return nil, err
		}
	}
	onionConn := OnionConn{
		transport: tpt.Transport(t),
		laddr:     &t.laddr,
		raddr:     &raddr,
	}
	if onionAddress != "" {
		split := strings.Split(onionAddress, ":")
		onionConn.Conn, err = dialer.Dial("tcp4", split[0]+".onion:"+split[1])
	} else {
		onionConn.Conn, err = dialer.Dial(netaddr.Network(), netaddr.String())
	}
	if err != nil {
		return nil, err
	}
	return t.Upgrader.UpgradeOutbound(ctx, t, &onionConn, p)
}

// Listen listens on the passed multiaddr.
func (t *OnionTransport) Listen(laddr ma.Multiaddr) (tpt.Listener, error) {

	// convert to net.Addr
	netaddr, err := laddr.ValueForProtocol(ma.P_ONION)
	if err != nil {

	}

	// retreive onion service virtport
	addr := strings.Split(netaddr, ":")
	if len(addr) != 2 {
		return nil, fmt.Errorf("failed to parse onion address")
	}

	// convert port string to int
	port, err := strconv.Atoi(addr[1])
	if err != nil {
		return nil, fmt.Errorf("failed to convert onion service port to int")
	}

	onionKey, ok := t.keys[addr[0]]
	if !ok {
		return nil, fmt.Errorf("missing onion service key material for %s", addr[0])
	}

	listener := OnionListener{
		port:  uint16(port),
		key:   onionKey,
		laddr: laddr,
	}

	// setup bulb listener
	_, err = pkcs1.OnionAddr(&onionKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to derive onion ID: %v", err)
	}
	listener.listener, err = t.controlConn.Listener(uint16(port), onionKey)
	if err != nil {
		return nil, err
	}
	t.laddr = laddr

	return &listener, nil
}

// CanDial returns true if this transport knows how to dial the given
// multiaddr.
//
// Returning true does not guarantee that dialing this multiaddr will
// succeed. This function should *only* be used to preemptively filter
// out addresses that we can't dial.
func (t *OnionTransport) CanDial(a ma.Multiaddr) bool {
	if t.onlyOnion {
		// only dial out on onion addresses
		return IsValidOnionMultiAddr(a)
	} else {
		return IsValidOnionMultiAddr(a) || mafmt.TCP.Matches(a)
	}
}

// Protocols returns the list of terminal protocols this transport can dial.
func (t *OnionTransport) Protocols() []int {
	return []int{ma.P_ONION, ma.P_TCP}
}

// Proxy always returns false for the onion transport.
func (t *OnionTransport) Proxy() bool {
	return false
}

// OnionListener implements go-libp2p-transport's Listener interface
type OnionListener struct {
	port      uint16
	key       *rsa.PrivateKey
	laddr     ma.Multiaddr
	listener  net.Listener
	transport tpt.Transport
	Upgrader *tptu.Upgrader
}

// Accept blocks until a connection is received returning
// go-libp2p-transport's Conn interface or an error if
// something went wrong
func (l *OnionListener) Accept() (tpt.Conn, error) {
	conn, err := l.listener.Accept()
	if err != nil {
		return nil, err
	}
	raddr, err := manet.FromNetAddr(conn.RemoteAddr())
	if err != nil {
		return nil, err
	}
	onionConn := OnionConn{
		Conn:      conn,
		transport: l.transport,
		laddr:     &l.laddr,
		raddr:     &raddr,
	}
	return l.Upgrader.UpgradeInbound(context.Background(), l.transport, &onionConn)
}

// Close shuts down the listener
func (l *OnionListener) Close() error {
	return l.listener.Close()
}

// Addr returns the net.Addr interface which represents
// the local multiaddr we are listening on
func (l *OnionListener) Addr() net.Addr {
	netaddr, _ := manet.ToNetAddr(l.laddr)
	return netaddr
}

// Multiaddr returns the local multiaddr we are listening on
func (l *OnionListener) Multiaddr() ma.Multiaddr {
	return l.laddr
}

// OnionConn implement's go-libp2p-transport's Conn interface
type OnionConn struct {
	net.Conn
	transport tpt.Transport
	laddr     *ma.Multiaddr
	raddr     *ma.Multiaddr
}

// Transport returns the OnionTransport associated
// with this OnionConn
func (c *OnionConn) Transport() tpt.Transport {
	return c.transport
}

// LocalMultiaddr returns the local multiaddr for this connection
func (c *OnionConn) LocalMultiaddr() ma.Multiaddr {
	return *c.laddr
}

// RemoteMultiaddr returns the remote multiaddr for this connection
func (c *OnionConn) RemoteMultiaddr() ma.Multiaddr {
	return *c.raddr
}
