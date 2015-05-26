//Copyright (c) 2013, Salesforce.com, Inc.
//All rights reserved.
//
//Redistribution and use in source and binary forms, with or without modification, are permitted provided that the following conditions are met:
//
//	Redistributions of source code must retain the above copyright notice, this list of conditions and the following disclaimer.
//	Redistributions in binary form must reproduce the above copyright notice, this list of conditions and the following disclaimer in the documentation and/or other materials provided with the distribution.
//	Neither the name of Salesforce.com nor the names of its contributors may be used to endorse or promote products derived from this software without specific prior written permission.
//
//THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package rmux

import (
	"fmt"
	"github.com/forcedotcom/rmux/connection"
	"github.com/forcedotcom/rmux/protocol"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	//Response code for when a command (that operates on multiple keys) is used on a server that is multiplexing
	MULTIPLEX_OPERATION_UNSUPPORTED_RESPONSE = []byte("This command is not supported for multiplexing servers")
	//Response code for when a client can't connect to any target servers
	CONNECTION_DOWN_RESPONSE = []byte("Connection down")
)

//The main RedisMultiplexer
//Listens on a specified socket or port, and assigns out queries to any number of connection pools
//If more than one connection pool is given multi-key operations are blocked
type RedisMultiplexer struct {
	HashRing *connection.HashRing
	//hashmap of [connection endpoint] -> connectionPools
	ConnectionCluster []*connection.ConnectionPool
	//The net.listener for our server
	Listener net.Listener
	//The amount of connections to store, in each of our connectionpools
	PoolSize int
	//The primary connection key to use.  If we're not operating on a key-based operation, it will go here
	PrimaryConnectionPool *connection.ConnectionPool
	//And overridable connect timeout.  Defaults to EXTERN_CONNECT_TIMEOUT
	EndpointConnectTimeout time.Duration
	//An overridable read timeout.  Defaults to EXTERN_READ_TIMEOUT
	EndpointReadTimeout time.Duration
	//An overridable write timeout.  Defaults to EXTERN_WRITE_TIMEOUT
	EndpointWriteTimeout time.Duration
	//An overridable read timeout.  Defaults to EXTERN_READ_TIMEOUT
	ClientReadTimeout time.Duration
	//An overridable write timeout.  Defaults to EXTERN_WRITE_TIMEOUT
	ClientWriteTimeout time.Duration

	//Whether or not the multiplexer is active.  Used to determine when a tear-down should be occuring
	active bool
	//The amount of active (outbound) connections that we have
	activeConnectionCount int
	//The amount of total (incoming) connections that we have
	connectionCount int32
	//whether or not we are multiplexing
	multiplexing bool
	infoResponse []byte
	infoMutex    sync.RWMutex
}

//Sub-task that handles the cleanup when a server goes down
func (this *RedisMultiplexer) initializeCleanup() {
	//Make a single-item channel for sigterm requests
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	signal.Notify(c, syscall.SIGTERM)
	// Block until we have a kill-request to pop off
	<-c
	//Flag ourselves as cleaning up
	this.active = false
	//And close our listener
	this.Listener.Close()
	//Give ourselves a bit to clean up
	time.Sleep(time.Millisecond * 150)
	os.Exit(1)
}

//Initializes a new redis multiplexer, listening on the given protocol/endpoint, with a set connectionPool size
//ex: "unix", "/tmp/myAwesomeSocket", 50
func NewRedisMultiplexer(listenProtocol, listenEndpoint string, poolSize int) (newRedisMultiplexer *RedisMultiplexer, err error) {
	newRedisMultiplexer = &RedisMultiplexer{}
	newRedisMultiplexer.Listener, err = net.Listen(listenProtocol, listenEndpoint)
	if err != nil {
		println("listen error", err.Error())
		return nil, err
	}
	newRedisMultiplexer.ConnectionCluster = make([]*connection.ConnectionPool, 0)
	newRedisMultiplexer.PoolSize = poolSize
	newRedisMultiplexer.active = true
	newRedisMultiplexer.EndpointConnectTimeout = connection.EXTERN_CONNECT_TIMEOUT
	newRedisMultiplexer.EndpointReadTimeout = connection.EXTERN_READ_TIMEOUT
	newRedisMultiplexer.EndpointWriteTimeout = connection.EXTERN_WRITE_TIMEOUT
	newRedisMultiplexer.ClientReadTimeout = connection.EXTERN_READ_TIMEOUT
	newRedisMultiplexer.ClientWriteTimeout = connection.EXTERN_WRITE_TIMEOUT
	newRedisMultiplexer.infoMutex = sync.RWMutex{}
	protocol.Debug("Redis Multiplexer Initialized")
	return
}

//Adds a connection to the redis multiplexer, for the given protocol and endpoint
func (this *RedisMultiplexer) AddConnection(remoteProtocol, remoteEndpoint string) {
	connectionCluster := connection.NewConnectionPool(remoteProtocol, remoteEndpoint, this.PoolSize)
	connectionCluster.ConnectTimeout = this.EndpointConnectTimeout
	connectionCluster.ReadTimeout = this.EndpointReadTimeout
	connectionCluster.WriteTimeout = this.EndpointWriteTimeout
	this.ConnectionCluster = append(this.ConnectionCluster, connectionCluster)
	if len(this.ConnectionCluster) == 1 {
		this.PrimaryConnectionPool = connectionCluster
	} else {
		this.multiplexing = true
	}
}

//Counts the number of active endpoints on the server
func (this *RedisMultiplexer) countActiveConnections() (activeConnections int) {
	activeConnections = 0
	for _, connectionPool := range this.ConnectionCluster {
		if connectionPool.CheckConnectionState() {
			activeConnections++
		}
	}
	return
}

//Checks the status of all connections, and calculates how many of them are currently up
func (this *RedisMultiplexer) maintainConnectionStates() {
	var m runtime.MemStats
	for this.active {
		this.activeConnectionCount = this.countActiveConnections()
		// protocol.Debug("We have %d connections", this.connectionCount)
		runtime.ReadMemStats(&m)
		// protocol.Debug("Memory profile: InUse(%d) Idle (%d) Released(%d)", m.HeapInuse, m.HeapIdle, m.HeapReleased)
		this.generateMultiplexInfo()
		time.Sleep(100 * time.Millisecond)
	}
}

//Generates the Info response for a multiplexed server
func (this *RedisMultiplexer) generateMultiplexInfo() {
	tmpSlice := fmt.Sprintf("rmux_version: %s\r\ngo_version: %s\r\nprocess_id: %d\r\nconnected_clients: %d\r\nactive_endpoints: %d\r\ntotal_endpoints: %d\r\nrole: master\r\n", "1.0", runtime.Version(), os.Getpid(), this.connectionCount, this.activeConnectionCount, len(this.ConnectionCluster))
	this.infoMutex.Lock()
	this.infoResponse = []byte(fmt.Sprintf("$%d\r\n%s", len(tmpSlice), tmpSlice))
	this.infoMutex.Unlock()
}

//Called when a rmux server is ready to begin accepting connections
func (this *RedisMultiplexer) Start() (err error) {
	this.HashRing, err = connection.NewHashRing(this.ConnectionCluster)
	if err != nil {
		return err
	}

	go this.maintainConnectionStates()
	go this.initializeCleanup()

	protocol.Debug("Test?")
	for this.active {
		fd, err := this.Listener.Accept()
		if err != nil {
			protocol.Debug("Start: Error received from listener.Accept: %s", err.Error())
			continue
		}
		protocol.Debug("Accepted connection.")

		go this.initializeClient(fd)
	}
	time.Sleep(100 * time.Millisecond)
	return
}

//Initializes a client's connection to our server.  Sets up our disconnect hooks and then passes the client off for request handling
func (this *RedisMultiplexer) initializeClient(localConnection net.Conn) {
	defer func() {
		atomic.AddInt32(&this.connectionCount, -1)
	}()
	atomic.AddInt32(&this.connectionCount, 1)
	//Add the connection to our internal list
	myClient := NewClient(localConnection, this.ClientReadTimeout, this.ClientWriteTimeout,
		this.multiplexing)

	defer func() {
		r := recover()
		if r != nil {
			if val, ok := r.(string); ok {
				// If we paniced, push that to the client before closing the connection
				protocol.WriteError([]byte(val), myClient.Writer, true)
			}
		}
		myClient.ConnectionReadWriter.NetConnection.Close()
	}()

	this.HandleClientRequests(myClient)
}

//Sends the pre-generated Info response for a multiplexed server
func (this *RedisMultiplexer) sendMultiplexInfo(myClient *Client) (err error) {
	this.infoMutex.RLock()
	err = protocol.WriteLine(this.infoResponse, myClient.Writer, true)
	this.infoMutex.RUnlock()
	return
}

//Handles requests for a client.
//Inspects all incoming commands, to find if they are key-driven or not.
//If they are, finds the appropriate connection pool, and passes the request off to it.
func (this *RedisMultiplexer) HandleClientRequests(client *Client) {
	if this.multiplexing {
		this.handleMultiplexingClientRequests(client)
	} else {
		this.handleNonMultiplexingClientRequests(client)
	}
}

func (this *RedisMultiplexer) handleMultiplexingClientRequests(client *Client) error {
	var connectionPool *connection.ConnectionPool

ClientLoop:
	for this.active && client.Active {
		// TODO: Block on available? Select?

		// Read a buffered command
		command, err := client.ReadBufferedCommand()
		if err != nil {
			if recErr, ok := err.(*protocol.RecoverableError); ok {
				// Since we can recover, flush an error to the client and loop up
				client.FlushError(recErr)
				continue
			} else if err == io.EOF {
				// Stream EOF-ed. Deactivate this client and break out.
				client.Active = false
				break
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				//We had a read timeout.  Continue and try try again
				continue
			} else {
				// This is something we've never seen before! Panic panic panic
				panic("New Client Read Error: " + err.Error())
			}
		}

		// Parse it - some need an immediate response
		immediateResponse, err := client.ParseCommand(command)
		if immediateResponse != nil {
			_ = client.FlushLine(immediateResponse) // TODO: handle error
			continue
		} else if err != nil {
			if err == ERR_QUIT {
				// Respond with OK, deactivate ourselves
				_ = client.FlushLine(protocol.OK_RESPONSE)
				client.Active = false
				break ClientLoop
			} else if recErr, ok := err.(*protocol.RecoverableError); ok {
				_ = client.FlushError(recErr) // TODO: Handle error
				continue
			} else {
				panic("Not sure how to handle this error: " + err.Error())
			}
		}

		// Write the command out to server
		connectionPool = this.HashRing.GetConnectionPool(command)
		connection := connectionPool.GetConnection()        // TODO: Nil handling
		_ = client.HandleRequest(connection, command, true) // TODO: Error handling
		connectionPool.RecycleRemoteConnection(connection)
	}

	return nil
}

func (this *RedisMultiplexer) handleNonMultiplexingClientRequests(client *Client) error {
	var connection *connection.Connection
	connectionPool := this.HashRing.DefaultConnectionPool // TODO: This doesn't have a default connection pool assigned

	defer func() {
		if connection != nil {
			connectionPool.RecycleRemoteConnection(connection)
		}
	}()

ClientLoop:
	for this.active && client.Active {
		// TODO: Block on available? Select?
		bufferedCommands, err := client.ReadBufferedCommands()
		if err != nil {
			if recErr, ok := err.(*protocol.RecoverableError); ok {
				// Since we can recover, flush an error to the client and loop up
				client.FlushError(recErr)
				continue ClientLoop
			} else if err == io.EOF {
				// Stream EOF-ed. Deactivate this client and break out.
				client.Active = false
				break
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				//We had a read timeout.  Continue and try try again
				continue
			} else {
				// This is something we've never seen before! Panic panic panic
				panic("New Client Read Error: " + err.Error())
			}
		} else if len(bufferedCommands) == 0 {
			continue
		}

		protocol.Debug("Read buffered commands: %v", bufferedCommands)

		for _, command := range bufferedCommands {
			immediateResponse, err := client.ParseCommand(command)

			if immediateResponse != nil || err != nil {
				// Need to respond to the client. Flush anything pending.
				if err := client.FlushRedisAndRespond(connection); err != nil {
					protocol.Debug("Error from FlushRedisAndRespond: %s", err)
				}
			}

			if immediateResponse != nil {
				err = client.FlushLine(immediateResponse)
				if err != nil {
					protocol.Debug("Error received when flushing an immediate response: %s", err)
				}
				continue
			} else if err != nil {
				if err == ERR_QUIT {
					// Respond with OK, deactivate ourselves
					client.FlushLine(protocol.OK_RESPONSE)
					client.Active = false
				} else if recErr, ok := err.(*protocol.RecoverableError); ok {
					// Flush stuff back to the client, get rid of the rest on the read buffer.
					client.FlushError(recErr)
					client.DiscardReaderBuffered()
				} else {
					panic("Not sure how to handle this error: " + err.Error())
				}

				continue ClientLoop
			}

			// Otherwise, the command is ready to buffer to the connection.
			// TODO: Maybe we should use a write buffer in front of it before grabbing the connection?
			if connection == nil {
				connection = connectionPool.GetConnection()

				//If we don't have a connection, something went horribly wrong
				if connection == nil {
					protocol.Debug("Failed to retrieve an active connection from the provided connection pool")
					return protocol.WriteError(CONNECTION_DOWN_RESPONSE, client.Writer, true)
				}
			}

			// Write the command out to server
			// TODO: See what happens when the output buffer is filled... can it be filled?
			_ = protocol.WriteCommand(command, connection.Writer, false)
			if err != nil {
				// TODO: Error handling - flush the buffer on both sides
				continue
			}
		}

		if connection != nil && connection.Writer.Buffered() > 0 {
			client.FlushRedisAndRespond(connection) // TODO: error handling
		}

		if client.Writer.Buffered() > 0 {
			client.Writer.Flush()
		}

		if connection != nil {
			connectionPool.RecycleRemoteConnection(connection)
		}
	}

	return nil
}
