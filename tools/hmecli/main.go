// hmecli drives one account's Hide My Email client. When an existing iCloud
// web session export is supplied it is adopted through
// icloud.HideMyEmailClient.UseWebSession, so listing, generating, reserving and
// deleting aliases need neither the account password nor a trusted-device code.
//
// Every remote write is write-ahead logged to <dir>/hme-pending.json first:
//
//   - reserve records the candidate, label and operation id before Reserve, so
//     a re-entry after an interrupted call reserves the same candidate instead
//     of generating and burning a new one;
//   - delete records deactivate_done and delete_done separately, so a re-entry
//     skips the steps that already happened.
//
// The file is removed only once the operation completed. With -session the
// export's account must agree with <dir>/session.json before any remote call.
//
// Output is limited to counts, names, states, operation ids and error codes:
// cookie and token values never reach stdout, stderr, or a log.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/icloud"
)

// pendingFileName keeps the interrupted operation in the account directory it
// belongs to. It holds addresses and an operation id, never cookies.
const pendingFileName = "hme-pending.json"

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

// hmeClient is the slice of the Hide My Email client this CLI drives. The
// interface exists so the resume-after-interruption logic runs without a
// network.
type hmeClient interface {
	Generate(ctx context.Context, language string) (string, error)
	Reserve(ctx context.Context, candidate, label, note string) error
	SetActive(ctx context.Context, email string, active bool) error
	Delete(ctx context.Context, email string) error
}

// pendingOp is the write-ahead record for one remote write.
type pendingOp struct {
	OperationID    string `json:"operation_id"`
	Action         string `json:"action"`
	Address        string `json:"address"`
	Label          string `json:"label,omitempty"`
	DeactivateDone bool   `json:"deactivate_done,omitempty"`
	DeleteDone     bool   `json:"delete_done,omitempty"`
	CreatedAt      string `json:"created_at"`
}

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprintln(out, "usage: hmecli -dir ACCOUNT_DIR [-session ICLOUD_WEB_SESSION_JSON] -action list|generate|reserve|delete [-email ADDRESS] [-label LABEL]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "adopts an existing iCloud web session (no password, no 2FA) and drives Hide My Email")
	fmt.Fprintln(out, "interrupted reserve/delete operations resume from <dir>/"+pendingFileName)
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
		return reserve(ctx, client, *dir, strings.TrimSpace(*email), strings.TrimSpace(*label), os.Stdout)
	case "delete":
		return deleteAlias(ctx, client, *dir, strings.TrimSpace(*email), os.Stdout)
	}
	return nil
}

// reserve persists the candidate, label and operation id before the remote
// call. A re-entry after an interrupted reserve reuses that candidate instead
// of generating another one.
func reserve(ctx context.Context, client hmeClient, dir, candidateOpt, labelOpt string, out io.Writer) error {
	pending, err := readPending(dir)
	if err != nil {
		return err
	}
	var op *pendingOp
	switch {
	case pending == nil:
		label := labelOpt
		if label == "" {
			return errors.New("-label is required for reserve")
		}
		candidate := candidateOpt
		if candidate == "" {
			generated, generateErr := client.Generate(ctx, "zh-cn")
			if generateErr != nil {
				return generateErr
			}
			candidate = generated
		}
		op = &pendingOp{
			OperationID: newOperationID(),
			Action:      "reserve",
			Address:     candidate,
			Label:       label,
			CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		}
		if err := writePending(dir, op); err != nil {
			return fmt.Errorf("write-ahead %s failed, no remote call was made: %w", pendingFileName, err)
		}
	case pending.Action != "reserve":
		return fmt.Errorf("directory holds an unfinished %s operation (%s); finish or remove %s first",
			pending.Action, pending.OperationID, pendingPath(dir))
	case candidateOpt != "" && !strings.EqualFold(candidateOpt, pending.Address):
		return fmt.Errorf("unfinished reserve targets %s, not -email %s; finish it or remove %s",
			pending.Address, candidateOpt, pendingPath(dir))
	default:
		op = pending
		fmt.Fprintf(out, "pending=resumed operation=%s candidate=%s label=%s\n", op.OperationID, op.Address, op.Label)
	}
	if err := client.Reserve(ctx, op.Address, op.Label, ""); err != nil {
		return err
	}
	if err := clearPending(dir); err != nil {
		return err
	}
	fmt.Fprintf(out, "reserved=%s label=%s operation=%s\n", op.Address, op.Label, op.OperationID)
	return nil
}

// deleteAlias deactivates then deletes, recording each finished step on disk so
// a re-entry skips what already happened.
func deleteAlias(ctx context.Context, client hmeClient, dir, addressOpt string, out io.Writer) error {
	address := addressOpt
	if address == "" {
		return errors.New("-email is required for delete")
	}
	pending, err := readPending(dir)
	if err != nil {
		return err
	}
	var op *pendingOp
	switch {
	case pending == nil:
		op = &pendingOp{
			OperationID: newOperationID(),
			Action:      "delete",
			Address:     address,
			CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		}
		if err := writePending(dir, op); err != nil {
			return fmt.Errorf("write-ahead %s failed, no remote call was made: %w", pendingFileName, err)
		}
	case pending.Action != "delete":
		return fmt.Errorf("directory holds an unfinished %s operation (%s); finish or remove %s first",
			pending.Action, pending.OperationID, pendingPath(dir))
	case !strings.EqualFold(pending.Address, address):
		return fmt.Errorf("unfinished delete targets %s, not -email %s; finish it or remove %s",
			pending.Address, address, pendingPath(dir))
	default:
		op = pending
	}
	if !op.DeactivateDone {
		if err := client.SetActive(ctx, op.Address, false); err != nil {
			return err
		}
		op.DeactivateDone = true
		if err := writePending(dir, op); err != nil {
			return fmt.Errorf("record deactivate_done failed: %w", err)
		}
	} else {
		fmt.Fprintf(out, "pending=resumed operation=%s stage=deactivate_done\n", op.OperationID)
	}
	if !op.DeleteDone {
		if err := client.Delete(ctx, op.Address); err != nil {
			return err
		}
		op.DeleteDone = true
		if err := writePending(dir, op); err != nil {
			return fmt.Errorf("record delete_done failed: %w", err)
		}
	} else {
		fmt.Fprintf(out, "pending=resumed operation=%s stage=delete_done\n", op.OperationID)
	}
	if err := clearPending(dir); err != nil {
		return err
	}
	fmt.Fprintf(out, "deleted=%s operation=%s\n", op.Address, op.OperationID)
	return nil
}

func pendingPath(dir string) string { return filepath.Join(dir, pendingFileName) }

func readPending(dir string) (*pendingOp, error) {
	data, err := os.ReadFile(pendingPath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", pendingFileName, err)
	}
	op := &pendingOp{}
	if err := json.Unmarshal(data, op); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON, inspect it before retrying: %w", pendingFileName, err)
	}
	if strings.TrimSpace(op.Action) == "" || strings.TrimSpace(op.Address) == "" {
		return nil, fmt.Errorf("%s has no action/address, inspect it before retrying", pendingFileName)
	}
	return op, nil
}

// writePending replaces the record atomically, so a crash mid-write can never
// leave a half-written operation behind.
func writePending(dir string, op *pendingOp) error {
	data, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".hme-pending-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, pendingPath(dir))
}

func clearPending(dir string) error {
	if err := os.Remove(pendingPath(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("operation succeeded but %s could not be removed: %w", pendingFileName, err)
	}
	return nil
}

func newOperationID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("op%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// accountBinding resolves the Apple Account this run belongs to. With -session
// the browser export is authoritative, but it must agree with the binding in
// <dir>/session.json: adopting another account's cookies into this directory
// would silently mix two identities. The check runs before any remote call.
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
		boundAppleID, bindErr := directoryAppleID(dir)
		if bindErr != nil {
			return "", "", nil, bindErr
		}
		if boundAppleID != "" && !strings.EqualFold(boundAppleID, appleID) {
			return "", "", nil, fmt.Errorf("session file is bound to %s but %s is bound to %s; refusing before any remote call",
				appleID, filepath.Join(dir, "session.json"), boundAppleID)
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

// directoryAppleID reads the Apple ID bound to the account directory. A
// directory without session.json (or without a readable binding in it) returns
// "" so a freshly prepared directory can still adopt a session.
func directoryAppleID(dir string) (string, error) {
	path := filepath.Join(dir, "session.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var bound struct {
		AppleID      string `json:"appleId"`
		AppleIDSnake string `json:"apple_id"`
	}
	if json.Unmarshal(data, &bound) != nil {
		return "", nil
	}
	if id := strings.TrimSpace(bound.AppleID); id != "" {
		return id, nil
	}
	return strings.TrimSpace(bound.AppleIDSnake), nil
}
