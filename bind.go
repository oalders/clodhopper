package main

import (
	"fmt"
	"net"
)

// cgnatRange is the IPv4 carrier-grade NAT block (RFC 6598, 100.64.0.0/10).
// Tailscale assigns addresses here; it is NOT internet-routable, so we treat it
// as safe. net.IP.IsPrivate does not cover this range, so we check it explicitly.
var cgnatRange = mustCIDR("100.64.0.0/10")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// isPublicIP reports whether ip is a globally-routable unicast address that is
// not loopback, RFC1918/ULA private, link-local, unspecified, or CGNAT. Those
// ranges are safe to bind to (loopback / LAN / tailnet); anything else that is
// global unicast is treated as public. Pure function — easy to unit-test.
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	if cgnatRange.Contains(ip) {
		return false
	}
	return ip.IsGlobalUnicast()
}

// resolveBindIPs returns the concrete IPs that binding to host will listen on.
// For the unspecified address (0.0.0.0 / ::) it enumerates every interface
// address; for a literal IP it returns that IP; for a hostname it resolves it.
func resolveBindIPs(host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() {
			return interfaceIPs()
		}
		return []net.IP{ip}, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("resolve host %q: %w", host, err)
	}
	return ips, nil
}

func interfaceIPs() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("enumerate interface addresses: %w", err)
	}
	var ips []net.IP
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			ips = append(ips, ipnet.IP)
		}
	}
	return ips, nil
}

// guardPublicBind refuses to start when binding to host would expose the
// (unauthenticated, plaintext) dashboard on a public IP, unless allowPublic is
// set. Fail-closed: a resolution error is treated as unsafe.
func guardPublicBind(host string, allowPublic bool) error {
	if allowPublic {
		return nil
	}
	ips, err := resolveBindIPs(host)
	if err != nil {
		return fmt.Errorf("cannot verify bind address is safe: %w; "+
			"set CLODHOPPER_ALLOW_PUBLIC=1 or pass --allow-public to override", err)
	}
	var public []string
	for _, ip := range ips {
		if isPublicIP(ip) {
			public = append(public, ip.String())
		}
	}
	if len(public) > 0 {
		return fmt.Errorf("refusing to bind %q: resolves to public IP(s) %v — "+
			"the dashboard has no authentication or TLS; set CLODHOPPER_ALLOW_PUBLIC=1 "+
			"or pass --allow-public to override", host, public)
	}
	return nil
}
