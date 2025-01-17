// Copyright 2018 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package dhcp4d implements a DHCPv4 server.
package dhcp4d

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/krolaw/dhcp4"
	"github.com/mdlayher/packet"
)

type Lease struct {
	Num              int       `json:"num"` // relative to Handler.start
	Addr             net.IP    `json:"addr"`
	HardwareAddr     string    `json:"hardware_addr"`
	Hostname         string    `json:"hostname"`
	HostnameOverride string    `json:"hostname_override"`
	Expiry           time.Time `json:"expiry"`
	LastACK          time.Time `json:"last_ack"`
}

type StaticLease struct {
	Addr         net.IP
	HardwareAddr string
	Hostname     string
}

func (l *Lease) Expired(at time.Time) bool {
	return !l.Expiry.IsZero() && at.After(l.Expiry)
}

func (l *Lease) Active(at time.Time) bool {
	return !l.LastACK.IsZero() && at.Before(l.LastACK.Add(leasePeriod))
}

type Handler struct {
	serverIP    net.IP
	start       net.IP // first IP address to hand out
	leaseRange  int    // number of IP addresses to hand out
	LeasePeriod time.Duration
	options     dhcp4.Options
	rawConn     net.PacketConn
	iface       *net.Interface

	timeNow func() time.Time

	staticLeases    map[string]StaticLease
	reservedOffsets map[int]struct{}

	// Leases is called whenever a new lease is handed out
	Leases func([]*Lease, *Lease)

	leasesMu sync.Mutex
	leasesHW map[string]int // points into leasesIP
	leasesIP map[int]*Lease
}

func NewHandler(iface *net.Interface, serverIP, startIP net.IP, netMask net.IP, leaseRange int, leasePeriod time.Duration, dnsServers []string, staticLeases []StaticLease, opts ...Option) (*Handler, error) {
	var err error

	var options options
	for _, opt := range opts {
		opt.set(&options)
	}

	conn := options.conn
	if conn == nil {
		conn, err = packet.Listen(iface, packet.Raw, syscall.ETH_P_ALL, nil)
		if err != nil {
			return nil, err
		}
	}

	serverIP = serverIP.To4()
	netMask = netMask.To4()
	startIP = startIP.To4()

	var dnsServerIPs []byte
	for _, s := range dnsServers {
		dnsIP := net.ParseIP(s)
		if dnsIP == nil {
			return nil, fmt.Errorf("parse dns ip error invalid: %s", s)
		}
		dnsServerIPs = append(dnsServerIPs, dnsIP.To4()...)
	}

	reservedOffsets := make(map[int]struct{})

	staticLeaseMap := make(map[string]StaticLease)
	for _, sl := range staticLeases {
		staticLeaseMap[strings.ToLower(sl.HardwareAddr)] = sl

		i := dhcp4.IPRange(startIP, sl.Addr)
		reservedOffsets[i] = struct{}{}
	}

	slog.Info("new handler", "serverIP", serverIP, "netMask", netMask)

	h := Handler{
		rawConn:         conn,
		iface:           iface,
		leasesHW:        make(map[string]int),
		leasesIP:        make(map[int]*Lease),
		staticLeases:    staticLeaseMap,
		serverIP:        serverIP,
		start:           startIP,
		leaseRange:      leaseRange,
		LeasePeriod:     leasePeriod,
		reservedOffsets: reservedOffsets,
		options: dhcp4.Options{
			// dhcp4.OptionSubnetMask: []byte{255, 255, 255, 0},
			// XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX
			dhcp4.OptionSubnetMask:       []byte(netMask),
			dhcp4.OptionRouter:           []byte(serverIP),
			dhcp4.OptionDomainNameServer: dnsServerIPs,
			dhcp4.OptionServerIdentifier: []byte(serverIP),
		},
		timeNow: time.Now,
	}

	slog.Info("new handler", "h", h)

	return &h, nil
}

// Apple recommends a DHCP lease time of 1 hour in
// https://support.apple.com/de-ch/HT202068,
// so if 20 minutes ever causes any trouble,
// we should try increasing it to 1 hour.
const leasePeriod = 20 * time.Minute

// SetLeases overwrites the leases database with the specified leases, typically
// loaded from persistent storage. There is no locking, so SetLeases must be
// called before Serve.
func (h *Handler) SetLeases(leases []*Lease) {
	h.leasesMu.Lock()
	defer h.leasesMu.Unlock()
	h.leasesHW = make(map[string]int)
	h.leasesIP = make(map[int]*Lease)
	for _, l := range leases {
		if l.LastACK.IsZero() {
			l.LastACK = l.Expiry
		}
		h.leasesHW[l.HardwareAddr] = l.Num
		h.leasesIP[l.Num] = l
	}
}

func (h *Handler) callLeasesLocked(lease *Lease) {
	if h.Leases == nil {
		return
	}
	var leases []*Lease
	for _, l := range h.leasesIP {
		leases = append(leases, l)
	}
	h.Leases(leases, lease)
}

func (h *Handler) SetHostname(hwaddr, hostname string) error {
	h.leasesMu.Lock()
	defer h.leasesMu.Unlock()
	leaseNum := h.leasesHW[hwaddr]
	lease := h.leasesIP[leaseNum]
	if lease.HardwareAddr != hwaddr || lease.Expired(h.timeNow()) {
		return fmt.Errorf("hwaddr %v does not have a valid lease", hwaddr)
	}
	lease.Hostname = hostname
	lease.HostnameOverride = hostname
	h.callLeasesLocked(lease)
	return nil
}

func (h *Handler) findLease() int {
	h.leasesMu.Lock()
	defer h.leasesMu.Unlock()
	now := h.timeNow()

	if len(h.leasesIP) < h.leaseRange {
		// TODO: hash the hwaddr like dnsmasq
		i := rand.Intn(h.leaseRange)

		if _, reserved := h.reservedOffsets[i]; reserved {
		}

		if l, ok := h.leasesIP[i]; !ok || l.Expired(now) {
			if _, reserved := h.reservedOffsets[i]; !reserved {
				return i
			}
		}
		for i := 0; i < h.leaseRange; i++ {
			if l, ok := h.leasesIP[i]; !ok || l.Expired(now) {
				if _, reserved := h.reservedOffsets[i]; !reserved {
					return i
				}
			}
		}
	}
	return -1
}

func (h *Handler) canLease(reqIP net.IP, hwaddr string) int {
	if len(reqIP) != 4 || reqIP.Equal(net.IPv4zero) {
		return -1
	}

	leaseNum := dhcp4.IPRange(h.start, reqIP) - 1
	if leaseNum < 0 {
		return -1
	}

	h.leasesMu.Lock()
	defer h.leasesMu.Unlock()
	l, ok := h.leasesIP[leaseNum]
	if !ok {
		if leaseNum >= h.leaseRange {
			return -1
		}

		return leaseNum // lease available
	}

	if l.HardwareAddr == hwaddr {
		return leaseNum // lease already owned by requestor
	}

	if leaseNum >= h.leaseRange {
		return -1
	}

	if l.Expired(h.timeNow()) {
		return leaseNum // lease expired
	}

	return -1 // lease unavailable
}

// ServeDHCP is always called from the same goroutine, so no locking is required.
func (h *Handler) ServeDHCP(p dhcp4.Packet, msgType dhcp4.MessageType, options dhcp4.Options) dhcp4.Packet {
	slog.Info("got dhcp packet", "iface", h.iface.Name, "type", msgType)
	reply := h.serveDHCP(p, msgType, options)
	if reply == nil {
		slog.Info("no reply unsupported request", "iface", h.iface.Name, "type", msgType)
		return nil // unsupported request
	}
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}
	destMAC := p.CHAddr()
	destIP := reply.YIAddr()
	if p.Broadcast() {
		destMAC = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
		destIP = net.IPv4bcast
	}
	ethernet := &layers.Ethernet{
		DstMAC:       destMAC,
		SrcMAC:       h.iface.HardwareAddr,
		EthernetType: layers.EthernetTypeIPv4,
	}

	ip := &layers.IPv4{
		Version:  4,
		TTL:      255,
		SrcIP:    h.serverIP,
		DstIP:    destIP,
		Protocol: layers.IPProtocolUDP,
		Flags:    layers.IPv4DontFragment,
	}
	udp := &layers.UDP{
		SrcPort: 67,
		DstPort: 68,
	}
	udp.SetNetworkLayerForChecksum(ip)
	gopacket.SerializeLayers(buf, opts,
		ethernet,
		ip,
		udp,
		gopacket.Payload(reply))

	if _, err := h.rawConn.WriteTo(buf.Bytes(), &packet.Addr{HardwareAddr: destMAC}); err != nil {
		slog.Error("WriteTo err", "err", err)
	}

	return nil
}

func (h *Handler) leaseHW(hwAddr string) (*Lease, bool) {
	h.leasesMu.Lock()
	defer h.leasesMu.Unlock()
	num, ok := h.leasesHW[hwAddr]
	if !ok {
		return nil, false
	}
	l, ok := h.leasesIP[num]
	return l, ok && l.HardwareAddr == hwAddr
}

func (h *Handler) leasePeriodForDevice(hwAddr string) time.Duration {
	hwAddrPrefix, err := hex.DecodeString(strings.ReplaceAll(hwAddr, ":", ""))
	if err != nil {
		return h.LeasePeriod
	}
	if len(hwAddrPrefix) != 6 {
		// Invalid MAC address
		return h.LeasePeriod
	}
	hwAddrPrefix = hwAddrPrefix[:3]
	i := sort.Search(len(nintendoMacPrefixes), func(i int) bool {
		return bytes.Compare(nintendoMacPrefixes[i][:], hwAddrPrefix) >= 0
	})
	if i < len(nintendoMacPrefixes) && bytes.Equal(nintendoMacPrefixes[i][:], hwAddrPrefix) {
		return 1 * time.Hour
	}
	return h.LeasePeriod
}

// TODO: is ServeDHCP always run from the same goroutine, or do we need locking?
func (h *Handler) serveDHCP(p dhcp4.Packet, msgType dhcp4.MessageType, options dhcp4.Options) dhcp4.Packet {
	reqIP := net.IP(options[dhcp4.OptionRequestedIPAddress])
	if reqIP == nil {
		reqIP = net.IP(p.CIAddr())
	}
	hwAddr := p.CHAddr().String()

	switch msgType {
	case dhcp4.Discover:
		free := -1

		// offer static lease if configured
		if sl, found := h.staticLeases[strings.ToLower(hwAddr)]; found {
			free = h.canLease(sl.Addr, hwAddr)
		}

		// try to offer the requested IP, if any and available
		if free < 0 && !reqIP.To4().Equal(net.IPv4zero) {
			free = h.canLease(reqIP, hwAddr)
			// log.Printf("canLease(%v, %s) = %d", reqIP, hwAddr, free)
		}

		// offer previous lease for this HardwareAddr, if any
		if lease, ok := h.leaseHW(hwAddr); ok && !lease.Expired(h.timeNow()) {
			free = lease.Num
			// log.Printf("h.leasesHW[%s] = %d", hwAddr, free)
		}

		if free == -1 {
			free = h.findLease()
			// log.Printf("findLease = %d", free)
		}

		if free == -1 {
			slog.Error("cannot reply with DHCPOFFER: no more leases available")
			return nil // no free leases
		}

		slog.Info("dhcp discover", "hw", hwAddr, "name", options[dhcp4.OptionHostName], "ip", dhcp4.IPAdd(h.start, free))

		return dhcp4.ReplyPacket(p,
			dhcp4.Offer,
			h.serverIP,
			dhcp4.IPAdd(h.start, free),
			h.leasePeriodForDevice(hwAddr),
			h.options.SelectOrderOrAll(options[dhcp4.OptionParameterRequestList]))

	case dhcp4.Request:
		if server, ok := options[dhcp4.OptionServerIdentifier]; ok && !net.IP(server).Equal(h.serverIP) {
			return nil // message not for this dhcp server
		}
		leaseNum := h.canLease(reqIP, hwAddr)
		if leaseNum == -1 {
			return dhcp4.ReplyPacket(p, dhcp4.NAK, h.serverIP, nil, 0, nil)
		}

		lease := &Lease{
			Num:          leaseNum,
			Addr:         make([]byte, 4),
			HardwareAddr: hwAddr,
			Expiry:       h.timeNow().Add(h.leasePeriodForDevice(hwAddr)),
			Hostname:     string(options[dhcp4.OptionHostName]),
			LastACK:      h.timeNow(),
		}
		copy(lease.Addr, reqIP.To4())

		if l, ok := h.leaseHW(lease.HardwareAddr); ok {
			if l.Expiry.IsZero() {
				// Retain permanent lease properties
				lease.Expiry = time.Time{}
				lease.Hostname = l.Hostname
			}
			if l.HostnameOverride != "" {
				lease.Hostname = l.HostnameOverride
				lease.HostnameOverride = l.HostnameOverride
			}

			// Release any old leases for this client
			h.leasesMu.Lock()
			delete(h.leasesIP, l.Num)
			h.leasesMu.Unlock()
		}

		h.leasesMu.Lock()
		defer h.leasesMu.Unlock()
		h.leasesIP[leaseNum] = lease
		h.leasesHW[lease.HardwareAddr] = leaseNum
		h.callLeasesLocked(lease)

		slog.Info("dhcp reply", "hw", hwAddr, "name", options[dhcp4.OptionHostName], "ip", reqIP)

		return dhcp4.ReplyPacket(
			p,
			dhcp4.ACK,
			h.serverIP,
			reqIP,
			h.leasePeriodForDevice(hwAddr),
			h.options.SelectOrderOrAll(options[dhcp4.OptionParameterRequestList]))
	case dhcp4.Decline:
		if h.expireLease(hwAddr) {
			slog.Info("expired lease DHCPDECLINE", "hw", hwAddr)
		}
		// Decline does not expect an ACK response.
		return nil
	}
	return nil
}

// expireLease expires the lease for hwAddr and reports whether or not the
// lease was actually expired by this call.
func (h *Handler) expireLease(hwAddr string) bool {
	h.leasesMu.Lock()
	defer h.leasesMu.Unlock()

	// TODO: deduplicate with h.leaseHW which also acquires h.leasesMu.

	num, ok := h.leasesHW[hwAddr]
	if !ok {
		return false
	}
	l, ok := h.leasesIP[num]
	if !ok {
		return false
	}
	if l.HardwareAddr != hwAddr {
		return false
	}
	l.Expiry = time.Now()
	return true
}
