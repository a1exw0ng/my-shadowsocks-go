package shadowsocks

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	idType  = 0 // address type index
	idIP0   = 1 // ip addres start index
	idDmLen = 1 // domain address length index
	idDm0   = 2 // domain address start index

	typeIPv4 = 1 // type is ipv4 address
	typeDm   = 3 // type is domain address
	typeIPv6 = 4 // type is ipv6 address

	lenIPv4   = 1 + net.IPv4len + 2 // 1addrType + ipv4 + 2port
	lenIPv6   = 1 + net.IPv6len + 2 // 1addrType + ipv6 + 2port
	lenDmBase = 1 + 1 + 2           // 1addrType + 1addrLen + 2port, plus addrLen
)

type Conn struct {
	net.Conn
	*Cipher
}

type UDP interface {
	ReadFromUDP(b []byte) (n int, src *net.UDPAddr, err error)
	Read(b []byte) (n int, err error)
	WriteToUDP(b []byte, src *net.UDPAddr) (n int, err error)
	Write(b []byte) (n int, err error)
	Close() error
	SetWriteDeadline(t time.Time) error
	SetReadDeadline(t time.Time) error
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
	ReadFrom(b []byte) (int, net.Addr, error)
}

func NewConn(cn net.Conn, cipher *Cipher) *Conn {
	return &Conn{cn, cipher}
}

type UDPConn struct {
	UDP
	*Cipher
}

func NewUDPConn(cn UDP, cipher *Cipher) *UDPConn {
	return &UDPConn{cn, cipher}
}

type CachedUDPConn struct {
	timer *time.Timer
	UDP
	i string
}

func NewCachedUDPConn(cn UDP) *CachedUDPConn {
	return &CachedUDPConn{nil, cn, ""}
}

func (c *CachedUDPConn) Check() {
	nl.Delete(c.i)
}

func (c *CachedUDPConn) Close() error {
	c.timer.Stop()
	return c.UDP.Close()
}

func (c *CachedUDPConn) SetTimer(index string) {
	c.i = index
	c.timer = time.AfterFunc(120*time.Second, c.Check)
}

func (c *CachedUDPConn) Refresh() bool {
	return c.timer.Reset(120 * time.Second)
}

type NATlist struct {
	sync.Mutex
	Conns      map[string]*CachedUDPConn
	AliveConns int
}

func (nl *NATlist) Delete(srcaddr string) {
	nl.Lock()
	defer nl.Unlock()
	c, ok := nl.Conns[srcaddr]
	if ok {
		c.Close()
		delete(nl.Conns, srcaddr)
		nl.AliveConns -= 1
	}
	ReqList = map[string]*ReqNode{} //del all
}

func (nl *NATlist) Get(srcaddr *net.UDPAddr, ss *UDPConn) (c *CachedUDPConn, ok bool, err error) {
	nl.Lock()
	defer nl.Unlock()
	index := srcaddr.String()
	_, ok = nl.Conns[index]
	if !ok {
		//NAT not exists or expired
		Debug.Printf("new udp conn %v<-->%v\n", srcaddr, ss.LocalAddr())
		nl.AliveConns += 1
		ok = false
		//full cone
		addr, _ := net.ResolveUDPAddr("udp", ":0")
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			return nil, false, err
		}
		c = NewCachedUDPConn(conn)
		nl.Conns[index] = c
		c.SetTimer(index)
		go Pipeloop(ss, srcaddr, c)
	} else {
		//NAT exists
		c, _ = nl.Conns[index]
		c.Refresh()
	}
	err = nil
	return
}

func ParseHeader(addr net.Addr) []byte {
	//what if the request address type is domain???
	ip, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil
	}
	buf := make([]byte, 20)
	IP := net.ParseIP(ip)
	b1 := IP.To4()
	iplen := 0
	if b1 == nil { //ipv6
		b1 = IP.To16()
		buf[0] = typeIPv6
		iplen = net.IPv6len
	} else { //ipv4
		buf[0] = typeIPv4
		iplen = net.IPv4len
	}
	copy(buf[1:], b1)
	port_i, _ := strconv.Atoi(port)
	binary.BigEndian.PutUint16(buf[1+iplen:], uint16(port_i))
	return buf[:1+iplen+2]
}

func Pipeloop(ss *UDPConn, srcaddr *net.UDPAddr, remote UDP) {
	buf := pool.Get().([]byte)
	defer pool.Put(buf)
	defer nl.Delete(srcaddr.String())
	for {
		n, raddr, err := remote.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(*net.OpError); ok && (ne.Err == syscall.EMFILE || ne.Err == syscall.ENFILE) {
				// log too many open file error
				// EMFILE is process reaches open file limits, ENFILE is system limit
				fmt.Println("[udp]read error:", err)
			} else if ne.Err.Error() == "use of closed network connection" {
				fmt.Println("[udp]Connection Closing:", remote.LocalAddr())
			} else {
				fmt.Println("[udp]error reading from:", remote.LocalAddr(), err)
			}
			return
		}
		// need improvement here
		ReqListLock.RLock()
		N, ok := ReqList[raddr.String()]
		ReqListLock.RUnlock()
		if ok {
			ss.WriteToUDP(append(N.Req, buf[:n]...), srcaddr)
		} else {
			header := ParseHeader(raddr)
			ss.WriteToUDP(append(header, buf[:n]...), srcaddr)
		}
		upTraffic(strconv.Itoa(ss.LocalAddr().(*net.UDPAddr).Port), n, srcaddr.IP.String())
	}
}

type ReqNode struct {
	Req    []byte
	ReqLen int
}

var ReqListLock sync.RWMutex
var ReqList = map[string]*ReqNode{}

func HandleUDPConnection(c *UDPConn, openvpn string) {
	buf := pool.Get().([]byte)
	defer pool.Put(buf)
	for {
		n, src, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}

		var dstIP net.IP
		var reqLen int

		switch buf[idType] {
		case typeIPv4:
			reqLen = lenIPv4
			dstIP = net.IP(buf[idIP0 : idIP0+net.IPv4len])
		case typeIPv6:
			reqLen = lenIPv6
			dstIP = net.IP(buf[idIP0 : idIP0+net.IPv6len])
		case typeDm:
			reqLen = int(buf[idDmLen]) + lenDmBase
			dIP, err := net.ResolveIPAddr("ip", string(buf[idDm0:idDm0+buf[idDmLen]]))
			if err != nil {
				fmt.Sprintf("[udp]failed to resolve domain name: %s\n", string(buf[idDm0:idDm0+buf[idDmLen]]))
				return
			}
			dstIP = dIP.IP
		default:
			fmt.Sprintf("[udp]addr type %d not supported", buf[idType])
			return
		}
		ip := dstIP.String()
		p := strconv.Itoa(int(binary.BigEndian.Uint16(buf[reqLen-2 : reqLen])))
		if (strings.HasPrefix(ip, "127.") && (p != "1194" || openvpn != "ok")) ||
			strings.HasPrefix(ip, "10.8.") || ip == "::1" {
			log.Printf("[udp]illegal connect to local network(%s)\n", ip)
			return
		}
		dst, _ := net.ResolveUDPAddr("udp", net.JoinHostPort(ip, p))
		ReqListLock.Lock()
		if _, ok := ReqList[dst.String()]; !ok {
			req := make([]byte, reqLen)
			copy(req, buf)
			ReqList[dst.String()] = &ReqNode{req, reqLen}
		}
		ReqListLock.Unlock()

		remote, _, err := nl.Get(src, c)
		if err != nil {
			return
		}
		_, err = remote.WriteToUDP(buf[reqLen:n], dst)
		if err != nil {
			if ne, ok := err.(*net.OpError); ok && (ne.Err == syscall.EMFILE || ne.Err == syscall.ENFILE) {
				// log too many open file error
				// EMFILE is process reaches open file limits, ENFILE is system limit
				fmt.Println("[udp]write error:", err)
			} else {
				fmt.Println("[udp]error connecting to:", dst, err)
			}
			return
		}
		upTraffic(p, n, ip)
		// Pipeloop
	} // for
}

var nl = NATlist{Conns: map[string]*CachedUDPConn{}}

func RawAddr(addr string) (buf []byte, err error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("shadowsocks: address error %s %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("shadowsocks: invalid port %s", addr)
	}

	hostLen := len(host)
	l := 1 + 1 + hostLen + 2 // addrType + lenByte + address + port
	buf = make([]byte, l)
	buf[0] = 3             // 3 means the address is domain name
	buf[1] = byte(hostLen) // host address length  followed by host address
	copy(buf[2:], host)
	binary.BigEndian.PutUint16(buf[2+hostLen:2+hostLen+2], uint16(port))
	return
}

// This is intended for use by users implementing a local socks proxy.
// rawaddr shoud contain part of the data in socks request, starting from the
// ATYP field. (Refer to rfc1928 for more information.)
func DialWithRawAddr(rawaddr []byte, server string, cipher *Cipher) (c *Conn, err error) {
	conn, err := net.Dial("tcp", server)
	if err != nil {
		return
	}
	c = NewConn(conn, cipher)
	if _, err = c.Write(rawaddr); err != nil {
		c.Close()
		return nil, err
	}
	return
}

// addr should be in the form of host:port
func Dial(addr, server string, cipher *Cipher) (c *Conn, err error) {
	ra, err := RawAddr(addr)
	if err != nil {
		return
	}
	return DialWithRawAddr(ra, server, cipher)
}

//n is the size of the payload
func (c *UDPConn) ReadFromUDP(b []byte) (n int, src *net.UDPAddr, err error) {
	buf := pool.Get().([]byte)
	defer pool.Put(buf)

	n, src, err = c.UDP.ReadFromUDP(buf)
	if err != nil {
		return
	}

	iv := buf[:c.info.ivLen]
	if err = c.initDecrypt(iv); err != nil {
		return
	}
	c.decrypt(b[0:n-c.info.ivLen], buf[c.info.ivLen:n])
	n = n - c.info.ivLen
	return
}

func (c *UDPConn) Read(b []byte) (n int, err error) {
	buf := pool.Get().([]byte)
	defer pool.Put(buf)

	n, err = c.UDP.Read(buf)
	if err != nil {
		return
	}

	iv := buf[:c.info.ivLen]
	if err = c.initDecrypt(iv); err != nil {
		return
	}
	c.decrypt(b[0:n-c.info.ivLen], buf[c.info.ivLen:n])
	n = n - c.info.ivLen
	return
}

//n = iv + payload
func (c *UDPConn) WriteToUDP(b []byte, src *net.UDPAddr) (n int, err error) {
	var cipherData []byte
	dataStart := 0

	var iv []byte
	iv, err = c.initEncrypt()
	if err != nil {
		return
	}
	// Put initialization vector in buffer, do a single write to send both
	// iv and data.
	cipherData = make([]byte, len(b)+len(iv))
	copy(cipherData, iv)
	dataStart = len(iv)

	c.encrypt(cipherData[dataStart:], b)
	n, err = c.UDP.WriteToUDP(cipherData, src)
	return
}

func (c *UDPConn) Write(b []byte) (n int, err error) {
	var cipherData []byte
	dataStart := 0

	var iv []byte
	iv, err = c.initEncrypt()
	if err != nil {
		return
	}
	// Put initialization vector in buffer, do a single write to send both
	// iv and data.
	cipherData = make([]byte, len(b)+len(iv))
	copy(cipherData, iv)
	dataStart = len(iv)

	c.encrypt(cipherData[dataStart:], b)
	n, err = c.UDP.Write(cipherData)
	return
}

func (c *Conn) Read(b []byte) (n int, err error) {
	if c.dec == nil {
		iv := make([]byte, c.info.ivLen)
		if _, err = io.ReadFull(c.Conn, iv); err != nil {
			return
		}
		if err = c.initDecrypt(iv); err != nil {
			return
		}
	}
	cipherData := make([]byte, len(b))
	n, err = c.Conn.Read(cipherData)
	if n > 0 {
		c.decrypt(b[0:n], cipherData[0:n])
	}
	return
}

func (c *Conn) Write(b []byte) (n int, err error) {
	var cipherData []byte
	dataStart := 0
	if c.enc == nil {
		var iv []byte
		iv, err = c.initEncrypt()
		if err != nil {
			return
		}
		// Put initialization vector in buffer, do a single write to send both
		// iv and data.
		cipherData = make([]byte, len(b)+len(iv))
		copy(cipherData, iv)
		dataStart = len(iv)
	} else {
		cipherData = make([]byte, len(b))
	}
	c.encrypt(cipherData[dataStart:], b)
	n, err = c.Conn.Write(cipherData)
	return
}
