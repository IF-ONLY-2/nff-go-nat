// Copyright 2018 Intel Corporation.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package nat

import (
	"fmt"
	"net"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/intel-go/nff-go/common"
	"github.com/intel-go/nff-go/packet"

	upd "github.com/intel-go/nff-go-nat/updatecfg"
)

const (
	GRPCServerPort = ":60602"
)

type server struct{}

func StartGRPCServer() error {
	lis, err := net.Listen("tcp", GRPCServerPort)
	if err != nil {
		return err
	}
	s := grpc.NewServer()
	upd.RegisterUpdaterServer(s, &server{})
	// Register reflection service on gRPC server.
	reflection.Register(s)

	go func() {
		if err := s.Serve(lis); err != nil {
			common.LogWarning(common.Initialization, "Error while serving GRPC requests:", err)
		}
	}()
	return nil
}

func (s *server) ControlDump(ctx context.Context, in *upd.DumpControlRequest) (*upd.Reply, error) {
	enable := in.GetEnableTrace()
	dumpType := in.GetTraceType()
	if dumpType < upd.TraceType_DUMP_DROP || dumpType > upd.TraceType_DUMP_KNI {
		return nil, fmt.Errorf("Bad value of dump type: %d", dumpType)
	}
	DumpEnabled[dumpType] = enable

	return &upd.Reply{
		Msg: "Success",
	}, nil
}

func (s *server) ChangeInterfaceAddress(ctx context.Context, in *upd.InterfaceAddressChangeRequest) (*upd.Reply, error) {
	portId := in.GetInterfaceId()
	port, _ := Natconfig.getPortAndPairByID(portId)
	if port == nil {
		return nil, fmt.Errorf("Interface with ID %d not found", portId)
	}
	subnet4, subnet6, err := convertSubnet(in.GetPortSubnet())
	if err != nil {
		return nil, err
	}
	var str string
	if subnet4 != nil {
		port.Subnet.Addr = subnet4.Addr
		port.Subnet.Mask = subnet4.Mask
		port.Subnet.addressAcquired = true
		port.setLinkLocalIPv4KNIAddress(port.Subnet.Addr, port.Subnet.Mask, Natconfig.bringUpKniInterfaces)
		str = port.Subnet.String()
	}
	if subnet6 != nil {
		port.Subnet6.Addr = subnet6.Addr
		port.Subnet6.Mask = subnet6.Mask
		port.Subnet6.addressAcquired = true
		port.setLinkLocalIPv6KNIAddress(port.Subnet6.Addr, port.Subnet6.Mask, Natconfig.bringUpKniInterfaces)
		packet.CalculateIPv6MulticastAddrForDstIP(&port.Subnet6.multicastAddr, port.Subnet6.Addr)
		port.setLinkLocalIPv6KNIAddress(port.Subnet6.llAddr, SingleIPMask, Natconfig.bringUpKniInterfaces)
		str = port.Subnet6.String()
	}

	return &upd.Reply{
		Msg: fmt.Sprintf("Successfully set port %d subnet to %s", portId, str),
	}, nil
}

func (s *server) ChangePortForwarding(ctx context.Context, in *upd.PortForwardingChangeRequest) (*upd.Reply, error) {
	portId := in.GetInterfaceId()
	port, pp := Natconfig.getPortAndPairByID(portId)
	if port == nil {
		return nil, fmt.Errorf("Interface with ID %d not found", portId)
	}

	fp, err := convertForwardedPort(in.GetPort())
	if err != nil {
		return nil, err
	}
	err = port.checkPortForwarding(fp)
	if err != nil {
		return nil, err
	}

	pp.mutex.Lock()
	if port.Type == iPUBLIC {
		pp.deleteOldConnection(fp.Protocol.ipv6, fp.Protocol.id, int(fp.Port))
	} else {
		port.deletePortForwardingEntry(fp.Protocol.ipv6, fp.Protocol.id, int(fp.Port))
	}
	if in.GetEnableForwarding() {
		port.enableStaticPortForward(fp)
	}
	pp.mutex.Unlock()

	return &upd.Reply{
		Msg: "Success",
	}, nil
}
