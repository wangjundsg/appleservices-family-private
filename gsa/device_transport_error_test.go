package gsa

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestDeviceTransportClassificationIsSanitized(t *testing.T) {
	for _, tc := range []struct {
		err             error
		retry, canceled bool
	}{
		{context.DeadlineExceeded, true, false}, {context.Canceled, false, true},
		{io.ErrUnexpectedEOF, true, false}, {&net.DNSError{Err: "secret", IsTimeout: true}, true, false},
		{&net.DNSError{Err: "secret", IsNotFound: true}, false, false},
		{x509.UnknownAuthorityError{}, false, false}, {errors.New("secret"), false, false},
	} {
		err := deviceTransportFailure(tc.err)
		var failure *DeviceTransportError
		if !errors.As(err, &failure) || failure.Retryable != tc.retry || failure.Canceled != tc.canceled || strings.Contains(err.Error(), "secret") {
			t.Fatal("unsafe transport classification")
		}
	}
}

func TestDeviceQueryCancellationReachesTransport(t *testing.T) {
	c := NewClient(deviceTestAnisette{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	c.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet {
			t.Fatal("check was not read-only")
		}
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	_, err := c.AccountDevicesContext(ctx, map[string]any{"adsid": "fixture", "GsIdmsToken": "fixture"})
	var failure *DeviceTransportError
	if calls != 1 || !errors.As(err, &failure) || !failure.Canceled || failure.Retryable {
		t.Fatal("cancellation lost or retried")
	}
	_, _ = c.AccountDevicesContext(ctx, map[string]any{"adsid": "fixture", "GsIdmsToken": "fixture"})
	if calls != 1 {
		t.Fatal("canceled context issued another request")
	}
}

func TestWindowsDisconnectErrorsAreTemporary(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Winsock errno classification")
	}
	for _, code := range []syscall.Errno{64, 10051, 10053, 10054, 10060, 10061, 10065} {
		err := deviceTransportFailure(&net.OpError{Op: "read", Err: code})
		if !err.(*DeviceTransportError).Retryable {
			t.Fatalf("Windows error %d was not retryable", code)
		}
	}
}
