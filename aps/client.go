package aps

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Laky-64/appleservices/gsa"
)

// State requires an Apple-issued client push certificate and its matching key.
// A developer APNs provider certificate is a different credential.
type State struct {
	CertificatePEM []byte
	PrivateKeyPEM  []byte
	Token          []byte
}

type Client struct {
	state       State
	key         *rsa.PrivateKey
	certificate *x509.Certificate
	connection  net.Conn
	writeMu     sync.Mutex
}

func NewClient(state State) (*Client, error) {
	if len(state.Token) != 0 && len(state.Token) != 32 {
		return nil, errors.New("aps: saved token must be 32 bytes")
	}
	pair, err := tls.X509KeyPair(state.CertificatePEM, state.PrivateKeyPEM)
	if err != nil {
		return nil, errors.New("aps: a matching client push certificate and private key are required")
	}
	key, ok := pair.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("aps: push authentication requires an RSA key")
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, errors.New("aps: invalid client push certificate")
	}
	now := time.Now()
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return nil, errors.New("aps: client push certificate is outside its validity period")
	}
	state.Token = append([]byte(nil), state.Token...)
	return &Client{state: state, key: key, certificate: certificate}, nil
}

func (c *Client) State() State {
	result := c.state
	result.Token = append([]byte(nil), result.Token...)
	return result
}

func (c *Client) Close() error {
	if c.connection != nil {
		return c.connection.Close()
	}
	return nil
}

func (c *Client) Connect(ctx context.Context) error {
	if c.connection != nil {
		return errors.New("aps: connection is already open")
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM([]byte(gsa.AppleRootCAPEM))
	secure, err := dialCourierTLS(ctx, &tls.Config{
		RootCAs: pool, ServerName: "courier.push.apple.com",
		NextProtos: []string{"apns-security-v3"}, MinVersion: tls.VersionTLS12,
	}, dialCourier)
	if err != nil {
		return errors.New("aps: courier TLS connection failed after bounded recovery")
	}
	c.connection = secure
	if err := c.authenticate(ctx); err != nil {
		c.Close()
		c.connection = nil
		return err
	}
	return nil
}

func (c *Client) send(f frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.connection == nil {
		return errors.New("aps: not connected")
	}
	if err := c.connection.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return err
	}
	return writeFrame(c.connection, f)
}

func (c *Client) authenticate(ctx context.Context) error {
	connection := c.connection
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	nonce := make([]byte, 17)
	binary.BigEndian.PutUint64(nonce[1:9], uint64(time.Now().UnixMilli()))
	if _, err := rand.Read(nonce[9:]); err != nil {
		return err
	}
	digest := sha1.Sum(nonce)
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA1, digest[:])
	if err != nil {
		return err
	}
	fields := []field{{2, []byte{1}}, {5, []byte{0, 0, 0, 65}}, {12, c.certificate.Raw}, {13, nonce}, {14, append([]byte{1, 1}, signature...)}}
	if len(c.state.Token) > 0 {
		fields = append(fields, field{1, c.state.Token})
	}
	if err := c.send(frame{7, fields}); err != nil {
		return errors.New("aps: connect request failed")
	}
	deadline := time.Now().Add(30 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	if err := c.connection.SetReadDeadline(deadline); err != nil {
		return err
	}
	response, err := readFrame(c.connection)
	if err != nil {
		return errors.New("aps: connect response failed")
	}
	if response.command != 8 {
		return errors.New("aps: expected connection acknowledgement")
	}
	status := response.get(1)
	if !bytes.Equal(status, []byte{0}) {
		return fmt.Errorf("aps: authentication rejected (status=%x)", status)
	}
	token := response.get(3)
	if len(token) == 0 {
		token = c.state.Token
	}
	if len(token) != 32 {
		return errors.New("aps: connect returned no valid device token")
	}
	c.state.Token = append([]byte(nil), token...)
	if err := c.send(frame{20, []field{{1, []byte{1}}, {2, []byte{0x7f, 0xff, 0xff, 0xff}}}}); err != nil {
		return err
	}
	topic := sha1.Sum([]byte(IDMSAuthTopic))
	if err := c.send(frame{9, []field{{1, c.state.Token}, {2, topic[:]}}}); err != nil {
		return err
	}
	return c.connection.SetReadDeadline(time.Time{})
}

// Listen acknowledges incoming pushes and delivers only AuthKit sign-in events.
// Raw pushes, account identifiers, and tokens are never written to logs.
func (c *Client) Listen(ctx context.Context, handle func(Event) error) error {
	if c.connection == nil || len(c.state.Token) != 32 {
		return errors.New("aps: authenticate before listening")
	}
	if handle == nil {
		return errors.New("aps: event handler is required")
	}
	connection := c.connection
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	keepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-keepCtx.Done():
				return
			case <-ticker.C:
				if err := c.send(frame{command: 12}); err != nil {
					c.Close()
					return
				}
			}
		}
	}()
	topic := sha1.Sum([]byte(IDMSAuthTopic))
	for {
		message, err := readFrame(c.connection)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("aps: push connection ended")
		}
		if message.command != 10 {
			continue
		}
		id := message.get(4)
		if len(id) != 4 {
			return errors.New("aps: notification missing message ID")
		}
		if err := c.send(frame{11, []field{{1, c.state.Token}, {4, id}, {8, []byte{0}}}}); err != nil {
			return err
		}
		if !bytes.Equal(message.get(2), topic[:]) {
			continue
		}
		token := message.get(1)
		if len(token) > 0 && !bytes.Equal(token, c.state.Token) {
			continue
		}
		event, err := ParseEvent(message.get(3))
		if event.Unsupported {
			if err := handle(event); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := handle(event); err != nil {
			return err
		}
	}
}
