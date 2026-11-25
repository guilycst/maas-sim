package driver

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"
)

type MemoryDriver struct {
	delay time.Duration
	mu    sync.Mutex
	ips   map[string]netip.Addr
}

func NewMemory(delay time.Duration) *MemoryDriver {
	return &MemoryDriver{delay: delay, ips: make(map[string]netip.Addr)}
}

func (*MemoryDriver) Network() Network {
	return Network{Subnet: netip.MustParsePrefix("192.0.2.0/24"), Gateway: netip.MustParseAddr("192.0.2.1")}
}

func (d *MemoryDriver) Deploy(ctx context.Context, node Node) (Instance, error) {
	timer := time.NewTimer(d.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return Instance{}, ctx.Err()
	case <-timer.C:
	}
	ip, err := d.claim("node:"+node.ID, netip.Addr{})
	return Instance{IP: ip}, err
}

func (d *MemoryDriver) Release(_ context.Context, id string) error {
	d.free("node:" + id)
	return nil
}

func (d *MemoryDriver) Power(_ context.Context, id string, _ bool) (Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Instance{IP: d.ips["node:"+id]}, nil
}

func (d *MemoryDriver) ReserveIP(_ context.Context, lease IPLease) (netip.Addr, error) {
	return d.claim("lease:"+lease.ID, lease.IP)
}

func (d *MemoryDriver) ReleaseIP(_ context.Context, id string) error {
	d.free("lease:" + id)
	return nil
}

func (d *MemoryDriver) Cleanup(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	clear(d.ips)
	return nil
}

func (d *MemoryDriver) free(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.ips, key)
}

func (d *MemoryDriver) claim(key string, requested netip.Addr) (netip.Addr, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if ip, ok := d.ips[key]; ok {
		if requested.IsValid() && ip != requested {
			return netip.Addr{}, errors.New("address already assigned")
		}
		return ip, nil
	}

	used := make(map[netip.Addr]bool, len(d.ips))
	for _, ip := range d.ips {
		used[ip] = true
	}
	if requested.IsValid() {
		if used[requested] {
			return netip.Addr{}, errors.New("address already in use")
		}
		d.ips[key] = requested
		return requested, nil
	}

	for host := byte(10); host < 255; host++ {
		ip := netip.AddrFrom4([4]byte{192, 0, 2, host})
		if !used[ip] {
			d.ips[key] = ip
			return ip, nil
		}
	}
	return netip.Addr{}, errors.New("address pool exhausted")
}
