package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/krolaw/dhcp4"
	"github.com/psanford/dhcpeterd/config"
	"github.com/psanford/dhcpeterd/internal/dhcp4d"
)

var confPath = flag.String("config", "dhcpeterd.toml", "Config path")

func main() {
	flag.Parse()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	conf, err := config.Load(*confPath)
	if err != nil {
		slog.Error("load config err", "err", err)
		os.Exit(1)
	}

	lm := newLeaseManager(conf.LeaseFile)
	go lm.updateLeaseFileLoop(ctx)

	for _, network := range conf.Networks {
		n := network
		go func() {
			err := run(n, lm)
			if err != nil {
				slog.Error("run error", "iface", n.Interface, "err", err)
				os.Exit(1)
			}
		}()
	}

	<-c
}

func run(conf config.Network, lm *leaseManager) error {
	iface, err := net.InterfaceByName(conf.Interface)
	if err != nil {
		return err
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return err
	}

	startIP := net.ParseIP(conf.StartIP)
	if startIP == nil {
		return fmt.Errorf("parse start_ip on %s error invalid: %s", conf.Interface, conf.StartIP)
	}

	var matchIPNet *net.IPNet

	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}

		if ipnet.Contains(startIP) {
			matchIPNet = ipnet
			break
		}
	}

	if matchIPNet == nil {
		return fmt.Errorf("failed to find network %s on %s", conf.StartIP, conf.Interface)
	}

	netmask := net.ParseIP(conf.NetMask)
	if netmask == nil {
		return fmt.Errorf("parse netmask on %s error invalid: %s", conf.Interface, conf.NetMask)
	}
	serverIP := matchIPNet.IP

	staticLeases := make([]dhcp4d.StaticLease, 0, len(conf.StaticLeases))
	for _, sl := range conf.StaticLeases {
		ip := net.ParseIP(sl.IP)
		if ip == nil {
			slog.Error("invalid static ip", "ip", sl.IP)
			continue
		}

		staticLeases = append(staticLeases, dhcp4d.StaticLease{
			Addr:         ip.To4(),
			HardwareAddr: sl.MacAddress,
			Hostname:     sl.Name,
		})
	}

	handler, err := dhcp4d.NewHandler(iface, serverIP, startIP, netmask, conf.Range, conf.LeaseDuration, conf.DNSServers, staticLeases)

	existingLeases := lm.lf.LeaseByInterface[conf.Interface]
	if len(existingLeases) > 0 {
		leases := make([]*dhcp4d.Lease, len(existingLeases))
		for i, l := range existingLeases {
			l := l
			leases[i] = &l
		}
		handler.SetLeases(leases)
	}

	handler.Leases = func(newLeases []*dhcp4d.Lease, latest *dhcp4d.Lease) {
		leases := make([]dhcp4d.Lease, len(newLeases))

		for i, l := range newLeases {
			leases[i] = *l
		}

		lm.leaseUpdate <- LeaseUpdate{
			IfaceName: conf.Interface,
			Leases:    leases,
		}
	}

	conn, err := newUDP4BoundListener(conf.Interface, ":67")
	if err != nil {
		return err
	}
	slog.Info("listen", "iface", conf.Interface, "server_ip", serverIP, "iface2", iface.Name, "start_ip", conf.StartIP)
	return dhcp4.Serve(conn, handler)
}

func newUDP4BoundListener(interfaceName, laddr string) (pc net.PacketConn, e error) {
	addr, err := net.ResolveUDPAddr("udp4", laddr)
	if err != nil {
		return nil, err
	}

	s, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, syscall.IPPROTO_UDP)
	if err != nil {
		return nil, err
	}
	defer func() { // clean up if something goes wrong
		if e != nil {
			syscall.Close(s)
		}
	}()

	if err := syscall.SetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return nil, err
	}
	if err := syscall.SetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1); err != nil {
		return nil, err
	}
	if err := syscall.SetsockoptString(s, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, interfaceName); err != nil {
		return nil, err
	}

	lsa := syscall.SockaddrInet4{Port: addr.Port}
	copy(lsa.Addr[:], addr.IP.To4())

	if err := syscall.Bind(s, &lsa); err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(s), "")
	defer f.Close()
	return net.FilePacketConn(f)
}
