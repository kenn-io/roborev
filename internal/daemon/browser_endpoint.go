package daemon

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

type BrowserEndpoint struct {
	Listener          net.Listener
	Address           string
	DialAddress       string
	Origin            string
	Enabled           bool
	remoteAuthEnabled bool
}

func ResolveBrowserEndpoint(web config.WebConfig) (BrowserEndpoint, error) {
	if !web.Enabled {
		return BrowserEndpoint{}, nil
	}
	listener, err := net.Listen("tcp", web.Listen)
	if err != nil {
		return BrowserEndpoint{}, fmt.Errorf("listen for browser traffic: %w", err)
	}
	address := listener.Addr().String()
	dial := dialAddress(listener.Addr())
	origin := web.PublicOrigin
	if origin == "" {
		origin = "http://" + address
	}
	return BrowserEndpoint{
		Listener:          listener,
		Address:           address,
		DialAddress:       dial.Host,
		Origin:            origin,
		Enabled:           true,
		remoteAuthEnabled: web.AuthToken != "",
	}, nil
}

func dialAddress(address net.Addr) *url.URL {
	host, port, err := net.SplitHostPort(address.String())
	if err != nil {
		return &url.URL{}
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}
	return &url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}
}
