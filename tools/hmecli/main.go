// hmecli drives one account's Hide My Email client. When an existing iCloud
// web session export is supplied it is adopted through
// icloud.HideMyEmailClient.UseWebSession, so listing, generating, reserving and
// deleting aliases need neither the account password nor a trusted-device code.
//
// Output is limited to counts, names, states and error codes: cookie and token
// values never reach stdout, stderr, or a log.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/icloud"
)

// webSessionFile is the private browser export produced by the protocol login.
// Only counts and the account binding are read here; cookie values stay inside
// the client.
type webSessionFile struct {
	AppleID        string                    `json:"apple_id"`
	DSID           string                    `json:"dsid"`
	AccountCountry string                    `json:"account_country"`
	Cookies        []icloud.WebSessionCookie `json:"cookies"`
	LoginStates    []struct {
		Cookies []icloud.WebSessionCookie `json:"cookies"`
	} `json:"login_states"`
}

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprintln(out, "usage: hmecli -dir ACCOUNT_DIR [-session ICLOUD_WEB_SESSION_JSON] -action list|generate|reserve|delete [-email ADDRESS] [-label LABEL]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "adopts an existing iCloud web session (no password, no 2FA) and drives Hide My Email")
	fmt.Fprintln(out, "output never contains cookie or token values")
	fmt.Fprintln(out, "")
	flag.PrintDefaults()
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "hmecli: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dir := flag.String("dir", "", "account directory, for example state/accounts/us-01")
	sessionFile := flag.String("session", "", "existing icloud-web-session.json to adopt instead of a password login")
	action := flag.String("action", "", "list | generate | reserve | delete")
	email := flag.String("email", "", "alias address for delete, or the candidate address for reserve")
	label := flag.String("label", "", "label for reserve")
	flag.Usage = usage
	flag.Parse()

	if strings.TrimSpace(*dir) == "" {
		return errors.New("-dir is required")
	}
	switch *action {
	case "", "list", "generate", "reserve", "delete":
	default:
		return errors.New("-action must be list, generate, reserve or delete")
	}
	if *action == "" && strings.TrimSpace(*sessionFile) == "" {
		return errors.New("-action is required when no -session is given")
	}

	appleID, dsid, cookies, err := accountBinding(*dir, *sessionFile)
	if err != nil {
		return err
	}
	client, err := icloud.NewHideMyEmailClient(*dir, appleID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 115*time.Second)
	defer cancel()

	if strings.TrimSpace(*sessionFile) != "" {
		web := icloud.WebSession{AppleID: appleID, DSID: dsid, Cookies: cookies}
		if err := client.UseWebSession(ctx, web); err != nil {
			return fmt.Errorf("web session adoption failed: %w", err)
		}
		fmt.Printf("session=adopted cookies=%d\n", len(cookies))
	}
	if !client.SessionActive() {
		return errors.New("no Hide My Email session is bound to this account; this tool never asks for a password")
	}
	if *action == "" {
		fmt.Println("session=ready")
		return nil
	}

	switch *action {
	case "list":
		info, listErr := client.List(ctx)
		if listErr != nil {
			return listErr
		}
		fmt.Printf("aliases=%d active=%d inactive=%d\n", len(info.Aliases), info.ActiveCount, info.InactiveCount)
		for _, alias := range info.Aliases {
			state := "inactive"
			if alias.Active {
				state = "active"
			}
			fmt.Printf("%s\t%s\t%s\n", alias.Email, alias.Label, state)
		}
	case "generate":
		candidate, generateErr := client.Generate(ctx, "zh-cn")
		if generateErr != nil {
			return generateErr
		}
		fmt.Printf("candidate=%s\n", candidate)
	case "reserve":
		labelValue := strings.TrimSpace(*label)
		if labelValue == "" {
			return errors.New("-label is required for reserve")
		}
		candidate := strings.TrimSpace(*email)
		if candidate == "" {
			generated, generateErr := client.Generate(ctx, "zh-cn")
			if generateErr != nil {
				return generateErr
			}
			candidate = generated
		}
		if reserveErr := client.Reserve(ctx, candidate, labelValue, ""); reserveErr != nil {
			return reserveErr
		}
		fmt.Printf("reserved=%s label=%s\n", candidate, labelValue)
	case "delete":
		address := strings.TrimSpace(*email)
		if address == "" {
			return errors.New("-email is required for delete")
		}
		if deactivateErr := client.SetActive(ctx, address, false); deactivateErr != nil {
			return deactivateErr
		}
		if deleteErr := client.Delete(ctx, address); deleteErr != nil {
			return deleteErr
		}
		fmt.Printf("deleted=%s\n", address)
	}
	return nil
}

// accountBinding resolves the Apple Account this run belongs to. With -session
// the browser export is authoritative; otherwise the saved Hide My Email
// session must already be bound to the directory.
func accountBinding(dir, sessionFile string) (string, string, []icloud.WebSessionCookie, error) {
	if strings.TrimSpace(sessionFile) != "" {
		data, readErr := os.ReadFile(sessionFile)
		if readErr != nil {
			return "", "", nil, errors.New("web session file is unreadable")
		}
		var export webSessionFile
		if json.Unmarshal(data, &export) != nil {
			return "", "", nil, errors.New("web session file is not valid JSON")
		}
		appleID := strings.TrimSpace(export.AppleID)
		if appleID == "" {
			return "", "", nil, errors.New("web session file has no account binding")
		}
		cookies := export.Cookies
		if len(cookies) == 0 && len(export.LoginStates) > 0 {
			cookies = export.LoginStates[0].Cookies
		}
		if len(cookies) == 0 {
			return "", "", nil, errors.New("web session file has no cookies")
		}
		return appleID, strings.TrimSpace(export.DSID), cookies, nil
	}
	data, readErr := os.ReadFile(filepath.Join(dir, "hme-session.json"))
	if readErr != nil {
		return "", "", nil, errors.New("account directory has no Hide My Email session; pass -session to adopt one")
	}
	var saved struct {
		AppleID string `json:"apple_id"`
	}
	if json.Unmarshal(data, &saved) != nil || strings.TrimSpace(saved.AppleID) == "" {
		return "", "", nil, errors.New("saved Hide My Email session has no account binding")
	}
	return strings.TrimSpace(saved.AppleID), "", nil, nil
}
