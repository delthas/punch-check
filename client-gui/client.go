//go:generate rsrc -arch=amd64 -manifest client.manifest -o rsrc_amd64.syso
//go:generate rsrc -arch=386 -manifest client.manifest -o rsrc_386.syso

package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	. "github.com/delthas/punch-check"
)

var serverHost = "punchcheckback.delthas.fr:23458"
var defaultStartPort = 34500

var mw *walk.MainWindow
var text *walk.TextEdit
var progress *walk.ProgressBar

func log(f string, args ...interface{}) {
	mw.Synchronize(func() {
		text.AppendText(strings.ReplaceAll(fmt.Sprintf(f+"\n", args...), "\n", "\r\n"))
	})
}

func process() {
	serverAddr, err := net.ResolveTCPAddr("tcp4", serverHost)
	if err != nil {
		log("failed resolving server host %q: %v", serverHost, err)
		return
	}

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
					log("failed creating UDP sockets after 10 tries: %v", err)
					return
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
		log("failed dialing server at %q: %v", serverHost, err)
		return
	}
	control.SetNoDelay(true)
	defer control.Close()

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
					log("reading from UDP socket: %v", err)
					return
				}
				buf = buf[:n]

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
			log("reading message from control socket: %v", err)
			return
		}
		switch m := m.(type) {
		case *MessageSend:
			if m.LocalPort < startPort || m.LocalPort >= startPort+len(ports) {
				log("invalid send message: invalid local port: %d", m.LocalPort)
				return
			}
			cs[m.LocalPort-startPort].WriteToUDP(m.Data, &net.UDPAddr{
				IP:   m.IP,
				Port: m.Port,
			})
		case *MessageInfo:
			if m.MessageType == 0 {
				log("error: %s", m.Message)
			} else if m.MessageType != 1 {
				log("message of unknown message type %d: %s", m.MessageType, m.Message)
			} else {
				log("%s", m.Message)
			}
			return
		default:
			log("invalid message type: %v", MessageType(m.Type()))
			return
		}
	}
}

func main() {
	size := Size{Width: 400, Height: 250}
	err := MainWindow{
		AssignTo: &mw,
		Title:    "PunchCheck",
		MinSize:  size,
		Size:     size,
		Layout:   VBox{},
		Children: []Widget{
			TextEdit{
				AssignTo: &text,
				ReadOnly: true,
			},
			ProgressBar{
				AssignTo:    &progress,
				MarqueeMode: true,
				Value:       1,
			},
		},
	}.Create()
	if err != nil {
		panic(err)
	}

	go func() {
		process()
		mw.Synchronize(func() {
			progress.SetMarqueeMode(false)
			progress.SetValue(100)
		})
	}()

	code := mw.Run()
	os.Exit(code)
}
