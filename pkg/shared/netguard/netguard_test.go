package netguard

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestCheckRefusesWhatAnAdministratorMustNotAimTheServerAt(t *testing.T) {
	for _, tc := range []struct {
		ip           string
		private, why string
	}{
		{"169.254.169.254", "", "the metadata service of AWS, Azure, Google and others"},
		{"169.254.1.1", "", "any link-local address"},
		{"fe80::1", "", "link-local IPv6"},
		{"fd00:ec2::254", "", "the IPv6 metadata service of AWS"},
		{"100.100.100.200", "", "the metadata service of Alibaba Cloud"},
		{"0.0.0.0", "", "the unspecified address"},
		{"0.1.2.3", "", "the \"this network\" block"},
		{"::", "", "the unspecified IPv6 address"},
		{"224.0.0.1", "", "multicast"},
		{"ff02::1", "", "multicast IPv6"},
		{"::ffff:169.254.169.254", "", "a metadata address written as IPv4-mapped IPv6"},
	} {
		for _, allow := range []bool{false, true} {
			if err := Check(netip.MustParseAddr(tc.ip), allow); !errors.Is(err, ErrBlocked) {
				t.Errorf("%s (%s) with private allowed=%v: %v, want it refused", tc.ip, tc.why, allow, err)
			}
		}
	}
}

func TestCheckRefusesPrivateNetworksUnlessAllowed(t *testing.T) {
	for _, ip := range []string{"10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.1.10", "fd12:3456::1", "100.64.0.1", "100.127.255.254", "::ffff:10.1.2.3"} {
		a := netip.MustParseAddr(ip)
		if err := Check(a, false); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s by default: %v, want it refused", ip, err)
		}
		if err := Check(a, true); err != nil {
			t.Errorf("%s with private addresses allowed: %v", ip, err)
		}
	}
}

func TestCheckAllowsPublicAndLoopbackAddresses(t *testing.T) {
	for _, ip := range []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946", "127.0.0.1", "::1", "172.15.0.1", "172.32.0.1", "100.63.255.255", "100.128.0.1", "::ffff:127.0.0.1"} {
		if err := Check(netip.MustParseAddr(ip), false); err != nil {
			t.Errorf("%s: %v, want it allowed", ip, err)
		}
	}
}

func TestControlJudgesTheAddressTheDialerResolvedAndAsksTheSettingEachTime(t *testing.T) {
	allow := false
	c := Control(func() bool { return allow })
	if err := c("tcp4", "10.0.0.5:443", nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("private, not allowed: %v", err)
	}
	allow = true
	if err := c("tcp4", "10.0.0.5:443", nil); err != nil {
		t.Fatalf("private, allowed a moment later: %v", err)
	}
	for _, bad := range []string{"no-port", "example.com:443", "[::1:443"} {
		if err := c("tcp", bad, nil); !errors.Is(err, ErrBlocked) {
			t.Errorf("%q: %v, want it refused", bad, err)
		}
	}
}

// A real dialer with the guard never reaches the network for a refused address: the check runs
// before the connection is made, so nothing is sent and no wait is spent.
func TestADialerWithTheGuardRefusesBeforeConnecting(t *testing.T) {
	d := &net.Dialer{Timeout: 5 * time.Second, Control: Control(func() bool { return false })}
	start := time.Now()
	for _, addr := range []string{"10.255.255.1:80", "169.254.169.254:80"} {
		_, err := d.DialContext(context.Background(), "tcp", addr)
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: %v, want ErrBlocked", addr, err)
		}
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the refusals took %v: the dialer waited on the network", time.Since(start))
	}
}
