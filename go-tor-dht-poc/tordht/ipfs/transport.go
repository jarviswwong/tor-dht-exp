package ipfs

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/cretz/tor-dht-poc/go-tor-dht-poc/tordht/ipfs/websocket"
	gorillaws "github.com/gorilla/websocket"

	"github.com/whyrusleeping/mafmt"

	"github.com/cretz/bine/tor"
	"github.com/libp2p/go-libp2p-peer"
	"github.com/libp2p/go-libp2p-transport"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr-net"

	upgrader "github.com/libp2p/go-libp2p-transport-upgrader"
)

// impls libp2p's transport.Transport
type TorTransport struct {
	bineTor  *tor.Tor
	conf     *TorTransportConf
	upgrader *upgrader.Upgrader

	dialerLock sync.Mutex
	torDialer  *tor.Dialer
	wsDialer   *gorillaws.Dialer
}

type TorTransportConf struct {
	DialConf  *tor.DialConf
	WebSocket bool
}

var OnionMultiaddrFormat = mafmt.Base(ma.P_ONION)
var TorMultiaddrFormat = mafmt.Or(OnionMultiaddrFormat, mafmt.TCP)

var _ transport.Transport = &TorTransport{}

func NewTorTransport(bineTor *tor.Tor, conf *TorTransportConf) func(*upgrader.Upgrader) *TorTransport {
	return func(upgrader *upgrader.Upgrader) *TorTransport {
		bineTor.Debugf("Creating transport with upgrader: %v", upgrader)
		if conf == nil {
			conf = &TorTransportConf{}
		}
		return &TorTransport{bineTor: bineTor, conf: conf, upgrader: upgrader}
	}
}

func (t *TorTransport) Dial(ctx context.Context, raddr ma.Multiaddr, p peer.ID) (transport.Conn, error) {
	t.bineTor.Debugf("For peer ID %v, dialing %v", p, raddr)
	var addr string
	if onionID, port, err := defaultAddrFormat.onionInfo(raddr); err != nil {
		return nil, err
	} else {
		addr = fmt.Sprintf("%v.onion:%v", onionID, port)
	}
	// Init the dialers
	if err := t.initDialers(ctx); err != nil {
		t.bineTor.Debugf("Failed initializing dialers: %v", err)
		return nil, err
	}
	// Now dial
	var netConn net.Conn
	if t.wsDialer != nil {
		t.bineTor.Debugf("Dialing addr: ws://%v", addr)
		wsConn, _, err := t.wsDialer.Dial("ws://"+addr, nil)
		if err != nil {
			t.bineTor.Debugf("Failed dialing: %v", err)
			return nil, err
		}
		netConn = websocket.NewConn(wsConn, nil)
	} else {
		var err error
		if netConn, err = t.torDialer.DialContext(ctx, "tcp", addr); err != nil {
			t.bineTor.Debugf("Failed dialing: %v", err)
			return nil, err
		}
	}
	// Convert connection
	if manetConn, err := manet.WrapNetConn(netConn); err != nil {
		t.bineTor.Debugf("Failed wrapping the net connection: %v", err)
		return nil, err
	} else if conn, err := t.upgrader.UpgradeOutbound(ctx, t, manetConn, p); err != nil {
		t.bineTor.Debugf("Failed upgrading connection: %v", err)
		return nil, err
	} else {
		return conn, nil
	}
}

func (t *TorTransport) initDialers(ctx context.Context) error {
	t.dialerLock.Lock()
	defer t.dialerLock.Unlock()
	// If already inited, good enough
	if t.torDialer != nil {
		return nil
	}
	var err error
	if t.torDialer, err = t.bineTor.Dialer(ctx, t.conf.DialConf); err != nil {
		return fmt.Errorf("Failed creating tor dialer: %v", err)
	}
	// Create web socket dialer if needed
	if t.conf.WebSocket {
		t.wsDialer = &gorillaws.Dialer{
			NetDial:          t.torDialer.Dial,
			Proxy:            http.ProxyFromEnvironment,
			HandshakeTimeout: 45 * time.Second,
		}
	}
	return nil
}

func (t *TorTransport) CanDial(addr ma.Multiaddr) bool {
	t.bineTor.Debugf("Checking if can dial %v", addr)
	_, _, err := defaultAddrFormat.onionInfo(addr)
	return err == nil
}

func (t *TorTransport) Listen(laddr ma.Multiaddr) (transport.Listener, error) {
	// TODO: support a bunch of config options on this if we want
	t.bineTor.Debugf("Called listen for %v", laddr)
	if val, err := laddr.ValueForProtocol(ONION_LISTEN_PROTO_CODE); err != nil {
		return nil, fmt.Errorf("Unable to get protocol value: %v", err)
	} else if val != "" {
		return nil, fmt.Errorf("Must be '/onionListen', got '/onionListen/%v'", val)
	}
	// Listen with version 3, wait 1 min for bootstrap
	ctx, cancelFn := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancelFn()
	onion, err := t.bineTor.Listen(ctx, &tor.ListenConf{Version3: true})
	if err != nil {
		t.bineTor.Debugf("Failed creating onion service: %v", err)
		return nil, err
	}
	fmt.Printf("[Bine API] Listening on onion: %v\n", onion.String())
	fmt.Printf("[Bine API] LocalListener: %v\n", onion.LocalListener.Addr())
	fmt.Printf("[Bine API] RemotePorts: %v\n", onion.RemotePorts)
	t.bineTor.Debugf("Listening on onion: %v", onion.String())
	// Close it if there is another error in here
	defer func() {
		if err != nil {
			t.bineTor.Debugf("Failed listen after onion creation: %v", err)
			onion.Close()
		}
	}()

	// Return a listener
	manetListen := &manetListener{transport: t, onion: onion, listener: onion}
	addrStr := defaultAddrFormat.onionAddr(onion.ID, onion.RemotePorts[0])
	if t.conf.WebSocket {
		addrStr += "/ws"
	}
	if manetListen.multiaddr, err = ma.NewMultiaddr(addrStr); err != nil {
		return nil, fmt.Errorf("Failed converting onion address: %v", err)
	}
	// If it had websocket, we need to delegate to that
	if t.conf.WebSocket {
		if manetListen.listener, err = websocket.StartNewListener(onion); err != nil {
			return nil, fmt.Errorf("Failed creating websocket: %v", err)
		}
	}

	t.bineTor.Debugf("Completed creating IPFS listener from onion, addr: %v", manetListen.multiaddr)
	return manetListen.Upgrade(t.upgrader), nil
}

func (t *TorTransport) Protocols() []int { return []int{ma.P_TCP, ma.P_ONION, ONION_LISTEN_PROTO_CODE} }
func (t *TorTransport) Proxy() bool      { return true }

type manetListener struct {
	transport *TorTransport
	onion     *tor.OnionService
	multiaddr ma.Multiaddr
	listener  net.Listener
}

func (m *manetListener) Accept() (manet.Conn, error) {
	if c, err := m.listener.Accept(); err != nil {
		return nil, err
	} else {
		ret := &manetConn{Conn: c, localMultiaddr: m.multiaddr}
		if ret.remoteMultiaddr, err = manet.FromNetAddr(c.RemoteAddr()); err != nil {
			return nil, err
		}
		return ret, nil
	}
}
func (m *manetListener) Close() error            { return m.onion.Close() }
func (m *manetListener) Addr() net.Addr          { return m.onion }
func (m *manetListener) Multiaddr() ma.Multiaddr { return m.multiaddr }
func (m *manetListener) Upgrade(u *upgrader.Upgrader) transport.Listener {
	return u.UpgradeListener(m.transport, m)
}

type manetConn struct {
	net.Conn
	localMultiaddr  ma.Multiaddr
	remoteMultiaddr ma.Multiaddr
}

func (m *manetConn) LocalMultiaddr() ma.Multiaddr  { return m.localMultiaddr }
func (m *manetConn) RemoteMultiaddr() ma.Multiaddr { return m.remoteMultiaddr }
