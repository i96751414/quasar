package util

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/i96751414/quasar/config"
)

var pingTimeout = 100 * time.Millisecond

func GetHTTPHost() string {
	return fmt.Sprintf("http://localhost:%d", config.ListenPort)
}

func GetListenAddr(interfaces string, portMin int, portMax int) (IPv4 string, IPv6 *string, port int, err error) {

	var listenIPv4s []string
	var listenIPv6s []string

	interfaces = strings.TrimSpace(interfaces)
	if interfaces != "" {
		listenIPv4s, listenIPv6s = getInterfacesIPs(strings.Split(strings.Replace(interfaces, " ", "", -1), ","))

		if len(listenIPv4s) == 0 {
			err = fmt.Errorf("could not find IP for specified interfaces: %s", interfaces)
			return
		}
	}

	if len(listenIPv4s) == 0 {
		listenIPv4s = append(listenIPv4s, "")
	}
	if len(listenIPv6s) == 0 {
		listenIPv6s = append(listenIPv6s, "")
	}

	IPv4, port, err = getIPv4AndPort(portMin, portMax, listenIPv4s)
	if err == nil {
		for _, ip := range listenIPv6s {
			addr := net.JoinHostPort(ip, strconv.Itoa(port))
			if !isPortUsed("tcp6", addr) {
				IPv6 = &ip
				break
			}
		}
	}

	return
}

func getIPv4AndPort(portMin, portMax int, IPv4s [] string) (ip string, port int, err error) {
	for p := portMin; p <= portMax; p++ {
		for _, i := range IPv4s {
			addr := net.JoinHostPort(i, strconv.Itoa(p))
			if !isPortUsed("tcp", addr) && !isPortUsed("udp", addr) {
				return i, p, nil
			}
		}
	}
	err = errors.New("unable to get IP and port from provided settings")
	return
}

func getInterfacesIPs(interfaces []string) (IPv4s []string, IPv6s []string) {
	for _, interfaceName := range interfaces {
		if addr := net.ParseIP(interfaceName); addr != nil {
			IPv4s = append(IPv4s, addr.To4().String())
			continue
		}

		i, err := net.InterfaceByName(interfaceName)
		if err != nil {
			log.Infof("Could not get IP for interface %s.", interfaceName)
			continue
		}

		var addresses []net.Addr
		if addresses, err = i.Addrs(); err == nil {
			for _, addr := range addresses {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip == nil || ip.IsLoopback() {
					continue
				}

				v6 := ip.To16()
				v4 := ip.To4()

				if v6 != nil && v4 == nil {
					IPv6s = append(IPv6s, v6.String()+"%"+interfaceName)
				}
				if v4 != nil {
					IPv4s = append(IPv4s, v4.String())
				}
			}
		}
	}
	return
}

func isPortUsed(network, addr string) bool {
	var err error
	if strings.Contains(network, "tcp") {
		err = pingTCP(network, addr)
	} else {
		err = pingUdp(network, addr)
	}
	return err == nil
}

func pingTCP(network, addr string) error {
	conn, err := net.DialTimeout(network, addr, pingTimeout)
	if conn != nil {
		_ = conn.Close()
	}

	return err
}

func pingUdp(network, addr string) (err error) {
	c, err := net.Dial(network, addr)
	if err != nil {
		return err
	}

	_, _ = c.Write([]byte("Ping!Ping!Ping!"))
	_ = c.SetReadDeadline(time.Now().Add(pingTimeout))

	rb := make([]byte, 1500)
	_, err = c.Read(rb)
	_ = c.Close()
	return err
}
