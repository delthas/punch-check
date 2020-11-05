package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/delthas/punch-check"
)

var retryTimeout = 15 * time.Second

var logErr = log.New(os.Stderr, "", log.Ldate|log.Ltime|log.Lshortfile)
var logDebug *log.Logger

var mutex sync.Mutex
var control *net.TCPConn

var cs []*net.UDPConn
var ports []int

var closed uint32 = 0

func writeControl(m Message) {
	mutex.Lock()
	defer mutex.Unlock()
	if control == nil {
		return
	}
	WriteMessage(control, m)
}

func main() {
	serverHost := flag.String("host", "", "server hostname[:port] (required)")
	debug := flag.Bool("debug", false, "add debug logging")
	var portsStr []string
	flag.Var((*StringSliceFlag)(&portsStr), "port", "port to listen on (pass multiple times for multiple ports)")
	flag.Parse()

	if len(portsStr) < RelayPortsCount {
		fmt.Fprintf(os.Stderr, "at least %d ports are required (use -port)\n", RelayPortsCount)
		flag.Usage()
		return
	}
	if *serverHost == "" {
		fmt.Fprintf(os.Stderr, "-host is required\n")
		flag.Usage()
		return
	}

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

	cs = make([]*net.UDPConn, len(portsStr))
	ports = make([]int, len(cs))
	for i, portStr := range portsStr {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			log.Fatalf("failed parsing UDP port %d: %v", port, err)
		}
		c, err := net.ListenUDP("udp4", &net.UDPAddr{
			Port: port,
		})
		if err != nil {
			log.Fatalf("failed creating UDP socket for port %d: %v", port, err)
		}
		cs[i] = c
		ports[i] = port
	}

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
				writeControl(&MessageReceive{
					LocalPort: ports[i],
					IP:        addr.IP,
					Port:      addr.Port,
					Data:      buf,
				})
			}
		}()
	}

	first := true
	for {
		if !first {
			time.Sleep(15 * time.Second)
		} else {
			first = false
		}
		c, err := net.DialTCP("tcp4", nil, serverAddr)
		if err != nil {
			logErr.Printf("failed dialing server at %q, retrying in %v: %v", *serverHost, retryTimeout, err)
			continue
		}
		c.SetNoDelay(true)
		logErr.Printf("connected to server: %q", *serverHost)
		WriteMessage(c, &MessagePorts{
			Ports: ports,
		})
		mutex.Lock()
		control = c
		mutex.Unlock()

	outer:
		for {
			m, err := ReadMessage(control)
			if err != nil {
				logErr.Printf("reading message from control socket: %v", err)
				break
			}
			switch m := m.(type) {
			case *MessageSend:
				index := Index(ports, m.LocalPort)
				if index == -1 {
					logErr.Fatalf("invalid send message: invalid local port: %d", m.LocalPort)
				}
				logDebug.Printf("writing to %s:%d from %d: %v", net.IP(m.IP).String(), m.Port, m.LocalPort, m.Data)
				cs[index].WriteToUDP(m.Data, &net.UDPAddr{
					IP:   m.IP,
					Port: m.Port,
				})
			default:
				logErr.Printf("invalid message type: %v", MessageType(m.Type()))
				break outer
			}
		}

		logErr.Printf("disconnected from server, retrying in %v", retryTimeout)
		mutex.Lock()
		c.Close()
		control = nil
		mutex.Unlock()
	}
}
