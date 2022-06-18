package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"

	uuid "github.com/satori/go.uuid"
	log "github.com/sirupsen/logrus"
)

// client connection
type ClientConn struct {
	Id   uuid.UUID
	Conn net.Conn
	Tls  bool
}

func newClientConn(c net.Conn) *ClientConn {
	return &ClientConn{
		Id:   uuid.NewV4(),
		Conn: c,
		Tls:  false,
	}
}

// server connection
type ServerConn struct {
	Id      uuid.UUID
	Address string
	Conn    net.Conn

	tlsHandshaked   chan struct{}
	tlsHandshakeErr error
	tlsConn         *tls.Conn
	tlsState        *tls.ConnectionState
	client          *http.Client
}

func newServerConn() *ServerConn {
	return &ServerConn{
		Id:            uuid.NewV4(),
		tlsHandshaked: make(chan struct{}),
	}
}

func (c *ServerConn) TlsState() *tls.ConnectionState {
	<-c.tlsHandshaked
	return c.tlsState
}

// connection context ctx key
var connContextKey = new(struct{})

// connection context
type ConnContext struct {
	ClientConn *ClientConn
	ServerConn *ServerConn

	proxy    *Proxy
	pipeConn *pipeConn
}

func newConnContext(c net.Conn, proxy *Proxy) *ConnContext {
	clientConn := newClientConn(c)
	return &ConnContext{
		ClientConn: clientConn,
		proxy:      proxy,
	}
}

func (connCtx *ConnContext) initHttpServerConn() {
	if connCtx.ServerConn != nil {
		return
	}
	if connCtx.ClientConn.Tls {
		return
	}

	serverConn := newServerConn()
	serverConn.client = &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				cw := &wrapServerConn{
					Conn:    c,
					proxy:   connCtx.proxy,
					connCtx: connCtx,
				}
				serverConn.Conn = cw
				serverConn.Address = addr
				defer func() {
					for _, addon := range connCtx.proxy.Addons {
						addon.ServerConnected(connCtx)
					}
				}()
				return cw, nil
			},
			ForceAttemptHTTP2:  false, // disable http2
			DisableCompression: true,  // To get the original response from the server, set Transport.DisableCompression to true.
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: connCtx.proxy.Opts.SslInsecure,
				KeyLogWriter:       getTlsKeyLogWriter(),
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 禁止自动重定向
			return http.ErrUseLastResponse
		},
	}
	connCtx.ServerConn = serverConn
}

func (connCtx *ConnContext) initServerTcpConn() error {
	log.Debugln("in initServerTcpConn")
	ServerConn := newServerConn()
	connCtx.ServerConn = ServerConn
	ServerConn.Address = connCtx.pipeConn.host

	plainConn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", ServerConn.Address)
	if err != nil {
		return err
	}
	ServerConn.Conn = &wrapServerConn{
		Conn:    plainConn,
		proxy:   connCtx.proxy,
		connCtx: connCtx,
	}

	for _, addon := range connCtx.proxy.Addons {
		addon.ServerConnected(connCtx)
	}

	return nil
}

func (connCtx *ConnContext) initHttpsServerConn() {
	if !connCtx.ClientConn.Tls {
		return
	}
	connCtx.ServerConn.client = &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				<-connCtx.ServerConn.tlsHandshaked
				return connCtx.ServerConn.tlsConn, connCtx.ServerConn.tlsHandshakeErr
			},
			ForceAttemptHTTP2:  false, // disable http2
			DisableCompression: true,  // To get the original response from the server, set Transport.DisableCompression to true.
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 禁止自动重定向
			return http.ErrUseLastResponse
		},
	}
}

func (connCtx *ConnContext) getCertificate(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cfg := &tls.Config{
		InsecureSkipVerify: connCtx.proxy.Opts.SslInsecure,
		KeyLogWriter:       getTlsKeyLogWriter(),
		ServerName:         clientHello.ServerName,
		NextProtos:         []string{"http/1.1"}, // todo: h2
		// CurvePreferences:   clientHello.SupportedCurves, // todo: 如果打开会出错
		CipherSuites: clientHello.CipherSuites,
	}
	if len(clientHello.SupportedVersions) > 0 {
		minVersion := clientHello.SupportedVersions[0]
		maxVersion := clientHello.SupportedVersions[0]
		for _, version := range clientHello.SupportedVersions {
			if version < minVersion {
				minVersion = version
			}
			if version > maxVersion {
				maxVersion = version
			}
		}
		cfg.MinVersion = minVersion
		cfg.MaxVersion = maxVersion
	}

	tlsConn := tls.Client(connCtx.ServerConn.Conn, cfg)
	err := tlsConn.HandshakeContext(context.Background())
	if err != nil {
		connCtx.ServerConn.tlsHandshakeErr = err
		close(connCtx.ServerConn.tlsHandshaked)
		return nil, err
	}

	connCtx.ServerConn.tlsConn = tlsConn
	tlsState := tlsConn.ConnectionState()
	connCtx.ServerConn.tlsState = &tlsState
	close(connCtx.ServerConn.tlsHandshaked)

	// todo: change here
	return connCtx.proxy.interceptor.(*middle).ca.GetCert(clientHello.ServerName)
}

// wrap tcpConn for remote client
type wrapClientConn struct {
	net.Conn
	proxy    *Proxy
	connCtx  *ConnContext
	closed   bool
	closeErr error
}

func (c *wrapClientConn) Close() error {
	log.Debugln("in wrapClientConn close")
	if c.closed {
		return c.closeErr
	}

	c.closed = true
	c.closeErr = c.Conn.Close()

	for _, addon := range c.proxy.Addons {
		addon.ClientDisconnected(c.connCtx.ClientConn)
	}

	if c.connCtx.ServerConn != nil && c.connCtx.ServerConn.Conn != nil {
		c.connCtx.ServerConn.Conn.Close()
	}

	return c.closeErr
}

// wrap tcpListener for remote client
type wrapListener struct {
	net.Listener
	proxy *Proxy
}

func (l *wrapListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	return &wrapClientConn{
		Conn:  c,
		proxy: l.proxy,
	}, nil
}

// wrap tcpConn for remote server
type wrapServerConn struct {
	net.Conn
	proxy    *Proxy
	connCtx  *ConnContext
	closed   bool
	closeErr error
}

func (c *wrapServerConn) Close() error {
	log.Debugln("in wrapServerConn close")
	if c.closed {
		return c.closeErr
	}

	c.closed = true
	c.closeErr = c.Conn.Close()

	for _, addon := range c.proxy.Addons {
		addon.ServerDisconnected(c.connCtx)
	}

	c.connCtx.ClientConn.Conn.Close()

	return c.closeErr
}