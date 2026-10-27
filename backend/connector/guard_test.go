package connector

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestGuard(t *testing.T) {
	// Customer hosts are dialed from inside our VPC; none of these may be reachable through a connector.
	for _, addr := range []string{
		"127.0.0.1", "10.0.0.5", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.100.100.200",
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "::1", "fd00:ec2::254", "fe80::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1",
	} {
		t.Run("refuses "+addr, func(t *testing.T) {
			if _, err := (Guard{}).Resolve(context.Background(), addr); !errors.Is(err, ErrPrivateAddress) {
				t.Fatalf("Resolve(%s) error = %v, want ErrPrivateAddress", addr, err)
			}
		})
	}
	t.Run("permits public addresses", func(t *testing.T) {
		for _, addr := range []string{"8.8.8.8", "34.200.1.1", "2606:4700::1111"} {
			if _, err := (Guard{}).Resolve(context.Background(), addr); err != nil {
				t.Errorf("Resolve(%s) = %v, want permitted", addr, err)
			}
		}
	})
	// A name is judged by what it resolves to, not by how it looks.
	t.Run("refuses a name that resolves inward", func(t *testing.T) {
		if _, err := (Guard{}).Resolve(context.Background(), "localhost"); !errors.Is(err, ErrPrivateAddress) {
			t.Fatalf("Resolve(localhost) error = %v, want ErrPrivateAddress", err)
		}
	})
	t.Run("refuses to dial an inward address", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if _, err := (Guard{}).DialContext(context.Background(), "tcp", listener.Addr().String()); !errors.Is(err, ErrPrivateAddress) {
			t.Fatalf("DialContext error = %v, want ErrPrivateAddress", err)
		}
	})
	// Local development reaches databases on the laptop.
	t.Run("allow private admits loopback", func(t *testing.T) {
		if _, err := (Guard{AllowPrivate: true}).Resolve(context.Background(), "127.0.0.1"); err != nil {
			t.Fatalf("Resolve = %v", err)
		}
	})
}
