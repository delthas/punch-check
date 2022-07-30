package punch

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"strconv"
)

type MessageType byte

var (
	SendType    MessageType = 0
	ReceiveType MessageType = 1
	InfoType    MessageType = 2
	PortsType   MessageType = 3
)

type Message interface {
	Type() MessageType
}

type MessageInfo struct {
	MessageType int    `json:"message_type"`
	Message     string `json:"message"`
}

func (m *MessageInfo) Type() MessageType {
	return InfoType
}

type MessagePorts struct {
	Ports []int `json:"ports"`
}

func (m *MessagePorts) Type() MessageType {
	return PortsType
}

type MessageSend struct {
	LocalPort int    `json:"local_port"`
	IP        []byte `json:"ip"`
	Port      int    `json:"port"`
	Data      []byte `json:"data"`
}

func (m *MessageSend) Type() MessageType {
	return SendType
}

type MessageReceive struct {
	LocalPort int    `json:"local_port"`
	IP        []byte `json:"ip"`
	Port      int    `json:"port"`
	Data      []byte `json:"data"`
}

func (m *MessageReceive) Type() MessageType {
	return ReceiveType
}

func ReadMessage(c *net.TCPConn) (Message, error) {
	header := make([]byte, 3)
	n, err := io.ReadFull(c, header)
	if err != nil {
		return nil, fmt.Errorf("reading message header: read error: %v", err)
	}
	mt := header[0]
	ml := int64(binary.BigEndian.Uint16(header[1:]))
	var m Message
	switch MessageType(mt) {
	case SendType:
		m = &MessageSend{}
	case ReceiveType:
		m = &MessageReceive{}
	case InfoType:
		m = &MessageInfo{}
	case PortsType:
		m = &MessagePorts{}
	default:
		return nil, fmt.Errorf("reading message: unknown message type: %v", MessageType(n))
	}
	jr := io.LimitReader(c, ml)
	jd := json.NewDecoder(jr)
	if err := jd.Decode(&m); err != nil {
		return nil, fmt.Errorf("reading message: parsing message: %v", err)
	}
	if _, err := io.Copy(ioutil.Discard, jr); err != nil {
		return nil, fmt.Errorf("reading message: seeking to end of json payload: %v", err)
	}
	return m, nil
}

func WriteMessage(c *net.TCPConn, m Message) error {
	header := make([]byte, 3)
	header[0] = byte(m.Type())
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("writing message: marshaling error: %v", err)
	}
	binary.BigEndian.PutUint16(header[1:], uint16(len(data)))
	if _, err := c.Write(header); err != nil {
		return fmt.Errorf("writing message header: write error: %v", err)
	}
	if _, err := c.Write(data); err != nil {
		return fmt.Errorf("writing message data: write error: %v", err)
	}
	return nil
}

func Index(a []int, e int) int {
	for i, v := range a {
		if v == e {
			return i
		}
	}
	return -1
}

func ResolveTCPBySRV(service string, host string) (*net.TCPAddr, error) {
	_, srvs, err := net.LookupSRV(service, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("resolving service %q of host %q: %v", service, host, err)
	}
	if len(srvs) == 0 {
		return nil, fmt.Errorf("resolving service %q of host %q: no SRV records found", service, host)
	}
	var lastRecord string
	for _, srv := range srvs {
		var addr *net.TCPAddr
		addr, err = net.ResolveTCPAddr("tcp4", net.JoinHostPort(srv.Target, strconv.Itoa(int(srv.Port))))
		if err != nil {
			lastRecord = srv.Target
			continue
		}
		return addr, nil
	}
	return nil, fmt.Errorf("resolving service %q of host %q: resolving %q: %v", service, host, lastRecord, err)
}

type StringSliceFlag []string

func (v *StringSliceFlag) String() string {
	return fmt.Sprint([]string(*v))
}

func (v *StringSliceFlag) Set(s string) error {
	*v = append(*v, s)
	return nil
}

var ClientRelaysCount = 2
var ClientPortsCount = 5
var RelayPortsCount = 2
