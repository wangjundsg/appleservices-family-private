package nativeadi

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Laky-64/appleservices/internal/backgroundexec"
	"github.com/Laky-64/appleservices/internal/privatefs"
	"github.com/google/uuid"
)

const LibrarySHA256 = "ca88e816dda449d7ba24d54a5ed709a2d392daeae28c25a61dcd2e7496e44cfe"

func bridgeEnvironment() []string {
	var result []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(key, "APPLESERVICES_APPLEID") && !strings.EqualFold(key, "APPLESERVICES_PASSWORD") {
			result = append(result, entry)
		}
	}
	return result
}

// CheckRuntime checks only local Python and compiler prerequisites.
func CheckRuntime(python string) error {
	if python == "" {
		python = "python"
	}
	if _, err := exec.LookPath("gcc"); err != nil {
		return errors.New("nativeadi: gcc/MinGW is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, "-c", "import struct,sys; sys.exit(0 if struct.calcsize('P') == 8 and sys.version_info >= (3,10) else 1)")
	backgroundexec.Configure(command)
	command.Env = bridgeEnvironment()
	if err := command.Run(); err != nil {
		return errors.New("nativeadi: 64-bit Python 3.10 or newer is required")
	}
	return nil
}

//go:embed helper.py
var helperSource []byte

//go:embed bridge.py
var bridgeSource []byte

//go:embed assets/transport_fence.c
var fenceSource []byte

type Config struct {
	LibraryPath string
	ProfileDir  string
	Python      string
	Identifier  []byte
	Serial      string
	PushToken   []byte
}

type reply struct {
	ID       uint64            `json:"id"`
	DeviceID string            `json:"device_id"`
	Ready    bool              `json:"ready"`
	Headers  map[string]string `json:"headers"`
	Code     string            `json:"code"`
	Error    string            `json:"error"`
	Function int               `json:"function"`
}

// Client obtains headers and user verification codes from the same locally
// provisioned machine. It never receives an account password or service token.
type Client struct {
	config   Config
	deviceID uuid.UUID
	mu       sync.Mutex
	sequence uint64
	command  *exec.Cmd
	input    io.WriteCloser
	replies  chan reply
	done     chan struct{}
	closed   bool
}

func ValidateLibrary(path string) error {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		return errors.New("nativeadi: Windows x64 is required")
	}
	if !filepath.IsAbs(path) {
		return errors.New("nativeadi: library path must be absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.New("nativeadi: verified OpenBubbles library is unavailable")
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, 64*1024*1024)); err != nil {
		return errors.New("nativeadi: library hash read failed")
	}
	if hex.EncodeToString(hash.Sum(nil)) != LibrarySHA256 {
		return errors.New("nativeadi: library does not match the signature-verified OpenBubbles 1.18.800.0 release")
	}
	return nil
}

func NewClient(config Config) (*Client, error) {
	if err := ValidateLibrary(config.LibraryPath); err != nil {
		return nil, err
	}
	identifier, err := uuid.FromBytes(config.Identifier)
	if err != nil || strings.TrimSpace(config.Serial) == "" || len(config.PushToken) != 32 {
		return nil, errors.New("nativeadi: complete device identity and genuine APS token are required")
	}
	profile, err := filepath.Abs(config.ProfileDir)
	if err != nil || config.ProfileDir == "" {
		return nil, errors.New("nativeadi: profile directory is required")
	}
	config.ProfileDir = profile
	if config.Python == "" {
		config.Python = "python"
	}
	python, err := exec.LookPath(config.Python)
	if err != nil {
		return nil, errors.New("nativeadi: 64-bit Python is unavailable")
	}
	config.Python = python
	return &Client{config: config, deviceID: identifier, replies: make(chan reply, 4), done: make(chan struct{})}, nil
}

func (c *Client) Start(ctx context.Context) error {
	if c.command != nil || c.closed {
		return errors.New("nativeadi: client cannot be started again")
	}
	helperEnv, err := Environment()
	if err != nil {
		return err
	}
	if err := privatefs.EnsureDir(c.config.ProfileDir); err != nil {
		return errors.New("nativeadi: bridge directory unavailable")
	}
	directory, err := os.MkdirTemp(c.config.ProfileDir, "bridge-")
	if err != nil {
		return errors.New("nativeadi: bridge directory unavailable")
	}
	started := false
	defer func() {
		if !started {
			os.RemoveAll(directory)
		}
	}()
	for name, source := range map[string][]byte{"helper.py": helperSource, "bridge.py": bridgeSource, "transport_fence.c": fenceSource} {
		if err := os.WriteFile(filepath.Join(directory, name), source, 0600); err != nil {
			return errors.New("nativeadi: could not save bridge source")
		}
	}
	fence := filepath.Join(directory, "transport_fence.dll")
	compiler, err := exec.LookPath("gcc")
	if err != nil {
		return errors.New("nativeadi: gcc/MinGW is required to build the local transport isolation shim")
	}
	build := exec.CommandContext(ctx, compiler, "-shared", "-O2", filepath.Join(directory, "transport_fence.c"), "-o", fence, "-lws2_32")
	backgroundexec.Configure(build)
	build.Env = bridgeEnvironment()
	if err := build.Run(); err != nil {
		return errors.New("nativeadi: transport isolation shim build failed")
	}
	command := exec.Command(c.config.Python, "-u", filepath.Join(directory, "helper.py"))
	backgroundexec.Configure(command)
	// The helper needs no account secrets, including through inherited env vars.
	command.Env = helperEnv
	input, err := command.StdinPipe()
	if err != nil {
		return errors.New("nativeadi: bridge input unavailable")
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		return errors.New("nativeadi: bridge output unavailable")
	}
	if err := command.Start(); err != nil {
		input.Close()
		output.Close()
		return errors.New("nativeadi: bridge could not start")
	}
	c.command, c.input = command, input
	started = true
	go func() {
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 65536)
		for scanner.Scan() {
			var message reply
			if json.Unmarshal(scanner.Bytes(), &message) != nil {
				break
			}
			select {
			case c.replies <- message:
			default:
				input.Close()
			}
		}
		input.Close()
		command.Wait()
		os.RemoveAll(directory)
		close(c.done)
	}()
	init := map[string]any{"library": c.config.LibraryPath, "fence": fence, "profile": c.config.ProfileDir,
		"identifier": c.config.Identifier, "serial": c.config.Serial, "push_token": c.config.PushToken,
		"mac_address": SyntheticMACAddress(c.config.Identifier)}
	if err := json.NewEncoder(input).Encode(init); err != nil {
		c.Close()
		return errors.New("nativeadi: bridge initialization failed")
	}
	message, err := c.receive(ctx, 0)
	if err != nil || !message.Ready {
		c.Close()
		if err == nil {
			err = errors.New("nativeadi: generation preflight did not complete")
		}
		return err
	}
	return nil
}

func (c *Client) receive(ctx context.Context, id uint64) (reply, error) {
	// The helper can send its final diagnostic and exit before we are scheduled.
	// Drain that reply before treating process exit as an unexplained failure.
	select {
	case message := <-c.replies:
		return c.validateReply(message, id)
	default:
	}
	select {
	case message := <-c.replies:
		return c.validateReply(message, id)
	case <-c.done:
		select {
		case message := <-c.replies:
			return c.validateReply(message, id)
		default:
		}
		return reply{}, errors.New("nativeadi: helper exited before completing the request")
	case <-ctx.Done():
		return reply{}, ctx.Err()
	}
}

func (c *Client) validateReply(message reply, id uint64) (reply, error) {
	if message.Error != "" {
		switch message.Error {
		case "native_connect_timeout", "native_tls_handshake_failed", "native_network_timeout", "native_connection_failed", "native_operation_failed",
			"native_bridge_timeout", "native_local_io_error", "native_local_value_error", "native_local_operation_failed":
			phase := map[int]string{51: "configuration", 150: "identity", 198: "profile-create", 166: "profile-load",
				18: "configuration-load", 17: "identity-load", 19: "push-load", 201: "transport-isolation",
				130: "provider", 103: "code/provisioning", 106: "headers"}[message.Function]
			detail := message.Error
			if phase != "" {
				detail += " (" + phase + ")"
			}
			return reply{}, errors.New("nativeadi: " + detail)
		default:
			return reply{}, errors.New("nativeadi: native generation or provisioning failed")
		}
	}
	identity, err := uuid.Parse(message.DeviceID)
	if err != nil || identity != c.deviceID || message.ID != id {
		return reply{}, errors.New("nativeadi: response used a different device or request")
	}
	return message, nil
}

func (c *Client) request(ctx context.Context, operation string) (reply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.command == nil || c.closed {
		return reply{}, errors.New("nativeadi: bridge is not running")
	}
	c.sequence++
	if err := json.NewEncoder(c.input).Encode(map[string]any{"id": c.sequence, "op": operation}); err != nil {
		return reply{}, errors.New("nativeadi: could not send native request")
	}
	message, err := c.receive(ctx, c.sequence)
	if err != nil {
		c.input.Close()
		c.command.Process.Kill()
		c.closed = true
	}
	return message, err
}

func (c *Client) Headers() (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	message, err := c.request(ctx, "headers")
	if err != nil {
		return nil, err
	}
	identity, err := uuid.Parse(message.Headers["X-Mme-Device-Id"])
	if err != nil || identity != c.deviceID {
		return nil, errors.New("nativeadi: authentication headers use a different device")
	}
	for _, key := range []string{"X-Apple-I-MD", "X-Apple-I-MD-M", "X-Apple-I-MD-RINFO", "X-Mme-Client-Info"} {
		if message.Headers[key] == "" {
			return nil, fmt.Errorf("nativeadi: authentication header %s is missing", key)
		}
	}
	// The ADI module reports the machine identifier it derived, but not the
	// lookup value that ties it to this device. Match the MDLU derivation used
	// by our remote anisette provider; this does not establish why a particular
	// delegate request was refused.
	if message.Headers["X-Apple-I-MD-LU"] == "" {
		if os.Getenv("APPLESERVICES_ADI_NO_MDLU") == "" {
			sum := sha256.Sum256(c.config.Identifier)
			message.Headers["X-Apple-I-MD-LU"] = hex.EncodeToString(sum[:])
		}
	}
	message.Headers["X-Apple-I-SRL-NO"] = c.config.Serial
	message.Headers["User-Agent"] = "akd/1.0 CFNetwork/1408.0.4 Darwin/22.5.0"
	// X-Apple-I-MD-RINFO is a routing/provisioning state. Known-good clients
	// send different values. An override is diagnostic only; changing this
	// number does not regenerate the associated provisioning credentials.
	if override := strings.TrimSpace(os.Getenv("APPLESERVICES_ADI_RINFO")); override != "" {
		message.Headers["X-Apple-I-MD-RINFO"] = override
	}
	return message.Headers, nil
}

func (c *Client) Code(ctx context.Context) (string, error) {
	message, err := c.request(ctx, "code")
	if err != nil {
		return "", err
	}
	if len(message.Code) != 6 || strings.IndexFunc(message.Code, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return "", errors.New("nativeadi: no valid six-digit output")
	}
	return message.Code, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	command, input := c.command, c.input
	c.mu.Unlock()
	if command == nil {
		return nil
	}
	input.Close()
	select {
	case <-c.done:
		return nil
	case <-time.After(5 * time.Second):
		command.Process.Kill()
		return errors.New("nativeadi: helper shutdown timed out")
	}
}
