// Package control provides authenticated loopback IPC for the local dashboard.
package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Laky-64/appleservices/icloud"
	"github.com/Laky-64/appleservices/internal/accountguard"
	"github.com/Laky-64/appleservices/internal/privatefs"
)

type Record struct {
	PID   int    `json:"pid"`
	URL   string `json:"url"`
	Token string `json:"token"`
}
type Device struct {
	Name         string `json:"name"`
	Model        string `json:"model"`
	Installation bool   `json:"installation"`
	Current      bool   `json:"current"`
	Trusted      *bool  `json:"trusted,omitempty"`
}
type Pending struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind,omitempty"`
	PairingID  string    `json:"pairing_id,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
}

const LocalCodeCapability = "local-code-generation"

type View struct {
	Status                 json.RawMessage `json:"status"`
	Devices                []Device        `json:"devices"`
	Pending                *Pending        `json:"pending"`
	CodeReady              bool            `json:"code_ready"`
	Capabilities           []string        `json:"capabilities,omitempty"`
	BridgeCredentialsReady bool            `json:"bridge_credentials_ready"`
}
type Action struct {
	Action    string `json:"action"`
	Code      string `json:"code,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Label     string `json:"label,omitempty"`
	Note      string `json:"note,omitempty"`
}
type Result struct {
	Subscription        *SubscriptionInfo               `json:"subscription,omitempty"`
	OK                  bool                            `json:"ok"`
	Message             string                          `json:"message,omitempty"`
	Code                string                          `json:"code,omitempty"`
	CodeKind            string                          `json:"code_kind,omitempty"`
	HideAt              time.Time                       `json:"hide_at,omitempty"`
	Storage             *icloud.StorageInfo             `json:"storage,omitempty"`
	StoragePlan         *icloud.StoragePlanInfo         `json:"storage_plan,omitempty"`
	Purchase            *icloud.PurchaseResult          `json:"purchase,omitempty"`
	Stage               string                          `json:"stage,omitempty"`
	HTTPStatus          int                             `json:"http_status,omitempty"`
	AppleCode           string                          `json:"apple_code,omitempty"`
	AppleHint           string                          `json:"apple_hint,omitempty"`
	StoreAuthentication *icloud.StoreAuthenticationInfo `json:"store_authentication,omitempty"`
	StoreForm           *icloud.StoreFormInfo           `json:"store_form,omitempty"`
	Probes              []icloud.StorageEndpointProbe   `json:"probes,omitempty"`
	Family              *FamilyInfo                     `json:"family,omitempty"`
	HideMyEmail         *icloud.HideMyEmailInfo         `json:"hide_my_email,omitempty"`
	HMECandidate        string                          `json:"hme_candidate,omitempty"`
	Activation          *ActivationInfo                 `json:"activation,omitempty"`
}

// SubscriptionInfo excludes Apple's private subscription identifier.
type SubscriptionInfo struct {
	Present   bool `json:"present"`
	AutoRenew bool `json:"auto_renew"`
	CN50GB    bool `json:"cn_50gb"`
}

type ActivationInfo struct {
	Required       bool   `json:"required"`
	Completed      bool   `json:"completed,omitempty"`
	Region         string `json:"region,omitempty"`
	ICloudVersion  string `json:"icloud_version,omitempty"`
	MediaVersion   string `json:"media_version,omitempty"`
	ICloudTermsURL string `json:"icloud_terms_url,omitempty"`
	MediaTermsURL  string `json:"media_terms_url,omitempty"`
}

// FamilyInfo is the redacted, UI-safe state returned by the native FamilyKit
// helper. OrganizerAccount and OrganizerName are local dashboard labels, never
// Apple identifiers. The value intentionally contains no Apple IDs, DSIDs,
// family IDs, invite codes, tokens, cookies or raw Apple responses.
type FamilyInfo struct {
	Member                 bool   `json:"member"`
	Eligible               bool   `json:"eligible"`
	EligibilityKnown       bool   `json:"eligibility_known"`
	Members                int    `json:"members"`
	Organizers             int    `json:"organizers"`
	Self                   int    `json:"self"`
	Children               int    `json:"children"`
	Invitations            int    `json:"invitations"`
	Pending                int    `json:"pending"`
	Organizer              bool   `json:"organizer"`
	OrganizerAccount       string `json:"organizer_account,omitempty"`
	OrganizerName          string `json:"organizer_name,omitempty"`
	StorageKnown           bool   `json:"storage_known"`
	StorageParticipating   bool   `json:"storage_participating"`
	StorageAvailable       bool   `json:"storage_available"`
	StorageEnabled         bool   `json:"storage_enabled"`
	StorageBytes           int64  `json:"storage_bytes,omitempty"`
	PurchaseSharingKnown   bool   `json:"purchase_sharing_known"`
	PurchaseSharingEnabled bool   `json:"purchase_sharing_enabled"`
	LocationSharingKnown   bool   `json:"location_sharing_known"`
	LocationSharingEnabled bool   `json:"location_sharing_enabled"`
}
type Command struct {
	Action
	Context context.Context
	Reply   chan Result
}
type Server struct {
	Record            Record
	Commands          chan Command
	dir               string
	listener          net.Listener
	http              *http.Server
	mu                sync.Mutex
	devices           []Device
	pending           *Pending
	ready             bool
	bridgeCredentials BridgeCredentials
}

func New(dir string) (*Server, error) {
	if err := privatefs.EnsureDir(dir); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("local control listener unavailable")
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		listener.Close()
		return nil, err
	}
	s := &Server{dir: dir, listener: listener, Commands: make(chan Command), Record: Record{PID: os.Getpid(), URL: "http://" + listener.Addr().String(), Token: hex.EncodeToString(key)}}
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 4 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	if err := os.MkdirAll(dir, 0700); err != nil {
		listener.Close()
		return nil, err
	}
	data, _ := json.Marshal(s.Record)
	if err := os.WriteFile(filepath.Join(dir, "control.json"), data, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	go s.http.Serve(listener)
	return s, nil
}
func (s *Server) Close() {
	s.http.Close()
	path := filepath.Join(s.dir, "control.json")
	var record Record
	data, _ := os.ReadFile(path)
	if json.Unmarshal(data, &record) == nil && record.PID == s.Record.PID && record.Token == s.Record.Token {
		os.Remove(path)
	}
}
func (s *Server) SetDevices(devices []Device) { s.mu.Lock(); s.devices = devices; s.mu.Unlock() }
func (s *Server) SetPending(pending *Pending) { s.mu.Lock(); s.pending = pending; s.mu.Unlock() }
func (s *Server) SetReady(ready bool)         { s.mu.Lock(); s.ready = ready; s.mu.Unlock() }
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Host != s.listener.Addr().String() || r.Header.Get("Origin") != "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Control-Token")), []byte(s.Record.Token)) != 1 {
		http.Error(w, `{"message":"local control authorization failed"}`, http.StatusForbidden)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/view" {
		data, err := os.ReadFile(filepath.Join(s.dir, "runtime.json"))
		var status struct {
			Running           bool      `json:"running"`
			PID               int       `json:"pid"`
			State             string    `json:"state"`
			Authenticated     bool      `json:"authenticated"`
			LastDeviceQueryAt time.Time `json:"last_device_query_at"`
		}
		if err != nil || json.Unmarshal(data, &status) != nil || status.PID != s.Record.PID {
			http.Error(w, `{"message":"starting"}`, 503)
			return
		}
		s.mu.Lock()
		if !PendingFresh(s.pending, time.Now()) {
			s.pending = nil
		}
		allowed := accountguard.Check(s.dir) == nil
		view := View{Status: data, Devices: s.devices, Pending: s.pending, CodeReady: s.ready && allowed && status.Running, Capabilities: []string{"hide-my-email", "health-v2", "mail-bridge-credentials", "expiring-requests", LocalCodeCapability}, BridgeCredentialsReady: allowed && s.ready && s.bridgeCredentials.AppleID != "" && s.bridgeCredentials.Password != ""}
		if !allowed {
			view.Pending = nil
		}
		s.mu.Unlock()
		json.NewEncoder(w).Encode(view)
		return
	}
	if accountguard.Check(s.dir) != nil {
		http.Error(w, `{"message":"account protected"}`, http.StatusLocked)
		return
	}
	if r.Method == "POST" && r.URL.Path == "/bridge-credentials" {
		s.serveBridgeCredentials(w, r)
		return
	}
	if r.Method != "POST" || r.URL.Path != "/action" {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var action Action
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&action) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, `{"message":"invalid action"}`, 400)
		return
	}
	switch action.Action {
	case "storage-subscription", "storage-cancel-auto-renew", "storage-skill-purchase-cn", "family-refresh-auth", "code", "devices", "refresh", "validate-account", "allow", "deny", "submit-sms", "resend-sms", "submit-account-code", "storage-info", "storage-plan", "storage-authenticate", "storage-skill-auth-local", "storage-skill-auth-file", "storage-skill-purchase-us", "storage-preview", "storage-purchase", "storage-probe", "storage-buy", "family-info", "family-add-self", "family-join-storage", "hme-list", "hme-accept-terms", "hme-generate", "hme-reserve", "hme-forward", "hme-reachable-start", "hme-reachable-verify", "hme-update", "hme-deactivate", "hme-reactivate", "hme-delete", "submit-pairing":
	default:
		http.Error(w, `{"message":"unknown action"}`, 400)
		return
	}
	if action.Action == "code" {
		s.mu.Lock()
		ready := s.ready
		s.mu.Unlock()
		if !ready {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(Result{Stage: "code_provider_unavailable", Message: "本机验证码模块尚未就绪"})
			return
		}
	}
	if action.Action == "allow" || action.Action == "deny" {
		valid := s.RequestFresh(action.RequestID)
		if !valid {
			http.Error(w, `{"message":"request expired"}`, http.StatusConflict)
			return
		}
	}
	timeout := 90 * time.Second
	if action.Action == "storage-skill-auth-file" {
		timeout = 3 * time.Minute
	} else if action.Action == "family-add-self" || strings.HasPrefix(action.Action, "hme-") {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	command := Command{Action: action, Context: ctx, Reply: make(chan Result, 1)}
	select {
	case s.Commands <- command:
	case <-ctx.Done():
		http.Error(w, `{"message":"control busy"}`, 504)
		return
	}
	select {
	case result := <-command.Reply:
		json.NewEncoder(w).Encode(result)
	case <-ctx.Done():
		http.Error(w, `{"message":"control timeout"}`, 504)
	}
}
