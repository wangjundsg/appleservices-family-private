package aps

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/Laky-64/appleservices/internal/activationenv"
	"golang.org/x/net/proxy"
)

type bufferedConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConnection) Read(data []byte) (int, error) { return c.reader.Read(data) }

func dialCourier(ctx context.Context, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	endpoint := &url.URL{Scheme: "https", Host: address}
	proxyURL, err := http.ProxyFromEnvironment(&http.Request{URL: endpoint})
	if err != nil {
		return nil, err
	}
	if proxyURL == nil {
		return dialer.DialContext(ctx, activationenv.Network(), address)
	}
	if proxyURL.Scheme == "socks5" || proxyURL.Scheme == "socks5h" {
		d, err := proxy.FromURL(proxyURL, dialer)
		if err != nil {
			return nil, err
		}
		cd, ok := d.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("aps: proxy cannot dial with a context")
		}
		return cd.DialContext(ctx, "tcp", address)
	}
	if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
		return nil, errors.New("aps: unsupported proxy scheme")
	}
	host := proxyURL.Host
	if proxyURL.Port() == "" {
		if proxyURL.Scheme == "https" {
			host = net.JoinHostPort(proxyURL.Hostname(), "443")
		} else {
			host = net.JoinHostPort(proxyURL.Hostname(), "80")
		}
	}
	connection, err := dialer.DialContext(ctx, activationenv.Network(), host)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			connection.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	if proxyURL.Scheme == "https" {
		secured := tls.Client(connection, &tls.Config{ServerName: proxyURL.Hostname(), MinVersion: tls.VersionTLS12})
		if err := secured.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		connection = secured
	}
	deadline := time.Now().Add(20 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	connection.SetDeadline(deadline)
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	if proxyURL.User != nil {
		pass, _ := proxyURL.User.Password()
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username()+":"+pass)))
	}
	if err := request.Write(connection); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("aps: proxy rejected courier tunnel")
	}
	connection.SetDeadline(time.Time{})
	success = true
	return &bufferedConnection{connection, reader}, nil
}
