package sudph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/AudriusButkevicius/pfilter"
	"github.com/SkycoinProject/dmsg"
	"github.com/SkycoinProject/dmsg/cipher"
	"github.com/SkycoinProject/skycoin/src/util/logging"
	"github.com/xtaci/kcp-go"

	"github.com/SkycoinProject/skywire-mainnet/internal/packetfilter"
	"github.com/SkycoinProject/skywire-mainnet/pkg/snet/arclient"
	"github.com/SkycoinProject/skywire-mainnet/pkg/snet/directtransport"
	"github.com/SkycoinProject/skywire-mainnet/pkg/snet/directtransport/porter"
)

// Type is sudp hole punch type.
const Type = "sudph"

// DialTimeout represents a timeout for dialing.
// TODO: Find best value.
const DialTimeout = 30 * time.Second

const HolePunchMessage = "holepunch"

// ErrTimeout indicates a timeout.
var ErrTimeout = errors.New("timeout")

// Client is the central control for incoming and outgoing 'sudp.Conn's.
type Client struct {
	log *logging.Logger

	lPK             cipher.PubKey
	lSK             cipher.SecKey
	p               *porter.Porter
	addressResolver arclient.APIClient

	localUDPAddr        string
	listenerConn        net.PacketConn
	visorConn           net.PacketConn
	addressResolverConn net.PacketConn
	packetFilter        *pfilter.PacketFilter

	lUDP net.Listener
	lMap map[uint16]*directtransport.Listener // key: lPort
	mx   sync.Mutex

	done chan struct{}
	once sync.Once
}

// NewClient creates a net Client.
func NewClient(pk cipher.PubKey, sk cipher.SecKey, addressResolver arclient.APIClient) *Client {
	c := &Client{
		log:             logging.MustGetLogger(Type),
		lPK:             pk,
		lSK:             sk,
		addressResolver: addressResolver,
		p:               porter.New(porter.PorterMinEphemeral),
		lMap:            make(map[uint16]*directtransport.Listener),
		done:            make(chan struct{}),
	}

	return c
}

// SetLogger sets a logger for Client.
func (c *Client) SetLogger(log *logging.Logger) {
	c.log = log
}

// Serve serves the listening portion of the client.
func (c *Client) Serve() error {
	if c.listenerConn != nil {
		return errors.New("already listening")
	}

	c.log.Infof("Serving SUDPH client")

	ctx := context.Background()
	network := "udp"

	lAddr, err := net.ResolveUDPAddr(network, "")
	if err != nil {
		return fmt.Errorf("net.ResolveUDPAddr (local): %w", err)
	}

	c.localUDPAddr = lAddr.String() // TODO(nkryuchkov): remove?

	c.log.Infof("SUDPH: Resolved local addr from %v to %v", "", lAddr)

	rAddr, err := net.ResolveUDPAddr(network, c.addressResolver.RemoteUDPAddr())
	if err != nil {
		return err
	}

	c.log.Infof("SUDPH dialing udp from %v to %v", lAddr, rAddr)

	listenerConn, err := net.ListenUDP(network, lAddr)
	if err != nil {
		return err
	}

	c.listenerConn = listenerConn

	c.packetFilter = pfilter.NewPacketFilter(listenerConn)
	c.visorConn = c.packetFilter.NewConn(100, nil)
	c.addressResolverConn = c.packetFilter.NewConn(10, packetfilter.NewAddressFilter(rAddr))

	c.packetFilter.Start()

	_, localPort, err := net.SplitHostPort(c.addressResolverConn.LocalAddr().String())
	if err != nil {
		return err
	}

	c.log.Infof("SUDPH Local port: %v", localPort)

	arKCPConn, err := kcp.NewConn(c.addressResolver.RemoteUDPAddr(), nil, 0, 0, c.addressResolverConn)
	if err != nil {
		return err
	}

	c.log.Infof("SUDPH updating local UDP addr from %v to %v", c.localUDPAddr, arKCPConn.LocalAddr().String())

	// TODO(nkryuchkov): consider moving some parts to address-resolver client
	emptyAddr := dmsg.Addr{PK: cipher.PubKey{}, Port: 0}
	hs := directtransport.InitiatorHandshake(c.lSK, dmsg.Addr{PK: c.lPK, Port: 0}, emptyAddr)

	connConfig := directtransport.ConnConfig{
		Log:       c.log,
		Conn:      arKCPConn,
		LocalPK:   c.lPK,
		LocalSK:   c.lSK,
		Deadline:  time.Now().Add(directtransport.HandshakeTimeout),
		Handshake: hs,
		Encrypt:   false,
		Initiator: true,
	}

	arConn, err := directtransport.NewConn(connConfig)
	if err != nil {
		return fmt.Errorf("newConn: %w", err)
	}

	addrCh, err := c.addressResolver.BindSUDPH(ctx, arConn, localPort)
	if err != nil {
		return err
	}

	go func() {
		for addr := range addrCh {
			udpAddr, err := net.ResolveUDPAddr("udp", addr.Addr)
			if err != nil {
				c.log.WithError(err).Errorf("Failed to resolve UDP address %q", addr)
				continue
			}

			// TODO(nkryuchkov): More robust solution
			c.log.Infof("Sending hole punch packet to %v", addr)
			if _, err := c.visorConn.WriteTo([]byte(HolePunchMessage), udpAddr); err != nil {
				c.log.WithError(err).Errorf("Failed to send hole punch packet to %v", udpAddr)
				continue
			}

			c.log.Infof("Sent hole punch packet to %v", addr)
		}
	}()

	lUDP, err := kcp.ServeConn(nil, 0, 0, c.visorConn)
	if err != nil {
		return err
	}

	c.lUDP = lUDP
	addr := lUDP.Addr()
	c.log.Infof("listening on udp addr: %v", addr)

	c.log.Infof("bound BindSUDPH to %v", c.addressResolver.LocalTCPAddr())

	go func() {
		for {
			if err := c.acceptUDPConn(); err != nil {
				c.log.Warnf("failed to accept incoming connection: %v", err)

				if !directtransport.IsHandshakeError(err) {
					c.log.Warnf("stopped serving sudpr")
					return
				}
			}
		}
	}()

	return nil
}

func (c *Client) dialVisor(visorData arclient.VisorData) (net.Conn, error) {
	if visorData.IsLocal {
		for _, host := range visorData.Addresses {
			addr := net.JoinHostPort(host, visorData.Port)
			conn, err := c.dialTimeout(addr)
			if err == nil {
				return conn, nil
			}
		}
	}

	return c.dialTimeout(visorData.RemoteAddr)
}

func (c *Client) dialTimeout(addr string) (net.Conn, error) {
	timer := time.NewTimer(DialTimeout)
	defer timer.Stop()

	c.log.Infof("Dialing %v from %v via udp", addr, c.addressResolver.LocalTCPAddr())

	for {
		select {
		case <-timer.C:
			return nil, ErrTimeout
		default:
			conn, err := c.dialUDP(addr)
			if err == nil {
				c.log.Infof("Dialed %v from %v", addr, c.addressResolver.LocalTCPAddr())
				return conn, nil
			}

			c.log.WithError(err).
				Warnf("Failed to dial %v from %v, trying again: %v", addr, c.addressResolver.LocalTCPAddr(), err)
		}
	}
}

func (c *Client) dialUDP(remoteAddr string) (net.Conn, error) {
	c.log.Infof("SUDPH c.localUDPAddr: %q", c.localUDPAddr)

	// TODO(nkryuchkov): Dial using listener conn?
	lAddr, err := net.ResolveUDPAddr("udp", c.localUDPAddr)
	if err != nil {
		return nil, fmt.Errorf("net.ResolveUDPAddr (local): %w", err)
	}

	rAddr, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		return nil, fmt.Errorf("net.ResolveUDPAddr (remote): %w", err)
	}

	c.log.Infof("SUDPH: Resolved local addr from %v to %v", c.localUDPAddr, lAddr)

	dialConn := c.packetFilter.NewConn(20, packetfilter.NewKCPConversationFilter())

	// TODO(nkryuchkov): More robust solution
	if _, err := dialConn.WriteTo([]byte(HolePunchMessage), rAddr); err != nil {
		return nil, fmt.Errorf("dialConn.WriteTo: %w", err)
	}

	kcpConn, err := kcp.NewConn(remoteAddr, nil, 0, 0, dialConn)
	if err != nil {
		return nil, err
	}

	return kcpConn, nil
}

func (c *Client) acceptUDPConn() error {
	if c.isClosed() {
		return io.ErrClosedPipe
	}

	udpConn, err := c.lUDP.Accept()
	if err != nil {
		return err
	}

	remoteAddr := udpConn.RemoteAddr()

	c.log.Infof("Accepted connection from %v", remoteAddr)

	var lis *directtransport.Listener

	hs := directtransport.ResponderHandshake(func(f2 directtransport.Frame2) error {
		c.mx.Lock()
		defer c.mx.Unlock()

		var ok bool
		if lis, ok = c.lMap[f2.DstAddr.Port]; !ok {
			return errors.New("not listening on given port")
		}

		return nil
	})

	connConfig := directtransport.ConnConfig{
		Log:       c.log,
		Conn:      udpConn,
		LocalPK:   c.lPK,
		LocalSK:   c.lSK,
		Deadline:  time.Now().Add(directtransport.HandshakeTimeout),
		Handshake: hs,
		FreePort:  nil,
		Encrypt:   true,
		Initiator: false,
	}

	conn, err := directtransport.NewConn(connConfig)
	if err != nil {
		return err
	}

	return lis.Introduce(conn)
}

// Dial dials a new sudph.Conn to specified remote public key and port.
func (c *Client) Dial(ctx context.Context, rPK cipher.PubKey, rPort uint16) (*directtransport.Conn, error) {
	if c.isClosed() {
		return nil, io.ErrClosedPipe
	}

	c.log.Infof("Dialing PK %v", rPK)

	visorData, err := c.addressResolver.ResolveSUDPH(ctx, rPK)
	if err != nil {
		return nil, fmt.Errorf("resolve PK (holepunch): %w", err)
	}

	c.log.Infof("Resolved PK %v to visor data %v, dialing", rPK, visorData)

	udpConn, err := c.dialVisor(visorData)
	if err != nil {
		return nil, err
	}

	c.log.Infof("Dialed %v:%v@%v", rPK, rPort, udpConn.RemoteAddr())

	lPort, freePort, err := c.p.ReserveEphemeral(ctx)
	if err != nil {
		return nil, fmt.Errorf("ReserveEphemeral: %w", err)
	}

	hs := directtransport.InitiatorHandshake(c.lSK, dmsg.Addr{PK: c.lPK, Port: lPort}, dmsg.Addr{PK: rPK, Port: rPort})

	connConfig := directtransport.ConnConfig{
		Log:       c.log,
		Conn:      udpConn,
		LocalPK:   c.lPK,
		LocalSK:   c.lSK,
		Deadline:  time.Now().Add(directtransport.HandshakeTimeout),
		Handshake: hs,
		FreePort:  freePort,
		Encrypt:   true,
		Initiator: true,
	}

	sudpConn, err := directtransport.NewConn(connConfig)
	if err != nil {
		return nil, fmt.Errorf("newConn: %w", err)
	}

	return sudpConn, nil
}

// Listen creates a new listener for sudp hole punch.
// The created Listener cannot actually accept remote connections unless Serve is called beforehand.
func (c *Client) Listen(lPort uint16) (*directtransport.Listener, error) {
	if c.isClosed() {
		return nil, io.ErrClosedPipe
	}

	ok, freePort := c.p.Reserve(lPort)
	if !ok {
		return nil, errors.New("port is already occupied")
	}

	c.mx.Lock()
	defer c.mx.Unlock()

	lAddr := dmsg.Addr{PK: c.lPK, Port: lPort}
	lis := directtransport.NewListener(lAddr, freePort)
	c.lMap[lPort] = lis

	return lis, nil
}

// Close closes the Client.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}

	c.once.Do(func() {
		close(c.done)

		c.mx.Lock()
		defer c.mx.Unlock()

		if err := c.addressResolver.Close(); err != nil {
			c.log.WithError(err).Warnf("Failed to close address resolver client")
		}

		for _, lis := range c.lMap {
			_ = lis.Close() // nolint:errcheck
		}
	})

	return nil
}

func (c *Client) isClosed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// Type returns the stream type.
func (c *Client) Type() string {
	return Type
}
