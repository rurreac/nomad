package client

import (
	"io"
	"net"
	"net/rpc"
	"strings"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/consul/lib"
	inmem "github.com/hashicorp/nomad/helper/codec"
	"github.com/hashicorp/nomad/helper/pool"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/yamux"
	"github.com/ugorji/go/codec"
)

// rpcEndpoints holds the RPC endpoints
type rpcEndpoints struct {
	ClientStats *ClientStats
	FileSystem  *FileSystem
}

// ClientRPC is used to make a local, client only RPC call
func (c *Client) ClientRPC(method string, args interface{}, reply interface{}) error {
	codec := &inmem.InmemCodec{
		Method: method,
		Args:   args,
		Reply:  reply,
	}
	if err := c.rpcServer.ServeRequest(codec); err != nil {
		return err
	}
	return codec.Err
}

// ClientStreamingRpcHandler is used to make a local, client only streaming RPC
// call.
func (c *Client) ClientStreamingRpcHandler(method string) (structs.StreamingRpcHandler, error) {
	return c.streamingRpcs.GetHandler(method)
}

// RPC is used to forward an RPC call to a nomad server, or fail if no servers.
func (c *Client) RPC(method string, args interface{}, reply interface{}) error {
	// Invoke the RPCHandler if it exists
	if c.config.RPCHandler != nil {
		return c.config.RPCHandler.RPC(method, args, reply)
	}

	// This is subtle but we start measuring the time on the client side
	// right at the time of the first request, vs. on the first retry as
	// is done on the server side inside forward(). This is because the
	// servers may already be applying the RPCHoldTimeout up there, so by
	// starting the timer here we won't potentially double up the delay.
	firstCheck := time.Now()

TRY:
	server := c.servers.FindServer()
	if server == nil {
		return noServersErr
	}

	// Make the request.
	rpcErr := c.connPool.RPC(c.Region(), server.Addr, c.RPCMajorVersion(), method, args, reply)
	if rpcErr == nil {
		return nil
	}

	// Move off to another server, and see if we can retry.
	c.logger.Printf("[ERR] nomad: %q RPC failed to server %s: %v", method, server.Addr, rpcErr)
	c.servers.NotifyFailedServer(server)
	if retry := canRetry(args, rpcErr); !retry {
		return rpcErr
	}

	// We can wait a bit and retry!
	if time.Since(firstCheck) < c.config.RPCHoldTimeout {
		jitter := lib.RandomStagger(c.config.RPCHoldTimeout / structs.JitterFraction)
		select {
		case <-time.After(jitter):
			goto TRY
		case <-c.shutdownCh:
		}
	}
	return rpcErr
}

// canRetry returns true if the given situation is safe for a retry.
func canRetry(args interface{}, err error) bool {
	// No leader errors are always safe to retry since no state could have
	// been changed.
	if structs.IsErrNoLeader(err) {
		return true
	}

	// Reads are safe to retry for stream errors, such as if a server was
	// being shut down.
	info, ok := args.(structs.RPCInfo)
	if ok && info.IsRead() && lib.IsErrEOF(err) {
		return true
	}

	return false
}

// setupClientRpc is used to setup the Client's RPC endpoints
func (c *Client) setupClientRpc() {
	// Initialize the RPC handlers
	c.endpoints.ClientStats = &ClientStats{c}
	c.endpoints.FileSystem = &FileSystem{c}

	// Initialize the streaming RPC handlers.
	c.endpoints.FileSystem.Register()

	// Create the RPC Server
	c.rpcServer = rpc.NewServer()

	// Register the endpoints with the RPC server
	c.setupClientRpcServer(c.rpcServer)

	go c.rpcConnListener()
}

// rpcConnListener is a long lived function that listens for new connections
// being made on the connection pool and starts an RPC listener for each
// connection.
func (c *Client) rpcConnListener() {
	// Make a channel for new connections.
	conns := make(chan *yamux.Session, 4)
	c.connPool.SetConnListener(conns)

	for {
		select {
		case <-c.shutdownCh:
			return
		case session, ok := <-conns:
			if !ok {
				continue
			}

			go c.listenConn(session)
		}
	}
}

// listenConn is used to listen for connections being made from the server on
// pre-existing connection. This should be called in a goroutine.
func (c *Client) listenConn(s *yamux.Session) {
	for {
		conn, err := s.Accept()
		if err != nil {
			if s.IsClosed() {
				return
			}

			c.logger.Printf("[ERR] client.rpc: failed to accept RPC conn: %v", err)
			continue
		}

		go c.handleConn(conn)
		metrics.IncrCounter([]string{"client", "rpc", "accept_conn"}, 1)
	}
}

// handleConn is used to determine if this is a RPC or Streaming RPC connection and
// invoke the correct handler
func (c *Client) handleConn(conn net.Conn) {
	// Read a single byte
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil {
		if err != io.EOF {
			c.logger.Printf("[ERR] client.rpc: failed to read byte: %v", err)
		}
		conn.Close()
		return
	}

	// Switch on the byte
	switch pool.RPCType(buf[0]) {
	case pool.RpcNomad:
		c.handleNomadConn(conn)

	case pool.RpcStreaming:
		c.handleStreamingConn(conn)

	default:
		c.logger.Printf("[ERR] client.rpc: unrecognized RPC byte: %v", buf[0])
		conn.Close()
		return
	}
}

// handleNomadConn is used to handle a single Nomad RPC connection.
func (c *Client) handleNomadConn(conn net.Conn) {
	defer conn.Close()
	rpcCodec := pool.NewServerCodec(conn)
	for {
		select {
		case <-c.shutdownCh:
			return
		default:
		}

		if err := c.rpcServer.ServeRequest(rpcCodec); err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "closed") {
				c.logger.Printf("[ERR] client.rpc: RPC error: %v (%v)", err, conn)
				metrics.IncrCounter([]string{"client", "rpc", "request_error"}, 1)
			}
			return
		}
		metrics.IncrCounter([]string{"client", "rpc", "request"}, 1)
	}
}

// handleStreamingConn is used to handle a single Streaming Nomad RPC connection.
func (c *Client) handleStreamingConn(conn net.Conn) {
	defer conn.Close()

	// Decode the header
	var header structs.StreamingRpcHeader
	decoder := codec.NewDecoder(conn, structs.MsgpackHandle)
	if err := decoder.Decode(&header); err != nil {
		if err != io.EOF && !strings.Contains(err.Error(), "closed") {
			c.logger.Printf("[ERR] client.rpc: Streaming RPC error: %v (%v)", err, conn)
			metrics.IncrCounter([]string{"client", "streaming_rpc", "request_error"}, 1)
		}

		return
	}

	handler, err := c.streamingRpcs.GetHandler(header.Method)
	if err != nil {
		c.logger.Printf("[ERR] client.rpc: Streaming RPC error: %v (%v)", err, conn)
		metrics.IncrCounter([]string{"client", "streaming_rpc", "request_error"}, 1)
		return
	}

	// Invoke the handler
	metrics.IncrCounter([]string{"client", "streaming_rpc", "request"}, 1)
	handler(conn)
}

// setupClientRpcServer is used to populate a client RPC server with endpoints.
func (c *Client) setupClientRpcServer(server *rpc.Server) {
	// Register the endpoints
	server.Register(c.endpoints.ClientStats)
}

// resolveServer given a sever's address as a string, return it's resolved
// net.Addr or an error.
func resolveServer(s string) (net.Addr, error) {
	const defaultClientPort = "4647" // default client RPC port
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		if strings.Contains(err.Error(), "missing port") {
			host = s
			port = defaultClientPort
		} else {
			return nil, err
		}
	}
	return net.ResolveTCPAddr("tcp", net.JoinHostPort(host, port))
}

// Ping is used to ping a particular server and returns whether it is healthy or
// a potential error.
func (c *Client) Ping(srv net.Addr) error {
	var reply struct{}
	err := c.connPool.RPC(c.Region(), srv, c.RPCMajorVersion(), "Status.Ping", struct{}{}, &reply)
	return err
}