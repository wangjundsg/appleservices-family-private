package aps

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"time"
)

// Retry only before push authentication; never resend a device-auth frame.
func dialCourierTLS(ctx context.Context, config *tls.Config, dial func(context.Context, string) (net.Conn, error)) (*tls.Conn, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * 250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, err := dial(attemptCtx, "1-courier.push.apple.com:5223")
		if err == nil {
			secure := tls.Client(conn, config)
			err = secure.HandshakeContext(attemptCtx)
			if err == nil {
				cancel()
				return secure, nil
			}
			conn.Close()
		}
		cancel()
		last = err
		var certificateError *tls.CertificateVerificationError
		if errors.As(err, &certificateError) {
			return nil, err
		}
		var networkError net.Error
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.As(err, &networkError) {
			return nil, err
		}
	}
	return nil, last
}
