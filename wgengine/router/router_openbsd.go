// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package router

import (
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os/exec"

	"github.com/tailscale/wireguard-go/tun"
	"go4.org/netipx"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/monitor"
)

// For now this router only supports the WireGuard userspace implementation.
// There is an experimental kernel version in the works for OpenBSD:
// https://git.zx2c4.com/wireguard-openbsd.

type openbsdRouter struct {
	logf    logger.Logf
	linkMon *monitor.Mon
	tunname string
	local4  netip.Prefix
	local6  netip.Prefix
	routes  map[netip.Prefix]struct{}
}

func newUserspaceRouter(logf logger.Logf, tundev tun.Device, linkMon *monitor.Mon) (Router, error) {
	tunname, err := tundev.Name()
	if err != nil {
		return nil, err
	}

	return &openbsdRouter{
		logf:    logf,
		linkMon: linkMon,
		tunname: tunname,
	}, nil
}

func cmd(args ...string) *exec.Cmd {
	if len(args) == 0 {
		log.Fatalf("exec.Cmd(%#v) invalid; need argv[0]", args)
	}
	return exec.Command(args[0], args[1:]...)
}

func (r *openbsdRouter) Up() error {
	ifup := []string{"ifconfig", r.tunname, "up"}
	if out, err := cmd(ifup...).CombinedOutput(); err != nil {
		r.logf("running ifconfig failed: %v\n%s", err, out)
		return err
	}
	return nil
}

func inet(p netip.Prefix) string {
	if p.Addr().Is6() {
		return "inet6"
	}
	return "inet"
}

func (r *openbsdRouter) Set(cfg *Config) error {
	if cfg == nil {
		cfg = &shutdownConfig
	}

	// TODO: support configuring multiple local addrs on interface.
	if len(cfg.LocalAddrs) == 0 {
		return nil
	}
	numIPv4 := 0
	numIPv6 := 0
	localAddr4 := netip.Prefix{}
	localAddr6 := netip.Prefix{}
	for _, addr := range cfg.LocalAddrs {
		if addr.Addr().Is4() {
			numIPv4++
			localAddr4 = addr
		}
		if addr.Addr().Is6() {
			numIPv6++
			localAddr6 = addr
		}
	}
	if numIPv4 > 1 || numIPv6 > 1 {
		return errors.New("openbsd doesn't support setting multiple local addrs yet")
	}

	var errq error

	if localAddr4 != r.local4 {
		if r.local4.IsValid() {
			addrdel := []string{"ifconfig", r.tunname,
				"inet", r.local4.String(), "-alias"}
			out, err := cmd(addrdel...).CombinedOutput()
			if err != nil {
				r.logf("addr del failed: %v: %v\n%s", addrdel, err, out)
				if errq == nil {
					errq = err
				}
			}

			routedel := []string{"route", "-q", "-n",
				"del", "-inet", r.local4.String(),
				"-iface", r.local4.Addr().String()}
			if out, err := cmd(routedel...).CombinedOutput(); err != nil {
				r.logf("route del failed: %v: %v\n%s", routedel, err, out)
				if errq == nil {
					errq = err
				}
			}
		}

		if localAddr4.IsValid() {
			addradd := []string{"ifconfig", r.tunname,
				"inet", localAddr4.String(), "alias"}
			out, err := cmd(addradd...).CombinedOutput()
			if err != nil {
				r.logf("addr add failed: %v: %v\n%s", addradd, err, out)
				if errq == nil {
					errq = err
				}
			}

			routeadd := []string{"route", "-q", "-n",
				"add", "-inet", localAddr4.String(),
				"-iface", localAddr4.Addr().String()}
			if out, err := cmd(routeadd...).CombinedOutput(); err != nil {
				r.logf("route add failed: %v: %v\n%s", routeadd, err, out)
				if errq == nil {
					errq = err
				}
			}
		}
	}

	if localAddr6.IsValid() {
		// in https://github.com/tailscale/tailscale/issues/1307 we made
		// FreeBSD use a /48 for IPv6 addresses, which is nice because we
		// don't need to additionally add routing entries. Do that here too.
		localAddr6 = netip.PrefixFrom(localAddr6.Addr(), 48)
	}

	if localAddr6 != r.local6 {
		if r.local6.IsValid() {
			addrdel := []string{"ifconfig", r.tunname,
				"inet6", r.local6.String(), "delete"}
			out, err := cmd(addrdel...).CombinedOutput()
			if err != nil {
				r.logf("addr del failed: %v: %v\n%s", addrdel, err, out)
				if errq == nil {
					errq = err
				}
			}
		}

		if localAddr6.IsValid() {
			addradd := []string{"ifconfig", r.tunname,
				"inet6", localAddr6.String()}
			out, err := cmd(addradd...).CombinedOutput()
			if err != nil {
				r.logf("addr add failed: %v: %v\n%s", addradd, err, out)
				if errq == nil {
					errq = err
				}
			}
		}
	}

	newRoutes := make(map[netip.Prefix]struct{})
	for _, route := range cfg.Routes {
		newRoutes[route] = struct{}{}
	}
	for route := range r.routes {
		if _, keep := newRoutes[route]; !keep {
			net := netipx.PrefixIPNet(route)
			nip := net.IP.Mask(net.Mask)
			nstr := fmt.Sprintf("%v/%d", nip, route.Bits())
			dst := localAddr4.Addr().String()
			if route.Addr().Is6() {
				dst = localAddr6.Addr().String()
			}
			routedel := []string{"route", "-q", "-n",
				"del", "-" + inet(route), nstr,
				"-iface", dst}
			out, err := cmd(routedel...).CombinedOutput()
			if err != nil {
				r.logf("route del failed: %v: %v\n%s", routedel, err, out)
				if errq == nil {
					errq = err
				}
			}
		}
	}
	for route := range newRoutes {
		if _, exists := r.routes[route]; !exists {
			net := netipx.PrefixIPNet(route)
			nip := net.IP.Mask(net.Mask)
			nstr := fmt.Sprintf("%v/%d", nip, route.Bits())
			dst := localAddr4.Addr().String()
			if route.Addr().Is6() {
				dst = localAddr6.Addr().String()
			}
			routeadd := []string{"route", "-q", "-n",
				"add", "-" + inet(route), nstr,
				"-iface", dst}
			out, err := cmd(routeadd...).CombinedOutput()
			if err != nil {
				r.logf("addr add failed: %v: %v\n%s", routeadd, err, out)
				if errq == nil {
					errq = err
				}
			}
		}
	}

	r.local4 = localAddr4
	r.local6 = localAddr6
	r.routes = newRoutes

	return errq
}

func (r *openbsdRouter) Close() error {
	cleanup(r.logf, r.tunname)
	return nil
}

func cleanup(logf logger.Logf, interfaceName string) {
	out, err := cmd("ifconfig", interfaceName, "down").CombinedOutput()
	if err != nil {
		logf("ifconfig down: %v\n%s", err, out)
	}
}
