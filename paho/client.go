package paho

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/netdata/paho.golang/packets"
)

var (
	DefaultKeepAlive       = 60 * time.Second
	DefaultShutdownTimeout = 10 * time.Second
	DefaultPacketTimeout   = 10 * time.Second
)

type (
	// ClientConfig are the user configurable options for the client, an
	// instance of this struct is passed into NewClient(), not all options
	// are required to be set, defaults are provided for Persistence, MIDs,
	// PacketTimeout and Router.
	ClientConfig struct {
		Conn            net.Conn
		MIDs            MIDService
		AuthHandler     Auther
		Router          Router
		Persistence     Persistence
		PacketTimeout   time.Duration
		ShutdownTimeout time.Duration
		Trace           Trace
		OnClose         func()
	}
	// Client is the struct representing an MQTT client
	Client struct {
		ClientConfig
		// caCtx is used for synchronously handling the connect/connack flow
		// raCtx is used for handling the MQTTv5 authentication exchange.

		connectOnce sync.Once
		ca          *Connack // connection ack.
		cerr        error    // connection error.

		mu             sync.Mutex
		closed         bool
		caCtx          *caContext
		raCtx          *CPContext
		exit           chan struct{}
		done           chan struct{}
		writeq         chan io.WriterTo
		writerDone     chan struct{}
		readerDone     chan struct{}
		pingerDone     chan struct{}
		pong           chan struct{}
		serverProps    CommsProperties
		clientProps    CommsProperties
		serverInflight *semaphore.Weighted
		clientInflight *semaphore.Weighted
	}

	// CommsProperties is a struct of the communication properties that may
	// be set by the server in the Connack and that the client needs to be
	// aware of for future subscribes/publishes
	CommsProperties struct {
		MaximumPacketSize    uint32
		ReceiveMaximum       uint16
		TopicAliasMaximum    uint16
		MaximumQoS           byte
		RetainAvailable      bool
		WildcardSubAvailable bool
		SubIDAvailable       bool
		SharedSubAvailable   bool
	}

	caContext struct {
		Context context.Context
		Return  chan *packets.Connack
	}
)

// NewClient is used to create a new default instance of an MQTT client.
// It returns a pointer to the new client instance.
// The default client uses the provided MessageID and
// StandardRouter implementations, and a noop Persistence.
// These should be replaced if desired before the client is connected.
// client.Conn *MUST* be set to an already connected net.Conn before
// Connect() is called.
func NewClient(conf ClientConfig) *Client {
	c := &Client{
		serverProps: CommsProperties{
			ReceiveMaximum:       65535,
			MaximumQoS:           2,
			MaximumPacketSize:    0,
			TopicAliasMaximum:    0,
			RetainAvailable:      true,
			WildcardSubAvailable: true,
			SubIDAvailable:       true,
			SharedSubAvailable:   true,
		},
		clientProps: CommsProperties{
			ReceiveMaximum:    65535,
			MaximumQoS:        2,
			MaximumPacketSize: 0,
			TopicAliasMaximum: 0,
		},
		exit:         make(chan struct{}),
		done:         make(chan struct{}),
		writeq:       make(chan io.WriterTo),
		writerDone:   make(chan struct{}),
		readerDone:   make(chan struct{}),
		pingerDone:   make(chan struct{}),
		pong:         make(chan struct{}, 1),
		ClientConfig: conf,
	}

	if c.Persistence == nil {
		c.Persistence = &noopPersistence{}
	}
	if c.MIDs == nil {
		c.MIDs = &MIDs{index: make(map[uint16]*CPContext)}
	}
	if c.PacketTimeout == 0 {
		c.PacketTimeout = DefaultPacketTimeout
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.Router == nil {
		c.Router = NewStandardRouter()
	}

	return c
}

// Connect is used to connect the client to a server. It presumes that
// the Client instance already has a working network connection.
// The function takes a pre-prepared Connect packet, and uses that to
// establish an MQTT connection. Assuming the connection completes
// successfully the rest of the client is initiated and the Connack
// returned. Otherwise the failure Connack (if there is one) is returned
// along with an error indicating the reason for the failure to connect.
func (c *Client) Connect(ctx context.Context, cp *Connect) (*Connack, error) {
	if c.Conn == nil {
		return nil, fmt.Errorf("client connection is nil")
	}
	c.connectOnce.Do(func() {
		defer func() {
			if c.cerr != nil {
				c.close()
			}
		}()

		keepalive := cp.KeepAlive
		if keepalive == 0 {
			keepalive = uint16(DefaultKeepAlive / time.Second)
		}
		if cp.Properties != nil {
			if cp.Properties.MaximumPacketSize != nil {
				c.clientProps.MaximumPacketSize = *cp.Properties.MaximumPacketSize
			}
			if cp.Properties.MaximumQOS != nil {
				c.clientProps.MaximumQoS = *cp.Properties.MaximumQOS
			}
			if cp.Properties.ReceiveMaximum != nil {
				c.clientProps.ReceiveMaximum = *cp.Properties.ReceiveMaximum
			}
			if cp.Properties.TopicAliasMaximum != nil {
				c.clientProps.TopicAliasMaximum = *cp.Properties.TopicAliasMaximum
			}
		}

		go c.writer()
		go c.reader()

		connCtx, cf := context.WithTimeout(ctx, c.PacketTimeout)
		defer cf()

		c.caCtx = &caContext{connCtx, make(chan *packets.Connack, 1)}

		ccp := cp.Packet()
		ccp.ProtocolName = "MQTT"
		ccp.ProtocolVersion = 5

		if c.cerr = c.write(ctx, ccp); c.cerr != nil {
			return
		}

		var cap *packets.Connack
		select {
		case <-connCtx.Done():
			c.cerr = connCtx.Err()
			return
		case cap = <-c.caCtx.Return:
		}

		ca := ConnackFromPacketConnack(cap)
		c.ca = ca

		if ca.ReasonCode >= 0x80 {
			var reason string
			if ca.Properties != nil {
				reason = ca.Properties.ReasonString
			}
			c.cerr = fmt.Errorf("failed to connect to server: %s", reason)
			return
		}

		if ca.Properties != nil {
			if ca.Properties.ServerKeepAlive != nil {
				keepalive = *ca.Properties.ServerKeepAlive
			}
			//if ca.Properties.AssignedClientID != "" {
			//	c.ClientID = ca.Properties.AssignedClientID
			//}
			if ca.Properties.ReceiveMaximum != nil {
				c.serverProps.ReceiveMaximum = *ca.Properties.ReceiveMaximum
			}
			if ca.Properties.MaximumQoS != nil {
				c.serverProps.MaximumQoS = *ca.Properties.MaximumQoS
			}
			if ca.Properties.MaximumPacketSize != nil {
				c.serverProps.MaximumPacketSize = *ca.Properties.MaximumPacketSize
			}
			if ca.Properties.TopicAliasMaximum != nil {
				c.serverProps.TopicAliasMaximum = *ca.Properties.TopicAliasMaximum
			}
			c.serverProps.RetainAvailable = ca.Properties.RetainAvailable
			c.serverProps.WildcardSubAvailable = ca.Properties.WildcardSubAvailable
			c.serverProps.SubIDAvailable = ca.Properties.SubIDAvailable
			c.serverProps.SharedSubAvailable = ca.Properties.SharedSubAvailable
		}

		c.serverInflight = semaphore.NewWeighted(int64(c.serverProps.ReceiveMaximum))
		c.clientInflight = semaphore.NewWeighted(int64(c.clientProps.ReceiveMaximum))

		go c.pinger(time.Duration(keepalive) * time.Second)
	})
	return c.ca, c.cerr
}

func (c *Client) waitConnected() {
	var dummy bool
	c.connectOnce.Do(func() {
		dummy = true
	})
	if dummy {
		panic("calling method on Client without Connect() call")
	}
}

func (c *Client) IsAlive() bool {
	c.waitConnected()
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

func (c *Client) Done() <-chan struct{} {
	c.waitConnected()
	return c.done
}

func (c *Client) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	go func() {
		c.traceDebug("closing")

		c.waitConnected()

		close(c.exit)
		<-c.writerDone
		<-c.pingerDone

		c.Conn.Close()
		<-c.readerDone
		close(c.done)

		if c.cerr == nil && c.OnClose != nil {
			// Call OnClose() only when initial connection was successful (and
			// callback provided).
			c.OnClose()
		}
	}()
}

func (c *Client) Shutdown(ctx context.Context) {
	c.waitConnected()
	err := c.write(ctx, packets.NewControlPacket(packets.DISCONNECT))
	if err == nil {
		select {
		case <-c.readerDone:
		case <-time.After(c.ShutdownTimeout):
		}
	}
	c.Close()
}

func (c *Client) Close() {
	c.waitConnected()
	c.close()
	<-c.done
}

var (
	ErrClosed       = fmt.Errorf("client closed")
	ErrNotConnected = fmt.Errorf("client is not connected")
)

func (c *Client) write(ctx context.Context, w io.WriterTo) (err error) {
	t := c.traceSend(w)
	defer func() {
		t.done(err)
	}()
	select {
	case <-c.exit:
		return ErrClosed
	case c.writeq <- w:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) writer() {
	defer func() {
		c.traceDebug("writer stopped")
		close(c.writerDone)
	}()
	for {
		var w io.WriterTo
		select {
		case <-c.exit:
			return
		case w = <-c.writeq:
		}
		_, err := w.WriteTo(c.Conn)
		if err != nil {
			c.fail(fmt.Errorf("write packet error: %w", err))
			return
		}
	}
}

// reader is the Client function that reads and handles incoming
// packets from the server. The function is started as a goroutine
// from Connect(), it exits when it receives a server initiated
// Disconnect, the Stop channel is  or there is an error reading
// a packet from the network connection
func (c *Client) reader() {
	defer func() {
		c.traceDebug("reader stopped")
		close(c.readerDone)
	}()
	ctx := context.Background()
	for {
		t := c.traceRecv()
		recv, err := packets.ReadPacket(c.Conn)
		t.done(recv, err)
		if err == io.EOF {
			c.close()
			return
		}
		if err != nil {
			c.fail(err)
			return
		}

		switch recv.Type {
		case packets.PINGRESP:
			select {
			case <-c.pingerDone:
				// Pinger don't need anything no more.
			case c.pong <- struct{}{}:
			}

		case packets.CONNACK:
			cap := recv.Content.(*packets.Connack)
			// NOTE: No need to acquire a lock for caCtx beacuse its never
			// change.
			if c.caCtx != nil {
				c.caCtx.Return <- cap
			}
		case packets.AUTH:
			ap := recv.Content.(*packets.Auth)
			switch ap.ReasonCode {
			case 0x0:
				if c.AuthHandler != nil {
					go c.AuthHandler.Authenticated()
				}
				c.mu.Lock()
				raCtx := c.raCtx
				c.mu.Unlock()
				if raCtx != nil {
					raCtx.Return <- *recv
				}
			case 0x18:
				if c.AuthHandler != nil {
					pkt := c.AuthHandler.Authenticate(AuthFromPacketAuth(ap)).Packet()
					if err := c.write(ctx, pkt); err != nil {
						c.fail(err)
						return
					}
				}
			}
		case packets.PUBLISH:
			pb := recv.Content.(*packets.Publish)
			go c.Router.Route(pb)
			switch pb.QoS {
			case 1:
				pa := packets.Puback{
					Properties: &packets.Properties{},
					PacketID:   pb.PacketID,
				}
				_ = c.write(ctx, &pa)
			case 2:
				pr := packets.Pubrec{
					Properties: &packets.Properties{},
					PacketID:   pb.PacketID,
				}
				_ = c.write(ctx, &pr)
			}
		case packets.PUBACK, packets.PUBCOMP, packets.SUBACK, packets.UNSUBACK:
			if cpCtx := c.MIDs.Get(recv.PacketID()); cpCtx != nil {
				cpCtx.Return <- *recv
			} else {
				c.traceDebug("received a response for a message ID we don't know")
			}
		case packets.PUBREC:
			if cpCtx := c.MIDs.Get(recv.PacketID()); cpCtx == nil {
				c.traceDebug("received a response for a message ID we don't know")
				pl := packets.Pubrel{
					PacketID:   recv.Content.(*packets.Pubrec).PacketID,
					ReasonCode: 0x92,
				}
				_ = c.write(ctx, &pl)
			} else {
				pr := recv.Content.(*packets.Pubrec)
				if pr.ReasonCode >= 0x80 {
					//Received a failure code, shortcut and return
					cpCtx.Return <- *recv
				} else {
					pl := packets.Pubrel{
						PacketID: pr.PacketID,
					}
					_ = c.write(ctx, &pl)
				}
			}
		case packets.PUBREL:
			//Auto respond to pubrels unless failure code
			pr := recv.Content.(*packets.Pubrel)
			if pr.ReasonCode < 0x80 {
				//Received a failure code, continue
				continue
			} else {
				pc := packets.Pubcomp{
					PacketID: pr.PacketID,
				}
				_ = c.write(ctx, &pc)
			}
		case packets.DISCONNECT:
			c.mu.Lock()
			raCtx := c.raCtx
			c.mu.Unlock()
			if raCtx != nil {
				raCtx.Return <- *recv
			}
			c.fail(fmt.Errorf("received server initiated disconnect"))
			return
		}
	}
}

func (c *Client) pinger(d time.Duration) {
	defer func() {
		c.traceDebug("pinger stopped")
		close(c.pingerDone)
	}()
	var (
		ctx   = context.Background()
		timer = time.NewTimer(d)
		ping  = packets.NewControlPacket(packets.PINGREQ)

		lastPing time.Time
		now      time.Time
	)
	for {
		select {
		case <-c.exit:
			timer.Stop()
			return

		case <-c.pong:
			lastPing = time.Time{}
			continue

		case now = <-timer.C:
			// Time to ping.
		}
		if !lastPing.IsZero() && now.Sub(lastPing) > 2*d {
			c.fail(fmt.Errorf("no pong for %s", now.Sub(lastPing)))
			return
		}
		if err := c.write(ctx, ping); err != nil {
			continue
		}
		if lastPing.IsZero() {
			lastPing = now
		}
		timer.Reset(d)
	}
}

func (c *Client) fail(e error) {
	c.traceDebug("client failed", func(t *DebugTrace) {
		t.Error = e
	})
	c.close()
}

// Authenticate is used to initiate a reauthentication of credentials with the
// server. This function sends the initial Auth packet to start the reauthentication
// then relies on the client AuthHandler managing any further requests from the
// server until either a successful Auth packet is passed back, or a Disconnect
// is received.
func (c *Client) Authenticate(ctx context.Context, a *Auth) (*AuthResponse, error) {
	c.waitConnected()
	c.traceDebug("client initiated reauthentication")

	raCtx := &CPContext{ctx, make(chan packets.ControlPacket, 1)}

	c.mu.Lock()
	if c.raCtx != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("previous auth is still in progress")
	}
	c.raCtx = raCtx
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.raCtx = nil
		c.mu.Unlock()
	}()

	if err := c.write(ctx, a.Packet()); err != nil {
		return nil, err
	}

	var rp packets.ControlPacket
	select {
	case <-ctx.Done():
		if e := ctx.Err(); e == context.DeadlineExceeded {
			c.traceDebug("timeout waiting for Auth to complete")
			return nil, e
		}
	case rp = <-raCtx.Return:
	}

	switch rp.Type {
	case packets.AUTH:
		//If we've received one here it must be successful, the only way
		//to abort a reauth is a server initiated disconnect
		return AuthResponseFromPacketAuth(rp.Content.(*packets.Auth)), nil
	case packets.DISCONNECT:
		return AuthResponseFromPacketDisconnect(rp.Content.(*packets.Disconnect)), nil
	}

	return nil, fmt.Errorf("error with Auth, didn't receive Auth or Disconnect")
}

// Subscribe is used to send a Subscription request to the MQTT server.
// It is passed a pre-prepared Subscribe packet and blocks waiting for
// a response Suback, or for the timeout to fire. Any response Suback
// is returned from the function, along with any errors.
func (c *Client) Subscribe(ctx context.Context, s *Subscribe) (*Suback, error) {
	c.waitConnected()
	if !c.serverProps.WildcardSubAvailable {
		for t := range s.Subscriptions {
			if strings.ContainsAny(t, "#+") {
				// Using a wildcard in a subscription when not supported
				return nil, fmt.Errorf("cannot subscribe to %s, server does not support wildcards", t)
			}
		}
	}
	if !c.serverProps.SubIDAvailable && s.Properties != nil && s.Properties.SubscriptionIdentifier != nil {
		return nil, fmt.Errorf("cannot send subscribe with subID set, server does not support subID")
	}
	if !c.serverProps.SharedSubAvailable {
		for t := range s.Subscriptions {
			if strings.HasPrefix(t, "$share") {
				return nil, fmt.Errorf("cannont subscribe to %s, server does not support shared subscriptions", t)
			}
		}
	}

	c.traceDebug(fmt.Sprintf("subscribing to %+v", s.Subscriptions))

	subCtx, cf := context.WithTimeout(ctx, c.PacketTimeout)
	defer cf()
	cpCtx := &CPContext{subCtx, make(chan packets.ControlPacket, 1)}

	sp := s.Packet()
	sp.PacketID = c.MIDs.Request(cpCtx)
	if err := c.write(ctx, sp); err != nil {
		return nil, err
	}
	c.traceDebug("waiting for SUBACK")
	var sap packets.ControlPacket

	select {
	case <-subCtx.Done():
		if e := subCtx.Err(); e == context.DeadlineExceeded {
			c.traceDebug("timeout waiting for SUBACK")
			return nil, e
		}
	case sap = <-cpCtx.Return:
	}

	if sap.Type != packets.SUBACK {
		return nil, fmt.Errorf("received %d instead of Suback", sap.Type)
	}

	sa := SubackFromPacketSuback(sap.Content.(*packets.Suback))
	switch {
	case len(sa.Reasons) == 1:
		if sa.Reasons[0] >= 0x80 {
			var reason string
			c.traceDebug(fmt.Sprintf(
				"received an error code in Suback: %v", sa.Reasons[0],
			))
			if sa.Properties != nil {
				reason = sa.Properties.ReasonString
			}
			return sa, fmt.Errorf("failed to subscribe to topic: %s", reason)
		}
	default:
		for _, code := range sa.Reasons {
			if code >= 0x80 {
				c.traceDebug(fmt.Sprintf(
					"received an error code in Suback: %v", code,
				))
				return sa, fmt.Errorf("at least one requested subscription failed")
			}
		}
	}

	return sa, nil
}

// Unsubscribe is used to send an Unsubscribe request to the MQTT server.
// It is passed a pre-prepared Unsubscribe packet and blocks waiting for
// a response Unsuback, or for the timeout to fire. Any response Unsuback
// is returned from the function, along with any errors.
func (c *Client) Unsubscribe(ctx context.Context, u *Unsubscribe) (*Unsuback, error) {
	c.waitConnected()
	c.traceDebug(fmt.Sprintf(
		"unsubscribing from %+v", u.Topics,
	))
	unsubCtx, cf := context.WithTimeout(ctx, c.PacketTimeout)
	defer cf()
	cpCtx := &CPContext{unsubCtx, make(chan packets.ControlPacket, 1)}

	up := u.Packet()
	up.PacketID = c.MIDs.Request(cpCtx)
	if err := c.write(ctx, up); err != nil {
		return nil, err
	}
	c.traceDebug("waiting for UNSUBACK")
	var uap packets.ControlPacket

	select {
	case <-unsubCtx.Done():
		if e := unsubCtx.Err(); e == context.DeadlineExceeded {
			c.traceDebug("timeout waiting for UNSUBACK")
			return nil, e
		}
	case uap = <-cpCtx.Return:
	}

	if uap.Type != packets.UNSUBACK {
		return nil, fmt.Errorf("received %d instead of Unsuback", uap.Type)
	}

	ua := UnsubackFromPacketUnsuback(uap.Content.(*packets.Unsuback))
	switch {
	case len(ua.Reasons) == 1:
		if ua.Reasons[0] >= 0x80 {
			var reason string
			c.traceDebug(fmt.Sprintf(
				"received an error code in Unsuback: %v", ua.Reasons[0],
			))
			if ua.Properties != nil {
				reason = ua.Properties.ReasonString
			}
			return ua, fmt.Errorf("failed to unsubscribe from topic: %s", reason)
		}
	default:
		for _, code := range ua.Reasons {
			if code >= 0x80 {
				c.traceDebug(fmt.Sprintf(
					"received an error code in Unsuback: %v", code,
				))
				return ua, fmt.Errorf("at least one requested unsubscribe failed")
			}
		}
	}

	return ua, nil
}

// Publish is used to send a publication to the MQTT server.
// It is passed a pre-prepared Publish packet and blocks waiting for
// the appropriate response, or for the timeout to fire.
// Any response message is returned from the function, along with any errors.
func (c *Client) Publish(ctx context.Context, p *Publish) (_ *PublishResponse, err error) {
	c.waitConnected()
	if p.QoS > c.serverProps.MaximumQoS {
		return nil, fmt.Errorf("cannot send Publish with QoS %d, server maximum QoS is %d", p.QoS, c.serverProps.MaximumQoS)
	}
	if p.Properties != nil && p.Properties.TopicAlias != nil {
		if c.serverProps.TopicAliasMaximum > 0 && *p.Properties.TopicAlias > c.serverProps.TopicAliasMaximum {
			return nil, fmt.Errorf("cannot send publish with TopicAlias %d, server topic alias maximum is %d", *p.Properties.TopicAlias, c.serverProps.TopicAliasMaximum)
		}
	}
	if !c.serverProps.RetainAvailable && p.Retain {
		return nil, fmt.Errorf("cannot send Publish with retain flag set, server does not support retained messages")
	}

	pb := p.Packet()
	t := c.tracePublish(pb)
	defer func() {
		t.done(err)
	}()

	switch p.QoS {
	case 0:
		if err := c.write(ctx, pb); err != nil {
			return nil, err
		}
		return nil, nil
	case 1, 2:
		return c.publishQoS12(ctx, pb)
	}

	return nil, fmt.Errorf("oops")
}

func (c *Client) publishQoS12(ctx context.Context, pb *packets.Publish) (*PublishResponse, error) {
	pubCtx, cf := context.WithTimeout(ctx, c.PacketTimeout)
	defer cf()
	if err := c.serverInflight.Acquire(pubCtx, 1); err != nil {
		return nil, err
	}
	cpCtx := &CPContext{pubCtx, make(chan packets.ControlPacket, 1)}

	pb.PacketID = c.MIDs.Request(cpCtx)
	if err := c.write(ctx, pb); err != nil {
		return nil, err
	}
	var resp packets.ControlPacket

	select {
	case <-pubCtx.Done():
		if e := pubCtx.Err(); e == context.DeadlineExceeded {
			c.traceDebug("timeout waiting for Publish response")
			return nil, e
		}
	case resp = <-cpCtx.Return:
	}

	switch pb.QoS {
	case 1:
		if resp.Type != packets.PUBACK {
			return nil, fmt.Errorf("received %d instead of PUBACK", resp.Type)
		}
		c.serverInflight.Release(1)

		pr := PublishResponseFromPuback(resp.Content.(*packets.Puback))
		if pr.ReasonCode >= 0x80 {
			return pr, fmt.Errorf("error publishing: %s", resp.Content.(*packets.Puback).Reason())
		}
		return pr, nil
	case 2:
		switch resp.Type {
		case packets.PUBCOMP:
			c.serverInflight.Release(1)
			pr := PublishResponseFromPubcomp(resp.Content.(*packets.Pubcomp))
			return pr, nil
		case packets.PUBREC:
			c.serverInflight.Release(1)
			pr := PublishResponseFromPubrec(resp.Content.(*packets.Pubrec))
			return pr, nil
		default:
			return nil, fmt.Errorf("received %d instead of PUBCOMP", resp.Type)
		}
	}

	return nil, fmt.Errorf("ended up with a non QoS1/2 message: %d", pb.QoS)
}

// Disconnect is used to send a Disconnect packet to the MQTT server
// Whether or not the attempt to send the Disconnect packet fails
// (and if it does this function returns any error) the network connection
// is .
func (c *Client) Disconnect(ctx context.Context, d *Disconnect) error {
	c.waitConnected()
	return c.write(ctx, d.Packet())
}
