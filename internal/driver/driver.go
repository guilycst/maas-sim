package driver

import (
	"context"
	"net/netip"
)

type Network struct {
	Subnet  netip.Prefix
	Gateway netip.Addr
}

type Node struct {
	ID       string
	Hostname string
	UserData []byte
}

type Instance struct {
	IP  netip.Addr
	MAC string
}

type IPLease struct {
	ID       string
	Hostname string
	IP       netip.Addr
}

type MachineDriver interface {
	Network() Network
	Deploy(context.Context, Node) (Instance, error)
	Release(context.Context, string) error
	Power(context.Context, string, bool) (Instance, error)
	ReserveIP(context.Context, IPLease) (netip.Addr, error)
	ReleaseIP(context.Context, string) error
	Cleanup(context.Context) error
}
