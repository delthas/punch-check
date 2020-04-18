package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"

	. "github.com/delthas/punch-check"
)

var defaultServerHost = "delthas.fr"
var defaultStartPort = 34500

var logErr = log.New(os.Stderr, "", 0)
var logDebug *log.Logger

var closed uint32 = 0

func main() {
	serverHost := flag.String("host", defaultServerHost, "server hostname[:port]")
	debug := flag.Bool("debug", false, "add debug logging")
	flag.Parse()

	if *debug {
		logDebug = log.New(os.Stderr, "debug: ", log.Ldate|log.Ltime|log.Lshortfile)
	} else {
		logDebug = log.New(ioutil.Discard, "", 0)
	}

	var serverAddr *net.TCPAddr
	_, _, err := net.SplitHostPort(*serverHost)
	if err != nil {
		serverAddr, err = ResolveTCPBySRV("punchcheck", *serverHost)
		if err != nil {
			logErr.Fatalf("failed resolving server host %q: %v", *serverHost, err)
		}
	} else {
		serverAddr, err = net.ResolveTCPAddr("tcp4", *serverHost)
		if err != nil {
			logErr.Fatalf("failed resolving server host %q: %v", *serverHost, err)
		}
	}

	defer atomic.StoreUint32(&closed, 1)

	cs := make([]*net.UDPConn, 10)
	var startPort int
outer:
	for startPort = defaultStartPort; ; startPort += len(cs) {
		for i := range cs {
			c, err := net.ListenUDP("udp4", &net.UDPAddr{
				Port: startPort + i,
			})
			if err != nil {
				if startPort > defaultStartPort+len(cs)*10 {
					log.Fatalf("failed creating UDP sockets after 10 tries: %v", err)
				}
				continue outer
			}
			cs[i] = c
		}
		break
	}
	ports := make([]int, len(cs))
	for i := range ports {
		ports[i] = startPort + i
	}

	control, err := net.DialTCP("tcp4", nil, serverAddr)
	if err != nil {
		logErr.Fatalf("failed dialing server at %q: %v", *serverHost, err)
	}
	control.SetNoDelay(true)
	defer control.Close()
	logDebug.Printf("connected to server: %q", *serverHost)

	WriteMessage(control, &MessagePorts{
		Ports: ports,
	})

	var writeLock sync.Mutex
	for i, c := range cs {
		c := c
		i := i
		go func() {
			for {
				buf := make([]byte, 1536)
				n, addr, err := c.ReadFromUDP(buf)
				if err != nil {
					if atomic.LoadUint32(&closed) == 1 {
						return
					}
					logErr.Fatalf("reading from UDP socket: %v", err)
				}
				buf = buf[:n]

				logDebug.Printf("forwarding read from %s:%d on %d: %v", addr.IP.String(), addr.Port, ports[i], buf)
				writeLock.Lock()
				WriteMessage(control, &MessageReceive{
					LocalPort: ports[i],
					IP:        addr.IP,
					Port:      addr.Port,
					Data:      buf,
				})
				writeLock.Unlock()
			}
		}()
	}

	for {
		m, err := ReadMessage(control)
		if err != nil {
			logErr.Fatalf("reading message from control socket: %v", err)
			return
		}
		switch m := m.(type) {
		case *MessageSend:
			if m.LocalPort < startPort || m.LocalPort >= startPort+len(ports) {
				logErr.Fatalf("invalid send message: invalid local port: %d", m.LocalPort)
			}
			logDebug.Printf("writing to %s:%d from %d: %v", net.IP(m.IP).String(), m.Port, m.LocalPort, m.Data)
			cs[m.LocalPort-startPort].WriteToUDP(m.Data, &net.UDPAddr{
				IP:   m.IP,
				Port: m.Port,
			})
		case *MessageInfo:
			if m.MessageType == 0 {
				logErr.Fatalf("error: %s", m.Message)
			} else if m.MessageType != 1 {
				logErr.Fatalf("message of unknown message type %d: %s", m.MessageType, m.Message)
			}
			fmt.Println(m.Message)
			return
		default:
			logErr.Fatalf("invalid message type: %v", MessageType(m.Type()))
		}
	}
}
