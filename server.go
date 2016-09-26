package h2tun

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/andrew-d/id"
	"github.com/koding/h2tun/proto"
	"github.com/koding/logging"
	"golang.org/x/net/http2"
)

// TODO document
//
// TODO (phase2) add dynamic allowed client add remove
// TODO (phase2) add ping control message type to measure RTT, see https://godoc.org/github.com/hashicorp/yamux#Session.Ping

type AllowedClient struct {
	ID        id.ID
	Host      string
	Listeners []net.Listener
}

// ServerConfig is Server configuration object.
type ServerConfig struct {
	// TLSConfig specifies the TLS configuration to use with tls.Listener.
	TLSConfig *tls.Config
	// Listener is an optional client server connection middleware.
	Listener func(net.Listener) net.Listener

	// AllowedClients specifies clients that can connect to the server.
	AllowedClients []*AllowedClient

	// Log specifies the logger. If nil a default logging.Logger is used.
	Log logging.Logger
}

// Server is a tunnel server.
type Server struct {
	config *ServerConfig

	listener net.Listener

	httpClient *http.Client
	hostConn   map[string]net.Conn
	hostConnMu sync.RWMutex

	log logging.Logger
}

// NewServer creates new Server base on configuration.
func NewServer(config *ServerConfig) (*Server, error) {
	l, err := tls.Listen("tcp", ":0", config.TLSConfig)
	if err != nil {
		return nil, fmt.Errorf("tls listener failed :%s", err)
	}
	if config.Listener != nil {
		l = config.Listener(l)
		if l == nil {
			return nil, fmt.Errorf("listener function did not return a listener")
		}
	}

	log := logging.NewLogger("server")
	if config.Log != nil {
		log = config.Log
	}

	s := &Server{
		config:   config,
		listener: l,
		log:      log,
	}
	s.initHTTPClient()

	return s, nil
}

func (s *Server) initHTTPClient() {
	// TODO try using connection pool for transport
	s.hostConn = make(map[string]net.Conn)
	s.httpClient = &http.Client{
		Transport: &http2.Transport{
			DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
				s.hostConnMu.RLock()
				defer s.hostConnMu.RUnlock()

				conn, ok := s.hostConn[addr]
				if !ok {
					return nil, fmt.Errorf("no connection for %q", addr)
				}
				return conn, nil
			},
		},
	}
}

func (s *Server) Start() {
	go s.listenControl()
	s.listenClientListeners()
}

func (s *Server) listenControl() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.log.Warning("Accept %s control connection to %q failed: %s",
				s.listener.Addr().Network(), s.listener.Addr().String(), err)
			continue
		}
		s.log.Info("Accepted %s control connection from %q to %q",
			s.listener.Addr().Network(), conn.RemoteAddr(), s.listener.Addr().String())
		go s.handleClient(conn)
	}
}

func (s *Server) handleClient(conn net.Conn) {
	var (
		client *AllowedClient
		req    *http.Request
		resp   *http.Response
		err    error
		ok     bool
	)

	id, err := peerID(conn.(*tls.Conn))
	if err != nil {
		s.log.Warning("Certificate error: %s", err)
		goto reject
	}

	client, ok = s.checkID(id)
	if !ok {
		s.log.Warning("Unknown certificate: %q", id.String())
		goto reject
	}

	req, err = http.NewRequest(http.MethodConnect, url(client.Host), nil)
	if err != nil {
		s.log.Error("Invalid host %q for client %q", client.Host, client.ID)
		goto reject
	}

	if err = conn.SetDeadline(time.Time{}); err != nil {
		s.log.Warning("Setting no deadline failed: %s", err)
		// recoverable
	}

	if err := s.addHostConn(client, conn); err != nil {
		s.log.Warning("Could not add host: %s", err)
		goto reject
	}

	resp, err = s.httpClient.Do(req)
	if err != nil {
		s.log.Warning("Handshake failed %s", err)
		goto reject
	}
	if resp.StatusCode != http.StatusOK {
		s.log.Warning("Handshake failed")
		goto reject
	}

	s.log.Info("Client %q connected from %q", client.ID, conn.RemoteAddr().String())

	return

reject:
	conn.Close()
	if client != nil {
		s.deleteHostConn(client.Host)
	}
}

func (s *Server) checkID(id id.ID) (*AllowedClient, bool) {
	for _, c := range s.config.AllowedClients {
		if id.Equals(c.ID) {
			return c, true
		}
	}
	return nil, false
}

func (s *Server) addHostConn(client *AllowedClient, conn net.Conn) error {
	key := hostPort(client.Host)

	s.hostConnMu.Lock()
	defer s.hostConnMu.Unlock()

	if c, ok := s.hostConn[key]; ok {
		return fmt.Errorf("client %q already connected from %q", client.ID, c.RemoteAddr().String())
	}

	s.hostConn[key] = conn

	return nil
}

func (s *Server) deleteHostConn(host string) {
	s.hostConnMu.Lock()
	delete(s.hostConn, hostPort(host))
	s.hostConnMu.Unlock()
}

func hostPort(host string) string {
	return fmt.Sprint(host, ":443")
}

func (s *Server) listenClientListeners() {
	for _, client := range s.config.AllowedClients {
		if client.Listeners == nil {
			continue
		}

		for _, l := range client.Listeners {
			go s.listen(l, client)
		}
	}
}

func (s *Server) listen(l net.Listener, client *AllowedClient) {
	for {
		conn, err := l.Accept()
		if err != nil {
			s.log.Warning("Accept %s connection to %q failed: %s",
				s.listener.Addr().Network(), s.listener.Addr().String(), err)
			continue
		}
		s.log.Debug("Accepted %s connection from %q to %q",
			l.Addr().Network(), conn.RemoteAddr(), l.Addr().String())

		msg := &proto.ControlMessage{
			Action:       proto.RequestClientSession,
			Protocol:     l.Addr().Network(),
			ForwardedFor: conn.RemoteAddr().String(),
			ForwardedBy:  conn.LocalAddr().String(),
		}

		go s.proxy(client.Host, conn, conn, msg)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	msg := &proto.ControlMessage{
		Action:       proto.RequestClientSession,
		Protocol:     proto.HTTPProtocol,
		ForwardedFor: r.RemoteAddr,
		ForwardedBy:  r.Host,
		URLPath:      r.URL.Path,
	}

	s.proxy(trimPort(r.Host), w, r, msg)
}

func trimPort(hostPort string) (host string) {
	host, _, _ = net.SplitHostPort(hostPort)
	if host == "" {
		return hostPort
	}
	return
}

func (s *Server) proxy(host string, w io.Writer, r interface{}, msg *proto.ControlMessage) {
	s.log.Debug("Start proxying %v to %q", msg, host)

	defer func() {
		if c, ok := r.(io.Closer); ok {
			c.Close()
		}
	}()

	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()

	req, err := http.NewRequest(http.MethodPut, url(host), pr)
	if err != nil {
		s.log.Error("Request creation failed: %s", err)
		return
	}
	msg.WriteTo(req.Header)

	var localToRemoteDone = make(chan struct{})

	localToRemote := func() {
		if hr, ok := r.(*http.Request); ok {
			hr.Write(pw)
			pw.Close()
		} else {
			transfer("local to remote", pw, r.(io.ReadCloser), s.log)
		}
		close(localToRemoteDone)
	}

	remoteToLocal := func() {
		resp, err := s.httpClient.Do(req)
		if err != nil {
			s.log.Error("Proxying conn to client %q failed: %s", host, err)
			return
		}
		if hw, ok := w.(http.ResponseWriter); ok {
			pr, err := http.ReadResponse(bufio.NewReader(resp.Body), r.(*http.Request))
			if err != nil {
				s.log.Error("Reading HTTP response failed: %s", err)
				return
			}
			copyHeader(hw.Header(), pr.Header)
			hw.WriteHeader(pr.StatusCode)
			transfer("remote to local", hw, pr.Body, s.log)
		} else {
			transfer("remote to local", w, resp.Body, s.log)
		}
	}

	go localToRemote()
	remoteToLocal()
	<-localToRemoteDone

	s.log.Debug("Done proxying %v to %q", msg, host)
}

func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *Server) Close() error {
	if s.listener == nil {
		return nil
	}
	return s.listener.Close()
}
