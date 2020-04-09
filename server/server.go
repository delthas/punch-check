package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"time"

	. "github.com/delthas/punch-check"
)

type event interface{}

type eventNew struct {
	c *net.TCPConn
	w chan Message
}

type eventClosed struct {
	c   *net.TCPConn
	err error
}

type eventRead struct {
	c       *net.TCPConn
	message Message
}

type connection struct {
	addr   *net.TCPAddr
	c      *net.TCPConn
	w      chan Message
	ports  []int
	client *client // nil if connection is a relay
}

func (c *connection) Write(localPort int, ip net.IP, port int) {
	data := make([]byte, 2)
	binary.BigEndian.PutUint16(data, uint16(localPort))
	c.w <- &MessageSend{
		LocalPort: localPort,
		IP:        ip,
		Port:      port,
		Data:      data,
	}
}

type client struct {
	last                      time.Time
	relays                    []*connection
	natPorts                  []int
	natPortDependentPort      int
	natEndpointDependentPort  int
	received                  bool
	receivedPortDependent     bool
	receivedEndpointDependent bool
	receivedHairpinning       bool
}

func (c *client) Done() bool {
	for _, port := range c.natPorts {
		if port == 0 {
			return false
		}
	}
	if c.natPortDependentPort == 0 || c.natEndpointDependentPort == 0 {
		return false
	}
	if !c.received || !c.receivedPortDependent || !c.receivedEndpointDependent || !c.receivedHairpinning {
		return false
	}
	return true
}

var punchTimeout = 5 * time.Second

var logErr = log.New(os.Stderr, "", log.Ldate|log.Ltime|log.Lshortfile)
var logDebug = log.New(os.Stderr, "", log.Ldate|log.Ltime|log.Lshortfile)

var allowedRelayHosts = []string{"punchcheckback.delthas.fr", "dille.cc", "delthas.fr"}
var allowedRelays []net.IP

var defaultServerPort = 23458

var events = make(chan event, 1000)
var connections = make(map[*net.TCPConn]*connection)

func acceptConnections(l *net.TCPListener) {
	for {
		c, err := l.AcceptTCP()
		if err != nil {
			break
		}
		c.SetNoDelay(true)
		w := make(chan Message)
		events <- eventNew{
			c: c,
			w: w,
		}
		go func() {
			for {
				m, err := ReadMessage(c)
				if err != nil {
					events <- eventClosed{
						c:   c,
						err: err,
					}
					return
				}
				events <- eventRead{
					c:       c,
					message: m,
				}
			}
		}()
		go func() {
			for m := range w {
				WriteMessage(c, m)
			}
			c.Close()
		}()
	}
}

func isRelay(addr *net.TCPAddr) bool {
	for _, relay := range allowedRelays {
		if relay.Equal(addr.IP) {
			return true
		}
	}
	return false
}

func main() {
	serverPort := flag.Int("port", defaultServerPort, "port to listen on")
	flag.Parse()

	allowedRelays = make([]net.IP, len(allowedRelayHosts))
	for i, relayHost := range allowedRelayHosts {
		addr, err := net.ResolveIPAddr("ip4", relayHost)
		if err != nil {
			logErr.Fatalf("failed resolving relay host %q: %v", relayHost, err)
		}
		allowedRelays[i] = addr.IP
	}

	l, err := net.ListenTCP("tcp4", &net.TCPAddr{
		Port: *serverPort,
	})
	if err != nil {
		logErr.Fatalf("failed creating control server socket on port %d: %v", *serverPort, err)
	}
	go acceptConnections(l)

	process()
}

func process() {
	for {
		select {
		case event := <-events:
			switch e := event.(type) {
			case eventNew:
				if _, ok := connections[e.c]; ok {
					break
				}
				addr := e.c.RemoteAddr().(*net.TCPAddr)
				var data *client
				if isRelay(addr) {
					for _, relay := range connections {
						if relay.client != nil {
							continue
						}
						if relay.addr.IP.Equal(addr.IP) {
							logErr.Printf("received new event of relayed that is already connected: %q", addr.IP.String())
							e.w <- &MessageInfo{
								MessageType: 0,
								Message:     "Internal error: Relay is already connected.",
							}
							close(e.w)
						}
					}
				} else {
					relayCount := 0
					for _, relay := range connections {
						if relay.client != nil {
							continue
						}
						relayCount++
					}
					if relayCount < ClientRelaysCount {
						logErr.Printf("not enough relays for client connection: want %d, has %d", ClientRelaysCount, relayCount)
						e.w <- &MessageInfo{
							MessageType: 0,
							Message:     "Internal error: Not enough relays available.",
						}
						close(e.w)
						break
					}
					relayIndexes := make(map[int]struct{}, ClientRelaysCount)
					for i := 0; i < ClientRelaysCount; i++ {
						relay := rand.Intn(relayCount)
						for {
							if _, ok := relayIndexes[relay]; !ok {
								break
							}
							relay = rand.Intn(relayCount)
						}
						relayIndexes[relay] = struct{}{}
					}
					relays := make([]*connection, ClientRelaysCount)
					i := 0
					ri := 0
					for _, relay := range connections {
						if relay.client != nil {
							continue
						}
						if _, ok := relayIndexes[ri]; ok {
							relays[i] = relay
							i++
						}
						ri++
					}
					data = &client{
						last:     time.Now(),
						relays:   relays,
						natPorts: make([]int, ClientPortsCount),
					}
				}
				connections[e.c] = &connection{
					addr:   addr,
					c:      e.c,
					w:      e.w,
					client: data,
				}
			case eventClosed:
				if _, ok := connections[e.c]; ok && e.err != nil {
					logErr.Printf("connection closed: %v", e.err)
				}
				closeConnection(e.c, nil)
			case eventRead:
				c, ok := connections[e.c]
				if !ok {
					break
				}
				switch m := e.message.(type) {
				case *MessagePorts:
					if c.ports != nil {
						logErr.Printf("received duplicate ports message: %v", e.message.Type())
						closeConnection(e.c, &MessageInfo{
							MessageType: 0,
							Message:     "Internal error: Unexpected ports message.",
						})
						break
					}
					var minPorts int
					if c.client != nil {
						minPorts = ClientPortsCount
					} else {
						minPorts = RelayPortsCount
					}
					if len(m.Ports) < minPorts {
						logErr.Printf("received invalid ports message: not enough ports: want %d, got %d", minPorts, len(m.Ports))
						closeConnection(e.c, &MessageInfo{
							MessageType: 0,
							Message:     "Internal error: Invalid ports message: not enough ports.",
						})
						break
					}
					if !unique(m.Ports) {
						logErr.Printf("received invalid ports message: ports are not unique: %v", m.Ports)
						closeConnection(e.c, &MessageInfo{
							MessageType: 0,
							Message:     "Internal error: Invalid ports message: ports are not unique.",
						})
						break
					}
					c.ports = m.Ports[:minPorts]
				case *MessageReceive:
					if len(m.Data) != 2 {
						break
					}
					remotePort := int(binary.BigEndian.Uint16(m.Data))
					var client *connection
					var relay *connection
					var clientPort int
					var clientNatPort int
					var relayPort int

					if c.client == nil {
						relay = c
						for _, c := range connections {
							if c.client != nil && c.addr.IP.Equal(m.IP) {
								client = c
								break
							}
						}
						if client == nil {
							logErr.Printf("received invalid receive message: unknown client: %s", net.IP(m.IP).String())
							break
						}
						clientPort = remotePort
						clientNatPort = m.Port
						relayPort = m.LocalPort
					} else {
						client = c
						if client.addr.IP.Equal(m.IP) {
							if m.LocalPort == client.ports[1] && m.Port == client.client.natPorts[2] {
								client.client.receivedHairpinning = true
							}
							break
						}
						for _, r := range c.client.relays {
							if r.addr.IP.Equal(m.IP) {
								relay = r
								break
							}
						}
						if relay == nil {
							logErr.Printf("received invalid receive message: unknown relay: %s", net.IP(m.IP).String())
							break
						}
						clientPort = m.LocalPort
						relayPort = m.Port
					}

					relayIndex := -1 // <A>0, <B>0
					for i, r := range client.client.relays {
						if r == relay {
							relayIndex = i
							break
						}
					}
					if relayIndex == -1 {
						logErr.Printf("received invalid receive message: unknown relay: %v", net.IP(relay.addr.IP))
						break
					}

					clientPortIndex := Index(client.ports, clientPort) // C<0>, C<1>
					if clientPortIndex == -1 {
						logErr.Printf("received invalid receive message: unknown client port: %v:%d", net.IP(client.addr.IP), clientPort)
						break
					}
					relayPortIndex := Index(relay.ports, relayPort) // A<0>, A<1>
					if relayPortIndex == -1 {
						logErr.Printf("received invalid receive message: unknown relay port for relay %v: %d", net.IP(relay.addr.IP), relayPort)
						break
					}

					if c.client == nil {
						if relayIndex == 0 && relayPortIndex == 0 { // C* -> A0
							client.client.natPorts[clientPortIndex] = clientNatPort
						} else if clientPortIndex == 0 {
							if relayIndex == 0 && relayPortIndex == 1 { // C0 -> A1
								client.client.natPortDependentPort = clientNatPort
							} else if relayIndex == 1 && relayPortIndex == 0 { // C0 -> B0
								client.client.natEndpointDependentPort = clientNatPort
							}
						}
					} else {
						if clientPortIndex == 1 {
							if relayIndex == 0 && relayPortIndex == 0 { // A0 -> C1
								client.client.received = true
							} else if relayIndex == 0 && relayPortIndex == 1 { // A1 -> C1
								client.client.receivedPortDependent = true
							} else if relayIndex == 1 && relayPortIndex == 0 { // B0 -> C1
								client.client.receivedEndpointDependent = true
							}
						}
					}
				default:
					logErr.Printf("received unexpected message type: %v", MessageType(e.message.Type()))
					closeConnection(e.c, &MessageInfo{
						MessageType: 0,
						Message:     fmt.Sprintf("Internal error: Invalid message type: %v.", MessageType(e.message.Type())),
					})
					break
				}
			}
		case <-time.After(50 * time.Millisecond):
			now := time.Now()
			for key, client := range connections {
				if client.client == nil {
					continue
				}
				if now.Sub(client.client.last) > punchTimeout || client.client.Done() {
					var message string
					if !client.client.received || client.client.natPorts[0] == 0 {
						message = "Test failed. UDP is blocked."
					} else {
						message = "Test complete.\n"
						var filtering string
						if client.client.receivedEndpointDependent {
							filtering = "endpoint-independent"
						} else if client.client.receivedPortDependent {
							filtering = "address-dependent"
						} else {
							filtering = "address and port-dependent"
						}
						holepunching := false
						var mapping string
						if client.client.natEndpointDependentPort == client.client.natPorts[0] {
							holepunching = true
							mapping = "endpoint-independent"
						} else if client.client.natPortDependentPort == client.client.natPorts[0] {
							mapping = "address-dependent"
						} else {
							mapping = "address and port-dependent"
						}
						parity := true
						preserved := true
						contiguous := true
						last := 0
						for i, port := range client.ports {
							natPort := client.client.natPorts[i]
							if natPort == 0 {
								last = 0
								continue
							}
							if port%2 != natPort%2 {
								parity = false
							}
							if port != natPort {
								preserved = false
							}
							if last != 0 && last != natPort+1 {
								contiguous = false
							}
							last = natPort
						}
						if holepunching {
							message += "Hole-punching is supported.\n"
						} else {
							message += "Hole-punching is NOT supported.\n"
						}
						message += fmt.Sprintf("Filtering: %s.\nMapping: %s.\n", filtering, mapping)
						if client.client.receivedHairpinning {
							message += "Hairpinning is supported.\n"
						}
						if parity {
							message += "Assignment preserves parity.\n"
						}
						if preserved {
							message += "Assignment preserves local port.\n"
						}
						if contiguous {
							message += "Assignment preserves contiguity.\n"
						}
					}
					closeConnection(key, &MessageInfo{
						MessageType: 1,
						Message:     message,
					})
					continue
				}
				for i := len(client.ports) - 1; i >= 0; i-- { // C* -> A0
					// send in reverse order to check both assignment contiguity and preservation
					clientPort := client.ports[i]
					relay := client.client.relays[0]
					client.Write(clientPort, relay.addr.IP, relay.ports[0])
				}
				{ // C0 -> A1
					relay := client.client.relays[0]
					client.Write(client.ports[0], relay.addr.IP, relay.ports[1])
				}
				{ // C0 -> B0
					relay := client.client.relays[1]
					client.Write(client.ports[0], relay.addr.IP, relay.ports[0])
				}
				natPort := client.client.natPorts[1]
				if natPort != 0 {
					{
						relay := client.client.relays[0]
						relay.Write(relay.ports[0], client.addr.IP, natPort) // A0 -> C1
						relay.Write(relay.ports[1], client.addr.IP, natPort) // A1 -> C1
					}
					{
						relay := client.client.relays[1]
						relay.Write(relay.ports[0], client.addr.IP, natPort) // B0 -> C1
					}
				}
				if client.client.natPorts[1] != 0 && client.client.natPorts[2] != 0 {
					client.Write(client.ports[1], client.addr.IP, client.client.natPorts[2]) // C1 -> C2
					client.Write(client.ports[2], client.addr.IP, client.client.natPorts[1]) // C2 -> C1
				}
			}
		}
	}
}

func closeConnection(key *net.TCPConn, info *MessageInfo) {
	c, ok := connections[key]
	if !ok {
		return
	}
	if info != nil {
		c.w <- info
	}
	close(c.w)
	delete(connections, key)
	if c.client != nil {
		return
	}
	for key, client := range connections {
		if client.client == nil {
			continue
		}
		for _, relay := range client.client.relays {
			if relay == c {
				closeConnection(key, &MessageInfo{
					MessageType: 0,
					Message:     "Internal error: Relay disconnected during the test.",
				})
				break
			}
		}
	}
}

func unique(a []int) bool {
	for i, v1 := range a {
		for v2 := range a[i+1:] {
			if v1 == v2 {
				return false
			}
		}
	}
	return true
}
