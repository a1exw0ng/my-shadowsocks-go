package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	ss "github.com/shadowsocks/shadowsocks-go/shadowsocks"
)

const dnsGoroutineNum = 64

func getRequest(conn *ss.Conn) (host, port string, extra []byte, err error) {
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

	// buf size should at least have the same size with the largest possible
	// request size (when addrType is 3, domain name has at most 256 bytes)
	// 1(addrType) + 1(lenByte) + 256(max length address) + 2(port)
	buf := make([]byte, 260)
	var n int
	// read till we get possible domain length field
	ss.SetReadTimeout(conn)
	if n, err = io.ReadAtLeast(conn, buf, idDmLen+1); err != nil {
		return
	}

	reqLen := -1
	switch buf[idType] {
	case typeIPv4:
		reqLen = lenIPv4
	case typeIPv6:
		reqLen = lenIPv6
	case typeDm:
		reqLen = int(buf[idDmLen]) + lenDmBase
	default:
		err = fmt.Errorf("addr type %d not supported", buf[idType])
		return
	}

	if n < reqLen { // rare case
		ss.SetReadTimeout(conn)
		if _, err = io.ReadFull(conn, buf[n:reqLen]); err != nil {
			return
		}
	} else if n > reqLen {
		// it's possible to read more than just the request head
		extra = buf[reqLen:n]
	}

	// Return string for typeIP is not most efficient, but browsers (Chrome,
	// Safari, Firefox) all seems using typeDm exclusively. So this is not a
	// big problem.
	switch buf[idType] {
	case typeIPv4:
		host = net.IP(buf[idIP0 : idIP0+net.IPv4len]).String()
	case typeIPv6:
		host = net.IP(buf[idIP0 : idIP0+net.IPv6len]).String()
	case typeDm:
		host = string(buf[idDm0 : idDm0+buf[idDmLen]])
	}
	// parse port
	port = strconv.Itoa(int(binary.BigEndian.Uint16(buf[reqLen-2 : reqLen])))
	return
}

const logCntDelta = 100

var connCnt uint64 // operate by sync/atomic

func handleConnection(conn *ss.Conn, port string, pflag *uint32, openvpn string) {
	var host string

	newConnCnt := atomic.AddUint64(&connCnt, 1) // connCnt++
	if newConnCnt%logCntDelta == 0 {
		log.Printf("Number of client connections reaches %d\n", newConnCnt)
	}

	// function arguments are always evaluated, so surround debug statement
	// with if statement
	ss.Debug.Printf("new client %s->%s\n", conn.RemoteAddr().String(), conn.LocalAddr())
	closed := false
	defer func() {
		ss.Debug.Printf("closed pipe %s<->%s\n", conn.RemoteAddr(), host)
		atomic.AddUint64(&connCnt, ^uint64(0)) // connCnt--
		if !closed {
			conn.Close()
		}
	}()

	h, p, extra, err := getRequest(conn)
	if err != nil {
		log.Println("error getting request", conn.RemoteAddr(), conn.LocalAddr(), err)
		return
	}
	host = h + ":" + p
	ss.Debug.Println("connecting", host)
	addr, err := net.ResolveIPAddr("ip", h)
	if err != nil {
		log.Println(err)
		return
	}
	ip := addr.String()
	if (strings.HasPrefix(ip, "127.") && (p != "1194" || openvpn != "ok")) ||
		strings.HasPrefix(ip, "10.8.") || ip == "::1" {
		log.Printf("illegal connect to local network(%s)\n", ip)
		return
	}
	remote, err := net.Dial("tcp", net.JoinHostPort(ip, p))
	if err != nil {
		if ne, ok := err.(*net.OpError); ok && (ne.Err == syscall.EMFILE || ne.Err == syscall.ENFILE) {
			// log too many open file error
			// EMFILE is process reaches open file limits, ENFILE is system limit
			log.Println("dial error:", err)
		} else {
			log.Println("error connecting to:", host, err)
		}
		return
	}
	defer func() {
		if !closed {
			remote.Close()
		}
	}()
	// write extra bytes read from
	if extra != nil {
		// Debug.Println("getRequest read extra data, writing to remote, len", len(extra))
		if _, err = remote.Write(extra); err != nil {
			ss.Debug.Println("write request extra error:", err)
			return
		}
	}
	ss.Debug.Printf("ping %s<->%s", conn.RemoteAddr(), host)
	go ss.PipeThenClose(conn, remote, ss.SET_TIMEOUT, pflag, port, "out")
	ss.PipeThenClose(remote, conn, ss.NO_TIMEOUT, pflag, port, "in")
	closed = true
	return
}

type PortListener struct {
	password string
	openvpn  string
	udp      string
	listener net.Listener
	pflag    *uint32
}

type UDPListener struct {
	password string
	openvpn  string
	udp      string
	listener *net.UDPConn
}

type PasswdManager struct {
	sync.Mutex
	portListener map[string]*PortListener
	udpListener  map[string]*UDPListener
}

func (pm *PasswdManager) add(port string, password [3]string, listener net.Listener, pflag *uint32) {
	pm.Lock()
	pm.portListener[port] = &PortListener{password[0], password[1], password[2], listener, pflag}
	pm.Unlock()

	ss.AddTraffic(port)
}

func (pm *PasswdManager) addUDP(port string, password [3]string, listener *net.UDPConn) {
	pm.Lock()
	pm.udpListener[port] = &UDPListener{password[0], password[1], password[2], listener}
	pm.Unlock()

	ss.AddTraffic(port)
}

func (pm *PasswdManager) get(port string) (pl *PortListener, ok bool) {
	pm.Lock()
	pl, ok = pm.portListener[port]
	pm.Unlock()
	return
}

func (pm *PasswdManager) getUDP(port string) (pl *UDPListener, ok bool) {
	pm.Lock()
	pl, ok = pm.udpListener[port]
	pm.Unlock()
	return
}

func (pm *PasswdManager) del(port string) {
	pl, ok := pm.get(port)
	if !ok {
		return
	}
	if udp {
		upl, ok := pm.getUDP(port)
		if !ok {
			return
		}
		upl.listener.Close()
	}
	pl.listener.Close()
	pm.Lock()
	delete(pm.portListener, port)
	if udp {
		delete(pm.udpListener, port)
	}
	pm.Unlock()

	atomic.StoreUint32(pl.pflag, 1)

	ss.DelTraffic(port)
}

// Update port password would first close a port and restart listening on that
// port. A different approach would be directly change the password used by
// that port, but that requires **sharing** password between the port listener
// and password manager.
func (pm *PasswdManager) updatePortPasswd(port string, password [3]string) {
	if pl, ok := pm.get(port); !ok {
		log.Printf("new port %s added\n", port)
	} else {
		if pl.password != password[0] || pl.openvpn != password[1] {
			log.Printf("closing port %s to update config", port)
			pl.listener.Close()
			if udp {
				if pl, ok := pm.getUDP(port); ok {
					log.Printf("[udp]closing port %s to update config", port)
					pl.listener.Close()
				}
			}
		} else if udp && pl.udp != password[2] {
			if pl, ok := pm.getUDP(port); ok {
				log.Printf("[udp]closing port %s to update config", port)
				pl.listener.Close()
			}
		} else {
			// nothing to change
			return
		}
	}
	// run will add the new port listener to passwdManager.
	// So there maybe concurrent access to passwdManager and we need lock to protect it.
	go run(port, password)

	if udp && password[2] == "ok" {
		go runUDP(port, password)
	}

}

var passwdManager = PasswdManager{portListener: map[string]*PortListener{}, udpListener: map[string]*UDPListener{}}

func updatePasswd() {
	log.Println("updating password")
	newconfig, err := ss.ParseConfig(configFile)
	if err != nil {
		log.Printf("error parsing config file %s to update password: %v\n", configFile, err)
		return
	}
	oldconfig := config
	config = newconfig

	if err = unifyPortPassword(config); err != nil {
		config = oldconfig
		return
	}
	for port, passwd := range config.PortPassword {
		passwdManager.updatePortPasswd(port, passwd)
		if oldconfig.PortPassword != nil {
			delete(oldconfig.PortPassword, port)
		}
	}
	// port password still left in the old config should be closed, delete Traffic
	for port, _ := range oldconfig.PortPassword {
		log.Printf("closing port %s as it's deleted\n", port)
		passwdManager.del(port)
	}
	log.Println("password updated")
}

func waitSignal() {
	var sigChan = make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP)
	for sig := range sigChan {
		if sig == syscall.SIGHUP {
			updatePasswd()
		} else {
			// is this going to happen?
			log.Printf("caught signal %v, exit", sig)
			os.Exit(0)
		}
	}
}

func run(port string, password [3]string) {
	ln, err := net.Listen(netTcp, ":"+port)
	if err != nil {
		log.Printf("error listening port %v: %v\n", port, err)
		return
	}
	var flag uint32 = 0
	passwdManager.add(port, password, ln, &flag)
	var cipher *ss.Cipher
	log.Printf("server listening port %v ...\n", port)
	for {
		conn, err := ln.Accept()
		if err != nil {
			// listener maybe closed to update password
			ss.Debug.Printf("accept error: %v\n", err)
			return
		}
		// Creating cipher upon first connection.
		if cipher == nil {
			log.Println("creating cipher for port:", port)
			cipher, err = ss.NewCipher(config.Method, password[0])
			if err != nil {
				log.Printf("Error generating cipher for port: %s %v\n", port, err)
				conn.Close()
				continue
			}
		}
		go handleConnection(ss.NewConn(conn, cipher.Copy()), port, &flag, password[1])
	}
}

func runUDP(port string, password [3]string) {
	addr, _ := net.ResolveUDPAddr(netUdp, ":"+port)
	conn, err := net.ListenUDP(netUdp, addr)
	if err != nil {
		log.Printf("error listening udp port %v: %v\n", port, err)
		return
	}
	passwdManager.addUDP(port, password, conn)
	log.Printf("server listening udp port %v ...\n", port)
	defer conn.Close()
	var cipher *ss.Cipher
	cipher, err = ss.NewCipher(config.Method, password[0])
	if err != nil {
		log.Printf("Error generating cipher for udp port: %s %v\n", port, err)
		conn.Close()
	}
	ss.HandleUDPConnection(ss.NewUDPConn(conn, cipher.Copy()), password[1])
}

func enoughOptions(config *ss.Config) bool {
	return config.ServerPort != 0 && config.Password != ""
}

func unifyPortPassword(config *ss.Config) (err error) {
	if len(config.PortPassword) == 0 { // this handles both nil PortPassword and empty one
		if enoughOptions(config) {
			port := strconv.Itoa(config.ServerPort)
			config.PortPassword = map[string][3]string{port: [3]string{config.Password}}
		}
	} else {
		if config.Password != "" || config.ServerPort != 0 {
			fmt.Fprintln(os.Stderr, "given port_password, ignore server_port and password option")
		}
	}
	return
}

var configFile string
var config *ss.Config
var netTcp, netUdp string
var udp bool

func main() {
	log.SetOutput(os.Stdout)

	var cmdConfig ss.Config
	var printVer, debug bool
	var core int

	flag.BoolVar(&printVer, "version", false, "print version")
	flag.StringVar(&configFile, "c", "config.json", "specify config file")
	flag.StringVar(&cmdConfig.Password, "k", "", "password")
	flag.IntVar(&cmdConfig.ServerPort, "p", 0, "server port")
	flag.IntVar(&cmdConfig.Timeout, "t", 60, "connection timeout (in seconds)")
	flag.StringVar(&cmdConfig.Method, "m", "", "encryption method, default: aes-256-cfb")
	flag.IntVar(&cmdConfig.Net, "n", 0, "ipv4(4) or ipv6(6) or both(0), default is both")
	flag.IntVar(&core, "core", 0, "maximum number of CPU cores to use, default is determinied by logical CPUs on server")
	flag.BoolVar(&udp, "u", false, "UDP Relay")
	flag.BoolVar(&debug, "d", false, "print debug message")
	flag.Parse()

	if printVer {
		ss.PrintVersion()
		os.Exit(0)
	}

	ss.SetDebug(debug)

	var err error
	config, err = ss.ParseConfig(configFile)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "error reading %s: %v\n", configFile, err)
			os.Exit(1)
		}
		config = &cmdConfig
	} else {
		ss.UpdateConfig(config, &cmdConfig)
	}
	switch config.Net {
	case 4:
		netTcp = "tcp4"
		netUdp = "udp4"
	case 6:
		netTcp = "tcp6"
		netUdp = "udp6"
	default:
		netTcp = "tcp"
		netUdp = "udp"
	}
	if config.Method == "" {
		config.Method = "aes-256-cfb"
	}
	if err = ss.CheckCipherMethod(config.Method); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err = unifyPortPassword(config); err != nil {
		os.Exit(1)
	}
	if core > 0 {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}
	ss.NewTraffic()
	for port, password := range config.PortPassword {
		go run(port, password)
		if udp && password[2] == "ok" {
			go runUDP(port, password)
		}
	}

	waitSignal()
}
