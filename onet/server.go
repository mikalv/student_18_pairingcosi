package onet

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dedis/kyber"
	"github.com/dedis/onet/cfgpath"
	"github.com/dedis/onet/log"
	"github.com/dedis/onet/network"
	"rsc.io/goversion/version"
)

// Server connects the Router, the Overlay, and the Services together. It sets
// up everything and returns once a working network has been set up.
type Server struct {
	// Our private-key
	private kyber.Scalar
	*network.Router
	// Overlay handles the mapping from tree and entityList to ServerIdentity.
	// It uses tokens to represent an unique ProtocolInstance in the system
	overlay *Overlay
	// lock associated to access trees
	treesLock            sync.Mutex
	serviceManager       *serviceManager
	statusReporterStruct *statusReporterStruct
	// protocols holds a map of all available protocols and how to create an
	// instance of it
	protocols *protocolStorage
	// webservice
	websocket *WebSocket
	// when this node has been started
	started time.Time

	suite network.Suite
}

func dbPathFromEnv() string {
	p := os.Getenv("CONODE_SERVICE_PATH")
	if p == "" {
		p = cfgpath.GetDataPath("conode")
	}
	return p
}

// NewServer returns a fresh Server tied to a given Router.
// If dbPath is "", the server will write its database to the default
// location. If dbPath is != "", it is considered a temp dir, and the
// DB is deleted on close.
func newServer(s network.Suite, dbPath string, r *network.Router, pkey kyber.Scalar) *Server {
	delDb := false
	if dbPath == "" {
		dbPath = dbPathFromEnv()
		log.ErrFatal(os.MkdirAll(dbPath, 0750))
	} else {
		delDb = true
	}

	c := &Server{
		private:              pkey,
		statusReporterStruct: newStatusReporterStruct(),
		Router:               r,
		protocols:            newProtocolStorage(),
		suite:                s,
	}
	c.overlay = NewOverlay(c)
	c.websocket = NewWebSocket(r.ServerIdentity)
	c.serviceManager = newServiceManager(c, c.overlay, dbPath, delDb)
	c.statusReporterStruct.RegisterStatusReporter("Generic", c)
	for name, inst := range protocols.instantiators {
		log.Lvl4("Registering global protocol", name)
		c.ProtocolRegister(name, inst)
	}
	return c
}

// NewServerTCP returns a new Server out of a private-key and its related
// public key within the ServerIdentity. The server will use a default
// TcpRouter as Router.
func NewServerTCP(e *network.ServerIdentity, suite network.Suite) *Server {
	return NewServerTCPWithListenAddr(e, suite, "")
}

// NewServerTCPWithListenAddr returns a new Server out of a private-key and
// its related public key within the ServerIdentity. The server will use a
// TcpRouter listening on the given address as Router.
func NewServerTCPWithListenAddr(e *network.ServerIdentity, suite network.Suite,
	listenAddr string) *Server {
	r, err := network.NewTCPRouterWithListenAddr(e, suite, listenAddr)
	log.ErrFatal(err)
	return newServer(suite, "", r, e.GetPrivate())
}

// Suite can (and should) be used to get the underlying Suite.
// Currently the suite is hardcoded into the network library.
// Don't use network.Suite but Host's Suite function instead if possible.
func (c *Server) Suite() network.Suite {
	return c.suite
}

var gover version.Version
var goverOnce sync.Once
var goverOk = false

// GetStatus is a function that returns the status report of the server.
func (c *Server) GetStatus() *Status {
	v := Version
	if gitTag != "" {
		v += "-"
		v += gitTag
	}

	a := c.serviceManager.availableServices()
	sort.Strings(a)

	st := &Status{Field: map[string]string{
		"Available_Services": strings.Join(a, ","),
		"TX_bytes":           strconv.FormatUint(c.Router.Tx(), 10),
		"RX_bytes":           strconv.FormatUint(c.Router.Rx(), 10),
		"Uptime":             time.Now().Sub(c.started).String(),
		"System": fmt.Sprintf("%s/%s/%s", runtime.GOOS, runtime.GOARCH,
			runtime.Version()),
		"Version":     v,
		"Host":        c.ServerIdentity.Address.Host(),
		"Port":        c.ServerIdentity.Address.Port(),
		"Description": c.ServerIdentity.Description,
		"ConnType":    string(c.ServerIdentity.Address.ConnType()),
	}}

	goverOnce.Do(func() {
		v, err := version.ReadExe(os.Args[0])
		if err == nil {
			gover = v
			goverOk = true
		}
	})

	if goverOk {
		st.Field["GoRelease"] = gover.Release
		st.Field["GoModuleInfo"] = gover.ModuleInfo
	}

	return st
}

// Close closes the overlay and the Router
func (c *Server) Close() error {
	c.overlay.stop()
	c.websocket.stop()
	c.overlay.Close()
	err := c.serviceManager.closeDatabase()
	if err != nil {
		log.Lvl3("Error closing database: " + err.Error())
	}
	err = c.Router.Stop()
	log.Lvl3("Host Close", c.ServerIdentity.Address, "listening?", c.Router.Listening())
	return err
}

// Address returns the address used by the Router.
func (c *Server) Address() network.Address {
	return c.ServerIdentity.Address
}

// Service returns the service with the given name.
func (c *Server) Service(name string) Service {
	return c.serviceManager.service(name)
}

// GetService is kept for backward-compatibility.
func (c *Server) GetService(name string) Service {
	log.Warn("This method is deprecated - use `Server.Service` instead")
	return c.Service(name)
}

// ProtocolRegister will sign up a new protocol to this Server.
// It returns the ID of the protocol.
func (c *Server) ProtocolRegister(name string, protocol NewProtocol) (ProtocolID, error) {
	return c.protocols.Register(name, protocol)
}

// protocolInstantiate instantiate a protocol from its ID
func (c *Server) protocolInstantiate(protoID ProtocolID, tni *TreeNodeInstance) (ProtocolInstance, error) {
	fn, ok := c.protocols.instantiators[c.protocols.ProtocolIDToName(protoID)]
	if !ok {
		return nil, errors.New("No protocol constructor with this ID")
	}
	return fn(tni)
}

// Start makes the router and the websocket listen on their respective
// ports.
func (c *Server) Start() {
	InformServerStarted()
	c.started = time.Now()
	log.Lvlf1("Starting server at %s on address %s with public key %s",
		c.started.Format("2006-01-02 15:04:05"),
		c.ServerIdentity.Address, c.ServerIdentity.Public)
	go c.Router.Start()
	c.websocket.start()
}
