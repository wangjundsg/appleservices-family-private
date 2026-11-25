package appleservices

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Laky-64/appleservices/gsa"
)

// ErrDeviceIdentityRefreshRequired means the durable iCloud/service session is
// still usable, but the Apple Account device-management identity was not saved
// by an older build (or is no longer available). A fresh account login can
// replenish it without discarding the accepted iCloud session.
var ErrDeviceIdentityRefreshRequired = errors.New("appleservices: device-management identity refresh required")

type identityAnisette struct {
	provider gsa.AnisetteProvider
	serial   string
}

func (p identityAnisette) Headers() (map[string]string, error) {
	headers, err := p.provider.Headers()
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(headers)+1)
	for key, value := range headers {
		if !strings.EqualFold(key, "X-Apple-I-SRL-NO") {
			result[key] = value
		}
	}
	result["X-Apple-I-SRL-NO"] = p.serial
	return result, nil
}

type AccountDevice = gsa.AccountDevice
type DeviceRegistration = gsa.DeviceRegistration
type CircleReply = gsa.CircleReply

// RegisterDevice announces this installation to the account. It does not prove
// trusted-device status: callers must verify account-side state and real delivery.
func (l *Login) RegisterDevice(registration DeviceRegistration) error {
	if l.client == nil || l.needsTwoFactor {
		return errors.New("appleservices: complete iCloud authentication before device registration")
	}
	return l.PrepareDeviceRegistration(registration)
}

// PrepareDeviceRegistration sends account-side GSA postdata before the iCloud
// delegate exchange. Acceptance of postdata is not iCloud activation.
func (l *Login) PrepareDeviceRegistration(registration DeviceRegistration) error {
	if l.needsTwoFactor || l.result == nil || l.adsid == "" {
		return errors.New("appleservices: complete account authentication before device registration")
	}
	backend, ok := l.backend.(interface {
		RegisterDevice(map[string]any, gsa.DeviceRegistration) error
	})
	if !ok {
		return errors.New("appleservices: backend does not support device registration")
	}
	return backend.RegisterDevice(l.result.SessionPayload, registration)
}

func (l *Login) AccountDevices() ([]AccountDevice, error) {
	return l.AccountDevicesContext(context.Background())
}

func (l *Login) AccountDevicesContext(ctx context.Context) ([]AccountDevice, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.client == nil || l.needsTwoFactor {
		return nil, errors.New("appleservices: complete iCloud authentication before reading device status")
	}
	if l.result == nil || l.result.SessionPayload == nil || spdString(l.result.SessionPayload, "GsIdmsToken") == "" {
		return nil, fmt.Errorf("%w: saved session predates device identity persistence", ErrDeviceIdentityRefreshRequired)
	}
	if backend, ok := l.backend.(interface {
		AccountDevicesContext(context.Context, map[string]any) ([]gsa.AccountDevice, error)
	}); ok {
		return backend.AccountDevicesContext(ctx, l.result.SessionPayload)
	}
	backend, ok := l.backend.(interface {
		AccountDevices(map[string]any) ([]gsa.AccountDevice, error)
	})
	if !ok {
		return nil, errors.New("appleservices: backend does not support device status")
	}
	return backend.AccountDevices(l.result.SessionPayload)
}

func (l *Login) RespondToSignIn(transaction string, allow bool) error {
	if l.client == nil || l.needsTwoFactor {
		return errors.New("appleservices: complete authentication before responding to a sign-in")
	}
	backend, ok := l.backend.(interface {
		RespondToSignIn(map[string]any, string, bool) error
	})
	if !ok {
		return errors.New("appleservices: backend cannot respond to sign-in notifications")
	}
	return backend.RespondToSignIn(l.result.SessionPayload, transaction, allow)
}

func (l *Login) RespondToCircle(reply CircleReply) error {
	if l.client == nil || l.needsTwoFactor {
		return errors.New("appleservices: authenticate before responding to Circle")
	}
	backend, ok := l.backend.(interface {
		RespondToCircle(map[string]any, gsa.CircleReply) error
	})
	if !ok {
		return errors.New("appleservices: backend cannot respond to Circle")
	}
	return backend.RespondToCircle(l.result.SessionPayload, reply)
}
