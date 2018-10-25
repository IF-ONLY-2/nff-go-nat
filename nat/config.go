// Copyright 2017-2018 Intel Corporation.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package nat

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/intel-go/nff-go/common"
	"github.com/intel-go/nff-go/flow"
	"github.com/intel-go/nff-go/packet"

	upd "github.com/intel-go/nff-go-nat/updatecfg"
)

type terminationDirection uint8
type interfaceType int

const (
	pri2pub terminationDirection = 0x0f
	pub2pri terminationDirection = 0xf0

	iPUBLIC  interfaceType = 0
	iPRIVATE interfaceType = 1

	dirDROP = uint(upd.TraceType_DUMP_DROP)
	dirSEND = uint(upd.TraceType_DUMP_TRANSLATE)
	dirKNI  = uint(upd.TraceType_DUMP_KNI)

	connectionTimeout time.Duration = 1 * time.Minute
	portReuseTimeout  time.Duration = 1 * time.Second
)

var (
	zeroIPv6Addr = [common.IPv6AddrLen]uint8{}
)

type hostPort struct {
	Addr4 uint32
	Addr6 [common.IPv6AddrLen]uint8
	Port  uint16
	ipv6  bool
}

type protocolId struct {
	id   uint8
	ipv6 bool
}

type forwardedPort struct {
	Port        uint16     `json:"port"`
	Destination hostPort   `json:"destination"`
	Protocol    protocolId `json:"protocol"`
}

var protocolIdLookup map[string]protocolId = map[string]protocolId{
	"TCP": protocolId{
		id:   common.TCPNumber,
		ipv6: false,
	},
	"UDP": protocolId{
		id:   common.UDPNumber,
		ipv6: false,
	},
	"TCP6": protocolId{
		id:   common.TCPNumber,
		ipv6: true,
	},
	"UDP6": protocolId{
		id:   common.UDPNumber,
		ipv6: true,
	},
}

func (out *protocolId) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	result, ok := protocolIdLookup[s]
	if !ok {
		return errors.New("Bad protocol name: " + s)
	}

	*out = result
	return nil
}

type ipv4Subnet struct {
	Addr            uint32
	Mask            uint32
	addressAcquired bool
	ds              dhcpState
}

func (fp *forwardedPort) String() string {
	return fmt.Sprintf("Port:%d, Destination IPv4: %v, Destination IPv6: %v, Protocol: %d",
		fp.Port,
		packet.IPv4ToString(fp.Destination.Addr4),
		packet.IPv6ToString(fp.Destination.Addr6),
		fp.Protocol)
}

func (subnet *ipv4Subnet) String() string {
	if subnet.addressAcquired {
		// Count most significant set bits
		mask := uint32(1) << 31
		i := 0
		for ; i <= 32; i++ {
			if subnet.Mask&mask == 0 {
				break
			}
			mask >>= 1
		}
		return packet.IPv4ToString(packet.SwapBytesUint32(subnet.Addr)) + "/" + strconv.Itoa(i)
	}
	return "DHCP address not acquired"
}

func (subnet *ipv4Subnet) checkAddrWithingSubnet(addr uint32) bool {
	return addr&subnet.Mask == subnet.Addr&subnet.Mask
}

type ipv6Subnet struct {
	Addr            [common.IPv6AddrLen]uint8
	multicastAddr   [common.IPv6AddrLen]uint8
	Mask            [common.IPv6AddrLen]uint8
	llAddr          [common.IPv6AddrLen]uint8
	llMulticastAddr [common.IPv6AddrLen]uint8
	addressAcquired bool
	ds              dhcpv6State
}

func (subnet *ipv6Subnet) String() string {
	if subnet.addressAcquired {
		// Count most significant set bits
		i := 0
		for ; i <= 128; i++ {
			mask := uint8(1) << uint(7-(i&7))
			if i == 128 || subnet.Mask[i>>3]&mask == 0 {
				break
			}
		}
		return packet.IPv6ToString(subnet.Addr) + "/" + strconv.Itoa(i)
	}
	return "DHCP address not acquired"
}

func (subnet *ipv6Subnet) andMask(addr [common.IPv6AddrLen]uint8) [common.IPv6AddrLen]uint8 {
	var result [common.IPv6AddrLen]uint8
	for i := range addr {
		result[i] = addr[i] & subnet.Mask[i]
	}
	return result
}

func (subnet *ipv6Subnet) checkAddrWithingSubnet(addr [common.IPv6AddrLen]uint8) bool {
	return subnet.andMask(addr) == subnet.andMask(subnet.Addr)
}

type macAddress [common.EtherAddrLen]uint8

type portMapEntry struct {
	lastused             time.Time
	finCount             uint8
	terminationDirection terminationDirection
	static               bool
}

// Type describing a network port
type ipPort struct {
	Index         uint16          `json:"index"`
	Subnet        ipv4Subnet      `json:"subnet"`
	Subnet6       ipv6Subnet      `json:"subnet6"`
	Vlan          uint16          `json:"vlan-tag"`
	KNIName       string          `json:"kni-name"`
	ForwardPorts  []forwardedPort `json:"forward-ports"`
	SrcMACAddress macAddress
	Type          interfaceType
	// Pointer to an opposite port in a pair
	opposite *ipPort
	// Map of allocated IP ports on public interface
	portmap  [][]portMapEntry
	portmap6 [][]portMapEntry
	// Main lookup table which contains entries for packets coming at this port
	translationTable []*sync.Map
	// ARP lookup table
	arpTable sync.Map
	// Debug dump stuff
	fdump    [dirKNI + 1]*os.File
	dumpsync [dirKNI + 1]sync.Mutex
}

// Config for one port pair.
type portPair struct {
	PrivatePort ipPort `json:"private-port"`
	PublicPort  ipPort `json:"public-port"`
	// Synchronization point for lookup table modifications
	mutex sync.Mutex
	// Port that was allocated last
	lastport int
}

// Config for NAT.
type Config struct {
	HostName  string     `json:"host-name"`
	PortPairs []portPair `json:"port-pairs"`
}

// Type used to pass handler index to translation functions.
type pairIndex struct {
	index int
}

var (
	// Natconfig is a config file.
	Natconfig *Config
	// CalculateChecksum is a flag whether checksums should be
	// calculated for modified packets.
	NoCalculateChecksum bool
	// HWTXChecksum is a flag whether checksums calculation should be
	// offloaded to HW.
	NoHWTXChecksum bool
	NeedKNI        bool
	NeedDHCP       bool

	// Debug variables
	DumpEnabled [dirKNI + 1]bool
)

func (pi pairIndex) Copy() interface{} {
	return pairIndex{
		index: pi.index,
	}
}

func (pi pairIndex) Delete() {
}

func convertIPv4(in []byte) (uint32, error) {
	if in == nil || len(in) > 4 {
		return 0, fmt.Errorf("Only IPv4 addresses are supported now while your address has %d bytes", len(in))
	}

	addr := (uint32(in[0]) << 24) | (uint32(in[1]) << 16) |
		(uint32(in[2]) << 8) | uint32(in[3])

	return addr, nil
}

// UnmarshalJSON parses ipv 4 subnet details.
func (out *ipv4Subnet) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	if s == "dhcp" {
		out.Addr = uint32(0)
		out.Mask = uint32(0)
		out.addressAcquired = false
		return nil
	}

	if ip, ipnet, err := net.ParseCIDR(s); err == nil {
		if out.Addr, err = convertIPv4(ip.To4()); err != nil {
			return err
		}
		if out.Mask, err = convertIPv4(ipnet.Mask); err != nil {
			return err
		}
		out.addressAcquired = true
		return nil
	}

	if ip := net.ParseIP(s); ip != nil {
		var err error
		if out.Addr, err = convertIPv4(ip.To4()); err != nil {
			return err
		}
		out.Mask = 0xffffffff
		out.addressAcquired = true
		return nil
	}
	return errors.New("Failed to parse address " + s)
}

// UnmarshalJSON parses ipv 4 subnet details.
func (out *ipv6Subnet) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	if s == "dhcp" {
		out.Addr = [common.IPv6AddrLen]uint8{}
		out.Mask = [common.IPv6AddrLen]uint8{}
		out.addressAcquired = false
		return nil
	}

	if ip, ipnet, err := net.ParseCIDR(s); err == nil {
		if ip.To16() == nil {
			return fmt.Errorf("Bad IPv6 address: %s", s)
		}
		copy(out.Addr[:], ip.To16())
		copy(out.Mask[:], ipnet.Mask)
		out.addressAcquired = true
		return nil
	}

	if ip := net.ParseIP(s); ip != nil {
		if ip.To16() == nil {
			return fmt.Errorf("Bad IPv6 address: %s", s)
		}
		copy(out.Addr[:], ip.To16())
		out.Mask = [common.IPv6AddrLen]uint8{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
		out.addressAcquired = true
		return nil
	}
	return errors.New("Failed to parse address " + s)
}

// UnmarshalJSON parses ipv4 host:port string. Port may be omitted and
// is set to zero in this case.
func (out *hostPort) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	hostStr, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return err
	}

	ipArray := net.ParseIP(hostStr)
	if ipArray == nil {
		return errors.New("Bad IP address specified: " + hostStr)
	}
	out.Addr4, err = convertIPv4(ipArray.To4())
	if err != nil {
		ipv6addr := ipArray.To16()
		if ipv6addr == nil {
			return err
		}
		copy(out.Addr6[:], ipv6addr)
		out.ipv6 = true
	}

	if portStr != "" {
		port, err := strconv.ParseInt(portStr, 10, 32)
		if err != nil {
			return err
		}
		out.Port = uint16(port)
	} else {
		out.Port = 0
	}

	return nil
}

// UnmarshalJSON parses MAC address.
func (out *macAddress) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	hw, err := net.ParseMAC(s)
	if err != nil {
		return err
	}

	copy(out[:], hw)
	return nil
}

// ReadConfig function reads and parses config file
func ReadConfig(fileName string) error {
	file, err := os.Open(fileName)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(file)

	err = decoder.Decode(&Natconfig)
	if err != nil {
		return err
	}

	for i := range Natconfig.PortPairs {
		pp := &Natconfig.PortPairs[i]

		pp.PrivatePort.Type = iPRIVATE
		pp.PublicPort.Type = iPUBLIC
		pp.PublicPort.opposite = &pp.PrivatePort
		pp.PrivatePort.opposite = &pp.PublicPort

		if pp.PrivatePort.Vlan == 0 && pp.PublicPort.Vlan != 0 {
			return errors.New("Private port with index " +
				strconv.Itoa(int(pp.PrivatePort.Index)) +
				" has zero vlan tag while public port with index " +
				strconv.Itoa(int(pp.PublicPort.Index)) +
				" has non-zero vlan tag. Transition between VLAN-enabled and VLAN-disabled networks is not supported yet.")
		} else if pp.PrivatePort.Vlan != 0 && pp.PublicPort.Vlan == 0 {
			return errors.New("Private port with index " +
				strconv.Itoa(int(pp.PrivatePort.Index)) +
				" has non-zero vlan tag while public port with index " +
				strconv.Itoa(int(pp.PublicPort.Index)) +
				" has zero vlan tag. Transition between VLAN-enabled and VLAN-disabled networks is not supported yet.")
		}

		port := &pp.PrivatePort
		for pi := 0; pi < 2; pi++ {
			if !port.Subnet.addressAcquired {
				if Natconfig.HostName == "" {
					return fmt.Errorf("DHCP option for port %d requires that you set host-name configuration option", port.Index)
				}
				NeedDHCP = true
			}

			if port.KNIName != "" {
				NeedKNI = true
			}

			for fpi := range port.ForwardPorts {
				fp := &port.ForwardPorts[fpi]
				err := port.checkPortForwarding(fp)
				if err != nil {
					return err
				}
			}
			port = &pp.PublicPort
		}
	}

	return nil
}

func (port *ipPort) checkPortForwarding(fp *forwardedPort) error {
	if fp.Destination.ipv6 != fp.Protocol.ipv6 {
		return fmt.Errorf("Port forwarding protocol should be TCP or UDP for IPv4 addresses and TCP6 or UDP6 for IPv6 addresses")
	}

	var isAddrZero bool
	if fp.Destination.ipv6 {
		isAddrZero = fp.Destination.Addr6 == zeroIPv6Addr
	} else {
		isAddrZero = fp.Destination.Addr4 == 0
	}

	if isAddrZero {
		if port.KNIName == "" {
			return errors.New("Port with index " +
				strconv.Itoa(int(port.Index)) +
				" should have \"kni-name\" setting if you want to forward packets to KNI address 0.0.0.0 or [::]")
		}
		if fp.Destination.Port != fp.Port {
			return errors.New("When address 0.0.0.0 or [::] is specified, it means that packets are forwarded to KNI interface. In this case destination port should be equal to forwarded port. You have different values: " +
				strconv.Itoa(int(fp.Port)) + " and " +
				strconv.Itoa(int(fp.Destination.Port)))
		}
		NeedKNI = true
	} else {
		if port.Type == iPRIVATE {
			return errors.New("Only KNI port forwarding is allowed on private port. All translated connections from private to public network can be initiated without any forwarding rules.")
		}

		if fp.Destination.ipv6 {
			if !port.opposite.Subnet6.checkAddrWithingSubnet(fp.Destination.Addr6) {
				return errors.New("Destination address " +
					packet.IPv6ToString(fp.Destination.Addr6) +
					" should be within subnet " +
					port.opposite.Subnet6.String())
			}
		} else {
			if !port.opposite.Subnet.checkAddrWithingSubnet(fp.Destination.Addr4) {
				return errors.New("Destination address " +
					packet.IPv4ToString(fp.Destination.Addr4) +
					" should be within subnet " +
					port.opposite.Subnet.String())
			}
		}

		if fp.Destination.Port == 0 {
			fp.Destination.Port = fp.Port
		}
	}
	return nil
}

// Reads MAC addresses for local interfaces into pair ports.
func (pp *portPair) initLocalMACs() {
	pp.PublicPort.SrcMACAddress = flow.GetPortMACAddress(pp.PublicPort.Index)
	pp.PrivatePort.SrcMACAddress = flow.GetPortMACAddress(pp.PrivatePort.Index)
}

func (port *ipPort) initIPv6LLAddresses() {
	packet.CalculateIPv6LinkLocalAddrForMAC(&port.Subnet6.llAddr, port.SrcMACAddress)
	println("Configured link local address", packet.IPv6ToString(port.Subnet6.llAddr), "for port", port.Index)
	packet.CalculateIPv6MulticastAddrForDstIP(&port.Subnet6.llMulticastAddr, port.Subnet6.llAddr)
	println("Configured link local multicast address", packet.IPv6ToString(port.Subnet6.llMulticastAddr), "for port", port.Index)
	if port.Subnet6.Addr != zeroIPv6Addr {
		packet.CalculateIPv6MulticastAddrForDstIP(&port.Subnet6.multicastAddr, port.Subnet6.Addr)
		println("Configured multicast address", packet.IPv6ToString(port.Subnet6.multicastAddr), "for port", port.Index)
	}
}

func (port *ipPort) allocatePublicPortPortMap() {
	port.portmap = make([][]portMapEntry, 256)
	port.portmap[common.ICMPNumber] = make([]portMapEntry, portEnd)
	port.portmap[common.TCPNumber] = make([]portMapEntry, portEnd)
	port.portmap[common.UDPNumber] = make([]portMapEntry, portEnd)
	port.portmap6 = make([][]portMapEntry, 256)
	port.portmap6[common.TCPNumber] = make([]portMapEntry, portEnd)
	port.portmap6[common.UDPNumber] = make([]portMapEntry, portEnd)
	port.portmap6[common.ICMPv6Number] = make([]portMapEntry, portEnd)
}

func (port *ipPort) allocateLookupMap() {
	port.translationTable = make([]*sync.Map, 256)
	for i := range port.translationTable {
		port.translationTable[i] = new(sync.Map)
	}
}

func (port *ipPort) initPortPortForwardingEntries() {
	// Initialize port forwarding rules on public interface
	for i := range port.ForwardPorts {
		port.enableStaticPortForward(&port.ForwardPorts[i])
	}
}

func (port *ipPort) enableStaticPortForward(fp *forwardedPort) {
	if fp.Protocol.ipv6 {
		keyEntry := Tuple6{
			addr: port.Subnet6.Addr,
			port: fp.Port,
		}
		valEntry := Tuple6{
			addr: fp.Destination.Addr6,
			port: fp.Destination.Port,
		}
		port.translationTable[fp.Protocol.id].Store(keyEntry, valEntry)
		if fp.Destination.Addr6 != zeroIPv6Addr {
			port.opposite.translationTable[fp.Protocol.id].Store(valEntry, keyEntry)
		}
		if port.Type == iPUBLIC {
			port.getPortmap(fp.Protocol.ipv6, fp.Protocol.id)[fp.Port] = portMapEntry{
				lastused:             time.Now(),
				finCount:             0,
				terminationDirection: 0,
				static:               true,
			}
		}
	} else {
		keyEntry := Tuple{
			addr: port.Subnet.Addr,
			port: fp.Port,
		}
		valEntry := Tuple{
			addr: fp.Destination.Addr4,
			port: fp.Destination.Port,
		}
		port.translationTable[fp.Protocol.id].Store(keyEntry, valEntry)
		if fp.Destination.Addr4 != 0 {
			port.opposite.translationTable[fp.Protocol.id].Store(valEntry, keyEntry)
		}
		if port.Type == iPUBLIC {
			port.getPortmap(fp.Protocol.ipv6, fp.Protocol.id)[fp.Port] = portMapEntry{
				lastused:             time.Now(),
				finCount:             0,
				terminationDirection: 0,
				static:               true,
			}
		}
	}
}

func (port *ipPort) getPortmap(ipv6 bool, protocol uint8) []portMapEntry {
	if ipv6 {
		return port.portmap6[protocol]
	} else {
		return port.portmap[protocol]
	}
}

// InitFlows initializes flow graph for all interface pairs.
func InitFlows() {
	for i := range Natconfig.PortPairs {
		pp := &Natconfig.PortPairs[i]

		// Init port pairs state
		pp.initLocalMACs()
		pp.PrivatePort.initIPv6LLAddresses()
		pp.PublicPort.initIPv6LLAddresses()
		pp.PrivatePort.allocateLookupMap()
		pp.PublicPort.allocateLookupMap()
		pp.PublicPort.allocatePublicPortPortMap()
		pp.lastport = portStart
		pp.PrivatePort.initPortPortForwardingEntries()
		pp.PublicPort.initPortPortForwardingEntries()

		// Handler context with handler index
		context := new(pairIndex)
		context.index = i

		var fromPubKNI, fromPrivKNI, toPub, toPriv *flow.Flow
		var pubKNI, privKNI *flow.Kni
		var outsPub = uint(2)
		var outsPriv = uint(2)

		// Initialize public to private flow
		publicToPrivate, err := flow.SetReceiver(pp.PublicPort.Index)
		flow.CheckFatal(err)
		if pp.PublicPort.KNIName != "" {
			outsPub = 3
		}
		pubTranslationOut, err := flow.SetSplitter(publicToPrivate, PublicToPrivateTranslation, outsPub, context)
		flow.CheckFatal(err)
		flow.CheckFatal(flow.SetStopper(pubTranslationOut[dirDROP]))

		// Initialize public KNI interface if requested
		if pp.PublicPort.KNIName != "" {
			pubKNI, err = flow.CreateKniDevice(pp.PublicPort.Index, pp.PublicPort.KNIName)
			flow.CheckFatal(err)
			flow.CheckFatal(flow.SetSenderKNI(pubTranslationOut[dirKNI], pubKNI))
			fromPubKNI = flow.SetReceiverKNI(pubKNI)
		}

		// Initialize private to public flow
		privateToPublic, err := flow.SetReceiver(pp.PrivatePort.Index)
		flow.CheckFatal(err)
		if pp.PrivatePort.KNIName != "" {
			outsPriv = 3
		}
		privTranslationOut, err := flow.SetSplitter(privateToPublic, PrivateToPublicTranslation, outsPriv, context)
		flow.CheckFatal(err)
		flow.CheckFatal(flow.SetStopper(privTranslationOut[dirDROP]))

		// Initialize private KNI interface if requested
		if pp.PrivatePort.KNIName != "" {
			privKNI, err = flow.CreateKniDevice(pp.PrivatePort.Index, pp.PrivatePort.KNIName)
			flow.CheckFatal(err)
			flow.CheckFatal(flow.SetSenderKNI(privTranslationOut[dirKNI], privKNI))
			fromPrivKNI = flow.SetReceiverKNI(privKNI)
		}

		// Merge traffic coming from public KNI with translated
		// traffic from private side
		if fromPubKNI != nil {
			toPub, err = flow.SetMerger(fromPubKNI, privTranslationOut[dirSEND])
			flow.CheckFatal(err)
		} else {
			toPub = privTranslationOut[dirSEND]
		}

		// Merge traffic coming from private KNI with translated
		// traffic from public side
		if fromPrivKNI != nil {
			toPriv, err = flow.SetMerger(fromPrivKNI, pubTranslationOut[dirSEND])
			flow.CheckFatal(err)
		} else {
			toPriv = pubTranslationOut[dirSEND]
		}

		// Set senders to output packets
		err = flow.SetSender(toPriv, pp.PrivatePort.Index)
		flow.CheckFatal(err)
		err = flow.SetSender(toPub, pp.PublicPort.Index)
		flow.CheckFatal(err)
	}
}

func CheckHWOffloading() bool {
	ports := []uint16{}

	for i := range Natconfig.PortPairs {
		pp := &Natconfig.PortPairs[i]
		ports = append(ports, pp.PublicPort.Index, pp.PrivatePort.Index)
	}

	return flow.CheckHWCapability(flow.HWTXChecksumCapability, ports)
}

func (c *Config) getPortAndPairByID(portId uint32) (*ipPort, *portPair) {
	for i := range c.PortPairs {
		pp := &c.PortPairs[i]
		if uint32(pp.PublicPort.Index) == portId {
			return &pp.PublicPort, pp
		}
		if uint32(pp.PrivatePort.Index) == portId {
			return &pp.PrivatePort, pp
		}
	}
	return nil, nil
}
