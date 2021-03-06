package connector

import (
	"net"
	"sync"
	"errors"
	"expvar"
)

var activeConnections = expvar.NewInt("activeConnections")
var connectionsCreated = expvar.NewInt("connectionsCreated")
var discardedConnections = expvar.NewInt("discardedConnections")

type AuthenticationError string

func (e AuthenticationError) Error() string {
	return string(e)
}

type ConnectionProvider interface {
	GetGeodeConnection() *GeodeConnection
}

type Pool struct {
	sync.RWMutex
	recentConnections     []*GeodeConnection
	providers             []ConnectionProvider
	authenticationEnabled bool
	username              string
	password              string
}

func NewPool() *Pool {
	return &Pool{
		authenticationEnabled: false,
	}
}

func (this *Pool) AddConnection(c net.Conn, handshakeDone bool) {
	gConn := &GeodeConnection{
		rawConn:            c,
		handshakeDone:      handshakeDone,
		authenticationDone: false,
		inUse:              false,
	}

	this.recentConnections = append(this.recentConnections, gConn)
}

func (this *Pool) AddLocator(host string, port int) {
	// TODO: Implement me
}

func (this *Pool) AddServer(host string, port int) {
	this.providers = append(this.providers, &serverConnectionProvider{
		host,
		port,
	})
}

func (this *Pool) GetConnection() (*GeodeConnection, error) {
	var gConn *GeodeConnection
	var err error

	this.Lock()
	defer this.Unlock()

	// First let's check the recent connections
	for _, c := range this.recentConnections {
		if ! c.inUse {
			gConn = c
		}
	}

	if gConn == nil {
		for i := len(this.providers) - 1; i >= 0; i-- {
			gConn = this.providers[i].GetGeodeConnection()
			if gConn != nil {
				break
			}
			this.providers = append(this.providers[:i], this.providers[i+1:]...)
		}

		if gConn != nil {
			this.recentConnections = append(this.recentConnections, gConn)
			connectionsCreated.Add(1)
		}
	}

	if gConn == nil {
		return nil, errors.New("no connections available")
	}

	err = gConn.handshake()
	if err != nil {
		this.discardConnection(gConn)
		return nil, err
	}

	if this.authenticationEnabled {
		err = gConn.authenticate(this.username, this.password)
		if err != nil {
			this.discardConnection(gConn)
			return nil, err
		}
	}

	gConn.inUse = true
	activeConnections.Add(1)

	return gConn, nil
}

func (this *Pool) ReturnConnection(gConn *GeodeConnection) {
	this.Lock()
	defer this.Unlock()

	gConn.inUse = false
	activeConnections.Add(-1)
}

// MUST hold the pool lock when calling
func (this *Pool) discardConnection(gConn *GeodeConnection) {
	for i, c := range this.recentConnections {
		if gConn == c {
			this.recentConnections = append(this.recentConnections[:i], this.recentConnections[i+1:]...)
			break
		}
	}

	_ = gConn.rawConn.Close()
}

// DiscardConnection is used publicly as it holds the necessary lock
func (this *Pool) DiscardConnection(gConn *GeodeConnection) {
	this.Lock()
	this.discardConnection(gConn)
	this.Unlock()

	discardedConnections.Add(1)
}

func (this *Pool) AddCredentials(username, password string) {
	this.username = username
	this.password = password
	this.authenticationEnabled = true
}
