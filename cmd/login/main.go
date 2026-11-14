package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Laky-64/appleservices"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if err == flag.ErrHelp {
			return
		}
		fmt.Fprintln(os.Stderr, "login: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string, input io.Reader, output, errOutput io.Writer) error {
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	flags.SetOutput(errOutput)
	appleID := flags.String("appleid", "", "Apple ID (or APPLESERVICES_APPLEID)")
	password := flags.String("password", "", "Apple ID password (prefer APPLESERVICES_PASSWORD)")
	dir := flags.String("dir", envOrDefault("APPLESERVICES_DIR", "./state"), "directory for device identity and trusted-session cookies")
	factor := flags.String("twofactor", "trusted-device", "verification method: trusted-device or sms")
	server := flags.String("anisette", "", "custom anisette server URL (default: public pool)")
	logout := flags.Bool("logout", false, "clear local trusted-session cookies and exit")
	checkpoint := flags.Bool("checkpoint", false, "save an account-bound delegate checkpoint for device registration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if strings.TrimSpace(*dir) == "" {
		return fmt.Errorf("dir is required")
	}
	store := fileStore{dir: *dir}
	if *logout {
		if err := appleservices.Logout(store); err != nil {
			return err
		}
		fmt.Fprintln(output, "Local session cleared.")
		return nil
	}
	method := appleservices.TrustedDevice
	switch *factor {
	case "trusted-device":
	case "sms":
		method = appleservices.SMS
	default:
		return fmt.Errorf("twofactor must be trusted-device or sms")
	}
	if strings.TrimSpace(*appleID) == "" || *password == "" {
		if *appleID == "" {
			*appleID = os.Getenv("APPLESERVICES_APPLEID")
		}
		if *password == "" {
			*password = os.Getenv("APPLESERVICES_PASSWORD")
		}
	}
	if strings.TrimSpace(*appleID) == "" || *password == "" {
		return fmt.Errorf("Apple ID and password are required; use run-login.ps1 or APPLESERVICES_APPLEID/APPLESERVICES_PASSWORD")
	}
	var opts []appleservices.Option
	if *server != "" {
		opts = append(opts, appleservices.WithAnisetteServers(*server))
	}
	fmt.Fprintln(errOutput, "Authenticating Apple ID ...")
	login, err := appleservices.BeginLogin(appleservices.Credentials{AppleID: *appleID, Password: *password}, store, opts...)
	if err != nil {
		return err
	}
	if login.NeedsTwoFactor() {
		fmt.Fprintln(errOutput, "Two-factor authentication required.")
		if err := completeTwoFactor(login, method, input, errOutput); err != nil {
			return err
		}
	}
	fmt.Fprintln(errOutput, "Completing iCloud login ...")
	client, err := login.Client()
	if err != nil {
		return err
	}
	if *checkpoint {
		if err := store.saveAccepted(*appleID, client.ResumeCheckpoint()); err != nil {
			return err
		}
	}
	// Do not print account IDs, credentials, cookies, or tokens.
	fmt.Fprintln(output, "iCloud authentication successful. Session cookies saved.")
	fmt.Fprintln(output, "This command does not register a trusted device or receive verification codes.")
	return nil
}

type twoFactorLogin interface {
	RequestCode(appleservices.TwoFactorMethod) error
	SubmitCode(appleservices.TwoFactorMethod, string) error
}

func completeTwoFactor(login twoFactorLogin, method appleservices.TwoFactorMethod, input io.Reader, output io.Writer) error {
	request := func() error {
		if err := login.RequestCode(method); err != nil {
			return err
		}
		if method == appleservices.SMS {
			fmt.Fprintln(output, "SMS verification code requested.")
		} else {
			fmt.Fprintln(output, "Trusted-device verification code requested.")
		}
		return nil
	}
	if err := request(); err != nil {
		return err
	}
	reader := bufio.NewReader(input)
	for {
		fmt.Fprint(output, "Enter six-digit code, or resend / sms / device: ")
		line, err := reader.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			return fmt.Errorf("read verification code: %w", err)
		}
		switch strings.TrimSpace(line) {
		case "resend":
		case "sms":
			method = appleservices.SMS
		case "device":
			method = appleservices.TrustedDevice
		default:
			return login.SubmitCode(method, strings.TrimSpace(line))
		}
		if err := request(); err != nil {
			return err
		}
	}
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
