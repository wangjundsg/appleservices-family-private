package appleservices

import (
	"errors"
	"fmt"

	"github.com/Laky-64/appleservices/gsa"
)

type Store interface {
	LoadDevice() (*Device, error)
	SaveDevice(*Device) error
	LoadSession() (*Session, error)
	SaveSession(*Session) error
}

func Logout(store Store) error {
	if store == nil {
		return errors.New("appleservices: Logout requires a Store")
	}
	if err := store.SaveSession(nil); err != nil {
		return fmt.Errorf("appleservices: Logout: clear session: %w", err)
	}
	return nil
}

type Device struct {
	Identifier       []byte
	ProvisioningBlob []byte
	SerialNumber     string `json:",omitempty"`
}

type Session struct {
	AppleID             string
	DSID                string
	Cookies             []Cookie
	Tokens              map[string]string `json:",omitempty"`
	DeviceIdentityToken string            `json:",omitempty"`
}

type Cookie struct {
	URL   string
	Name  string
	Value string
}

type Credentials struct {
	AppleID  string
	Password string
}

// RestoredICloudSession is an account-bound, previously accepted iCloud
// delegate checkpoint. It lets a local worker resume without retaining the
// Apple Account password. Callers must load it from the same account directory
// as Session and Device.
type RestoredICloudSession struct {
	DSID          string
	ADSID         string
	MMEAuthToken  string
	CloudKitToken string
	ServiceData   map[string]any
}

type Option func(*options)

type options struct {
	anisette      gsa.AnisetteProvider
	newBackend    backendFactory
	fetchDelegate delegateFetcher
	servers       []string
	serialNumber  string
	pushToken     []byte
	storePassword string
}

// WithStorePassword supplies an in-memory password for separate Store authentication
// after ResumeLogin. It does not authenticate GSA or persist the password.
func WithStorePassword(password string) Option {
	return func(o *options) { o.storePassword = password }
}

// WithDeviceSerialNumber identifies this installation in account-device calls.
// Use a stable value for the lifetime of the installation.
func WithDeviceSerialNumber(serial string) Option {
	return func(o *options) { o.serialNumber = serial }
}

// WithPushToken binds authentication to a device token obtained from APS.
func WithPushToken(token []byte) Option {
	return func(o *options) { o.pushToken = append([]byte(nil), token...) }
}

func WithAnisette(p gsa.AnisetteProvider) Option {
	return func(o *options) {
		o.anisette = p
	}
}

// WithAnisetteServers uses the supplied anisette servers instead of the public pool.
func WithAnisetteServers(urls ...string) Option {
	return func(o *options) {
		o.servers = append([]string{}, urls...)
	}
}
