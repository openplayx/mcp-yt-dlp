package netguard

import (
	"context"
	"errors"
	"testing"
)

func TestAddressOK(t *testing.T) {
	for _, a := range []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946", "8.8.8.8"} {
		if !AddressOK(a) {
			t.Errorf("%s should be public", a)
		}
	}
	for _, a := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.211", "169.254.169.254", "::1", "fc00::1",
		"::ffff:127.0.0.1", "0.0.0.0", "100.64.0.1", "192.0.2.1", "224.0.0.1", "fe80::1%eth0", "2001:db8::1", "bogus"} {
		if AddressOK(a) {
			t.Errorf("%s should be refused", a)
		}
	}
}

func TestCheckURL(t *testing.T) {
	ctx := context.Background()
	pub := func(context.Context, string, int) ([]string, error) { return []string{"93.184.216.34"}, nil }
	mixed := func(context.Context, string, int) ([]string, error) {
		return []string{"93.184.216.34", "10.0.0.1"}, nil
	}
	fail := func(context.Context, string, int) ([]string, error) { return nil, errors.New("nxdomain") }
	if u, err := CheckURL(ctx, "  https://www.youtube.com/watch?v=x ", false, pub); err != nil || u != "https://www.youtube.com/watch?v=x" {
		t.Fatal(u, err)
	}
	if _, err := CheckURL(ctx, "https://a.example/", false, mixed); err == nil {
		t.Fatal("one private address is enough to refuse")
	}
	if _, err := CheckURL(ctx, "https://a.example/", false, fail); err == nil {
		t.Fatal("unresolvable")
	}
	if _, err := CheckURL(ctx, "http://printer.lan/", false, pub); err == nil {
		t.Fatal(".lan")
	}
	if _, err := CheckURL(ctx, "http://192.168.1.5/v", true, pub); err != nil {
		t.Fatal("private allowed for LAN tests")
	}
	if _, err := CheckURL(ctx, "https://a.example:99999/", false, pub); err == nil {
		t.Fatal("bad port")
	}
}
