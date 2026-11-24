// familyinspect performs read-only discovery of Apple's Family Sharing account
// endpoint and, when available, returns a redacted membership summary.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	appleservices "github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/gsa"
	"github.com/Laky-64/appleservices/icloud"
	"github.com/Laky-64/appleservices/nativeadi"
	"github.com/google/uuid"
	"golang.org/x/net/html"
	"howett.net/plist"
)

const configurationURL = "https://setup.icloud.com/configurations/init?context=settings"

type acceptedCheckpoint struct {
	Session     appleservices.Session `json:"session"`
	ADSID       string                `json:"adsid"`
	ServiceData map[string]any        `json:"service-data"`
}

type familyInviteCheckpoint struct {
	TargetHash string    `json:"target_hash"`
	InviteCode string    `json:"invite_code"`
	CreatedAt  time.Time `json:"created_at"`
}

const safeFamilyStatusPrefix = "FAMILY_STATUS_JSON="

type safeFamilySummary struct {
	Member                 bool  `json:"member"`
	Eligible               bool  `json:"eligible"`
	EligibilityKnown       bool  `json:"eligibility_known"`
	Members                int   `json:"members"`
	Organizers             int   `json:"organizers"`
	Self                   int   `json:"self"`
	Children               int   `json:"children"`
	Invitations            int   `json:"invitations"`
	Pending                int   `json:"pending"`
	Organizer              bool  `json:"organizer"`
	StorageKnown           bool  `json:"storage_known"`
	StorageParticipating   bool  `json:"storage_participating"`
	StorageAvailable       bool  `json:"storage_available"`
	StorageEnabled         bool  `json:"storage_enabled"`
	StorageBytes           int64 `json:"storage_bytes,omitempty"`
	PurchaseSharingKnown   bool  `json:"purchase_sharing_known"`
	PurchaseSharingEnabled bool  `json:"purchase_sharing_enabled"`
	LocationSharingKnown   bool  `json:"location_sharing_known"`
	LocationSharingEnabled bool  `json:"location_sharing_enabled"`
}

func decodeObject(data []byte) (map[string]any, error) {
	var value map[string]any
	if json.Unmarshal(data, &value) == nil {
		return value, nil
	}
	if _, err := plist.Unmarshal(data, &value); err == nil {
		return value, nil
	}
	return nil, errors.New("response is not JSON or plist")
}

func appleEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	host := strings.ToLower(u.Hostname())
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" ||
		!(host == "icloud.com" || strings.HasSuffix(host, ".icloud.com") || host == "apple.com" || strings.HasSuffix(host, ".apple.com")) {
		return nil, errors.New("family endpoint is outside Apple HTTPS")
	}
	return u, nil
}

func readLimited(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return nil, errors.New("family response is invalid")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("family request returned HTTP %d", resp.StatusCode)
	}
	return data, nil
}

func tokenHeader(dsid, token string) string {
	return base64.StdEncoding.EncodeToString([]byte(dsid + ":" + token))
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func numericIDValue(v any) string {
	var value string
	switch x := v.(type) {
	case string:
		value = strings.TrimSpace(x)
	case int:
		if x > 0 {
			value = strconv.Itoa(x)
		}
	case int64:
		if x > 0 {
			value = strconv.FormatInt(x, 10)
		}
	case uint64:
		if x > 0 {
			value = strconv.FormatUint(x, 10)
		}
	case float64:
		if x > 0 && x == math.Trunc(x) && x <= 1<<53 {
			value = strconv.FormatUint(uint64(x), 10)
		}
	}
	if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return ""
	}
	return value
}

func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

func familyMemberAppleID(member map[string]any) string {
	if value := stringValue(member["appleId"]); value != "" {
		return value
	}
	return stringValue(member["member-apple-id"])
}

func summarizeFamilyMembers(members []any) (organizers, children, me int, selfOrganizer bool, selfMember map[string]any) {
	for _, raw := range members {
		member, _ := raw.(map[string]any)
		if boolValue(member["member-is-organizer"]) {
			organizers++
		}
		if boolValue(member["member-is-child-account"]) {
			children++
		}
		if boolValue(member["is-me"]) {
			me++
			selfMember = member
			selfOrganizer = boolValue(member["member-is-organizer"])
		}
	}
	return
}

func integerValue(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case uint64:
		if x <= math.MaxInt64 {
			return int64(x)
		}
	case float64:
		if x >= 0 && x == math.Trunc(x) && x <= math.MaxInt64 {
			return int64(x)
		}
	case string:
		value, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return value
	}
	return 0
}

func normalizedFamilyKey(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		if r >= 'a' && r <= 'z' {
			return r
		}
		return -1
	}, value)
}

func findFamilyBool(value any, match func(string) bool) (bool, bool) {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if match(normalizedFamilyKey(key)) {
				if result, ok := child.(bool); ok {
					return result, true
				}
			}
		}
		for _, child := range current {
			if result, ok := findFamilyBool(child, match); ok {
				return result, true
			}
		}
	case []any:
		for _, child := range current {
			if result, ok := findFamilyBool(child, match); ok {
				return result, true
			}
		}
	}
	return false, false
}

func findFamilyInteger(value any, key string) int64 {
	switch current := value.(type) {
	case map[string]any:
		for candidate, child := range current {
			if strings.EqualFold(candidate, key) {
				if result := integerValue(child); result > 0 {
					return result
				}
			}
		}
		for _, child := range current {
			if result := findFamilyInteger(child, key); result > 0 {
				return result
			}
		}
	case []any:
		for _, child := range current {
			if result := findFamilyInteger(child, key); result > 0 {
				return result
			}
		}
	}
	return 0
}

func headerValue(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

type safeFamilyForm struct {
	Method string
	Host   string
	Path   string
	Fields []string
}

func safeFamilyUIForms(origin *url.URL, data []byte) []safeFamilyForm {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	forms := make([]safeFamilyForm, 0)
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "form" {
			method, action := "GET", ""
			for _, attr := range node.Attr {
				switch strings.ToLower(attr.Key) {
				case "method":
					method = strings.ToUpper(strings.TrimSpace(attr.Val))
				case "action":
					action = strings.TrimSpace(attr.Val)
				}
			}
			destination := origin
			if ref, parseErr := url.Parse(action); parseErr == nil && ref != nil {
				destination = origin.ResolveReference(ref)
			}
			form := safeFamilyForm{Method: method, Host: destination.Hostname(), Path: destination.EscapedPath()}
			var fields func(*html.Node)
			fields = func(child *html.Node) {
				if child.Type == html.ElementNode && (child.Data == "input" || child.Data == "button" || child.Data == "select") {
					name, kind := "", child.Data
					for _, attr := range child.Attr {
						if strings.EqualFold(attr.Key, "name") {
							name = strings.TrimSpace(attr.Val)
						}
						if strings.EqualFold(attr.Key, "type") && strings.TrimSpace(attr.Val) != "" {
							kind = strings.ToLower(strings.TrimSpace(attr.Val))
						}
					}
					if name != "" {
						form.Fields = append(form.Fields, kind+":"+name)
					}
				}
				for next := child.FirstChild; next != nil; next = next.NextSibling {
					fields(next)
				}
			}
			fields(node)
			sort.Strings(form.Fields)
			forms = append(forms, form)
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return forms
}

func safeFamilyUIScripts(origin *url.URL, data []byte) []*url.URL {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	documentBase := origin
	var findBase func(*html.Node)
	findBase = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "base" {
			for _, attr := range node.Attr {
				if !strings.EqualFold(attr.Key, "href") || strings.TrimSpace(attr.Val) == "" {
					continue
				}
				ref, parseErr := url.Parse(strings.TrimSpace(attr.Val))
				if parseErr == nil {
					candidate := origin.ResolveReference(ref)
					if _, validationErr := appleEndpoint(candidate.String()); validationErr == nil {
						documentBase = candidate
					}
				}
			}
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			findBase(child)
		}
	}
	findBase(doc)
	scripts := make([]*url.URL, 0)
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "script" {
			for _, attr := range node.Attr {
				if !strings.EqualFold(attr.Key, "src") || strings.TrimSpace(attr.Val) == "" {
					continue
				}
				ref, parseErr := url.Parse(strings.TrimSpace(attr.Val))
				if parseErr != nil {
					continue
				}
				destination := documentBase.ResolveReference(ref)
				if _, validationErr := appleEndpoint(destination.String()); validationErr == nil {
					scripts = append(scripts, destination)
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return scripts
}

var safeJavaScriptIdentifier = regexp.MustCompile(`[A-Za-z_$][A-Za-z0-9_$]{2,}`)

func safeFamilyActionIdentifiers(data []byte) []string {
	seen := map[string]bool{}
	for _, match := range safeJavaScriptIdentifier.FindAll(data, -1) {
		identifier := string(match)
		lower := strings.ToLower(identifier)
		if strings.Contains(lower, "family") || strings.Contains(lower, "invite") || strings.Contains(lower, "organizer") || strings.Contains(lower, "setup") || strings.Contains(lower, "finish") || strings.Contains(lower, "create") {
			seen[identifier] = true
		}
	}
	result := make([]string, 0, len(seen))
	for identifier := range seen {
		result = append(result, identifier)
	}
	sort.Strings(result)
	return result
}

func run() error {
	dir := flag.String("dir", "", "bound account directory")
	gsaCheckpoint := flag.String("gsa", "", "optional fresh private GSA checkpoint for family app tokens")
	dump := flag.String("dump", "", "new private raw family response")
	listEndpoints := flag.Bool("list-endpoints", false, "print safe family endpoint keys, hosts and paths")
	inspectSetupUI := flag.Bool("inspect-setup-ui", false, "GET the current setup-family UI and print only safe form metadata")
	setupUIDump := flag.String("setup-ui-dump", "", "new private raw setup-family UI response; requires -inspect-setup-ui")
	createFamily := flag.Bool("create-family", false, "create one family with this account as organizer; no invitation or purchase sharing")
	inspectSubscriptions := flag.Bool("subscriptions", false, "read the current family subscription summary")
	subscriptionsDump := flag.String("subscriptions-dump", "", "new private raw family subscriptions response; requires -subscriptions")
	inspectStorageUI := flag.Bool("inspect-storage-ui", false, "GET the current family iCloud storage UI and print safe metadata")
	storageUIDump := flag.String("storage-ui-dump", "", "new private raw family storage UI response; requires -inspect-storage-ui")
	inspectICloudSharingUI := flag.Bool("inspect-icloud-sharing-ui", false, "GET the server-bound iCloud+ family subscription UI and print safe metadata")
	iCloudSharingUIDump := flag.String("icloud-sharing-ui-dump", "", "new private raw iCloud+ sharing UI response; requires -inspect-icloud-sharing-ui")
	shareICloudStorage := flag.Bool("share-icloud-storage", false, "enable this organizer's current iCloud+ plan for the bound family")
	joinSharedStorage := flag.Bool("join-shared-storage", false, "switch this non-organizer family member to the available shared iCloud+ plan")
	addPoolMemberDir := flag.String("add-pool-member-dir", "", "add one independently authenticated pool account through Apple's PET in-person flow")
	removePoolMemberDir := flag.String("remove-pool-member-dir", "", "remove one current non-child pool member from this organizer's family")
	verifyPoolMemberDir := flag.String("verify-pool-member-dir", "", "read-only: require this exact local account in the organizer family")
	targetPasswordEnv := flag.Bool("target-password-env", false, "use and immediately clear APPLESERVICES_TARGET_PASSWORD as Apple's rawFallback credential")
	inviteAccountDir := flag.String("invite-account-dir", "", "target pool account directory for a new organizer invitation")
	inviteOut := flag.String("invite-out", "", "new private invite checkpoint for the target account")
	acceptInvite := flag.String("accept-invite", "", "private invite checkpoint bound to this account")
	safeJSON := flag.Bool("safe-json", false, "print one redacted machine-readable family status line")
	flag.Parse()
	if *inspectICloudSharingUI {
		*inspectSubscriptions = true
	}
	if *dir == "" {
		return errors.New("bound account directory is required")
	}
	if *setupUIDump != "" && !*inspectSetupUI {
		return errors.New("setup UI dump requires setup UI inspection")
	}
	if *subscriptionsDump != "" && !*inspectSubscriptions {
		return errors.New("subscriptions dump requires subscriptions inspection")
	}
	if *storageUIDump != "" && !*inspectStorageUI {
		return errors.New("storage UI dump requires storage UI inspection")
	}
	if *iCloudSharingUIDump != "" && !*inspectICloudSharingUI {
		return errors.New("iCloud sharing UI dump requires iCloud sharing UI inspection")
	}
	if (*createFamily && *shareICloudStorage) || (*shareICloudStorage && *joinSharedStorage) || (*createFamily && *joinSharedStorage) {
		return errors.New("family creation and storage mutations must be separate verified operations")
	}
	if (*inviteAccountDir == "") != (*inviteOut == "") {
		return errors.New("invite account directory and invite output are required together")
	}
	if *acceptInvite != "" && (*inviteAccountDir != "" || *createFamily || *shareICloudStorage) {
		return errors.New("invite acceptance must be a separate target-account operation")
	}
	if *addPoolMemberDir != "" && (*removePoolMemberDir != "" || *inviteAccountDir != "" || *acceptInvite != "" || *createFamily || *shareICloudStorage) {
		return errors.New("pool member addition must be a separate verified operation")
	}
	if *removePoolMemberDir != "" && (*inviteAccountDir != "" || *acceptInvite != "" || *createFamily || *shareICloudStorage || *joinSharedStorage) {
		return errors.New("pool member removal must be a separate verified operation")
	}
	if *joinSharedStorage && (*inviteAccountDir != "" || *acceptInvite != "" || *addPoolMemberDir != "" || *removePoolMemberDir != "") {
		return errors.New("joining shared storage must be a separate current-member operation")
	}
	if *targetPasswordEnv && *addPoolMemberDir == "" {
		return errors.New("target password environment requires pool member addition")
	}
	var saved appleservices.Session
	data, err := os.ReadFile(filepath.Join(*dir, "session.json"))
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.AppleID == "" || saved.DSID == "" {
		return errors.New("bound account session unavailable")
	}
	auth := saved
	if *gsaCheckpoint == "" {
		fresh := filepath.Join(*dir, "family-gsa.json")
		if info, err := os.Stat(fresh); err == nil && time.Since(info.ModTime()) >= 0 && time.Since(info.ModTime()) < 10*time.Minute {
			*gsaCheckpoint = fresh
		}
	}
	if *gsaCheckpoint != "" {
		data, err = os.ReadFile(*gsaCheckpoint)
		if err != nil || len(data) > 2<<20 || json.Unmarshal(data, &auth) != nil || !strings.EqualFold(auth.AppleID, saved.AppleID) || auth.DSID != saved.DSID {
			return errors.New("fresh GSA checkpoint account binding failed")
		}
	}
	var accepted acceptedCheckpoint
	data, err = os.ReadFile(filepath.Join(*dir, "icloud-accepted.json"))
	if err != nil || json.Unmarshal(data, &accepted) != nil || !strings.EqualFold(saved.AppleID, accepted.Session.AppleID) || accepted.ADSID != saved.DSID || accepted.Session.DSID == "" || accepted.Session.Tokens["mmeAuthToken"] == "" {
		return errors.New("accepted iCloud checkpoint unavailable")
	}
	icloudToken, familyToken := auth.Tokens["com.apple.gs.icloud.auth"], auth.Tokens["com.apple.gs.icloud.family.auth"]
	if icloudToken == "" || familyToken == "" {
		return errors.New("family GrandSlam tokens unavailable")
	}
	var device appleservices.Device
	data, err = os.ReadFile(filepath.Join(*dir, "device.json"))
	if err != nil || json.Unmarshal(data, &device) != nil || len(device.Identifier) != 16 || device.SerialNumber == "" {
		return errors.New("bound device unavailable")
	}
	var hardware struct {
		Push struct {
			Token []byte `plist:"token"`
		} `plist:"push"`
	}
	profile := filepath.Join(*dir, "native-adi")
	data, err = os.ReadFile(filepath.Join(profile, "hw_info.plist"))
	if err != nil {
		return errors.New("bound Mac hardware unavailable")
	}
	if _, err := plist.Unmarshal(data, &hardware); err != nil || len(hardware.Push.Token) != 32 {
		return errors.New("bound APS token unavailable")
	}
	library, _ := filepath.Abs(".cache/openbubbles-store/rust_lib_bluebubbles.dll")
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	native, err := nativeadi.NewClient(nativeadi.Config{LibraryPath: library, ProfileDir: profile, Python: "python", Identifier: device.Identifier, Serial: device.SerialNumber, PushToken: hardware.Push.Token})
	if err != nil {
		return err
	}
	if err := native.Start(ctx); err != nil {
		return err
	}
	defer native.Close()
	headers, err := native.Headers()
	if err != nil {
		return err
	}
	transport := gsa.AppleRootTransport().(*http.Transport)
	// setup.icloud.com/configurations/init currently stalls on this Windows
	// route after HTTP/2 negotiation while the same request succeeds over 1.1.
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	defer transport.CloseIdleConnections()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, configurationURL, nil)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("User-Agent", "Setup/1.0 CFNetwork/1408.0.4 Darwin/22.5.0")
	request.Header.Set("X-MMe-Country", "CN")
	request.Header.Set("X-MMe-Language", "en-US")
	request.Header.Set("X-Apple-Locale", "en_US")
	request.Header.Set("X-MMe-Nac-Version", "22C65")
	resp, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("family configuration transport failed: %w", err)
	}
	configurationData, err := readLimited(resp)
	if err != nil {
		return err
	}
	configuration, err := decodeObject(configurationData)
	if err != nil {
		return err
	}
	urls, _ := configuration["urls"].(map[string]any)
	if urls == nil {
		return errors.New("family configuration has no URL bag")
	}
	apiTransport := gsa.AppleRootTransport().(*http.Transport)
	defer apiTransport.CloseIdleConnections()
	apiClient := &http.Client{Jar: jar, Transport: apiTransport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	keys := make([]string, 0)
	for key := range urls {
		if strings.Contains(strings.ToLower(key), "family") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	fmt.Printf("Apple family URL bag loaded; family-keys=%d\n", len(keys))
	if *listEndpoints {
		for _, key := range keys {
			u, err := appleEndpoint(stringValue(urls[key]))
			if err == nil {
				fmt.Printf("family endpoint key=%s host=%s path=%s\n", key, u.Hostname(), u.EscapedPath())
			}
		}
	}
	rawDetails := stringValue(urls["getFamilyDetails"])
	if rawDetails == "" {
		rawDetails = stringValue(urls["getFamilyDetailsURL"])
	}
	if rawDetails == "" {
		return errors.New("Apple bag did not return a family-details endpoint")
	}
	detailsURL, err := appleEndpoint(rawDetails)
	if err != nil {
		return err
	}
	newFamilyRequest := func(endpoint string, fresno bool) (*http.Request, error) {
		u, err := appleEndpoint(endpoint)
		if err != nil {
			return nil, err
		}
		var body []byte
		if fresno {
			accountDetails := map[string]any{
				"appleID":       saved.AppleID,
				"dsid":          accepted.Session.DSID,
				"altDSID":       saved.DSID,
				"dsFamilyToken": familyToken,
			}
			body, err = plist.Marshal(map[string]any{"iCloudAccountDetails": accountDetails, "iTunesAccountDetails": accountDetails}, plist.XMLFormat)
			if err != nil {
				return nil, errors.New("cannot encode family account binding")
			}
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		request.Header.Set("User-Agent", "com.apple.Preferences/1.0 CFNetwork/1408.0.4 Darwin/22.5.0")
		request.Header.Set("X-MMe-Client-Info", "<MacBookPro15,1> <Mac OS X;13.1;22C65> <com.apple.AppleAccount/1.0 (com.apple.systempreferences.AppleIDSettings/1)>")
		request.Header.Set("X-MMe-Country", "CN")
		request.Header.Set("X-MMe-Language", "en-US")
		request.Header.Set("Accept", "application/json, text/plist, application/x-plist")
		request.Header.Set("Accept-Language", "en-US")
		if fresno {
			request.Header.Set("Content-Type", "application/x-plist")
			request.Header.Set("X-APNS-Push-Token", strings.ToUpper(fmt.Sprintf("%x", hardware.Push.Token)))
		}
		request.Header.Set("Authorization", "Basic "+tokenHeader(accepted.Session.DSID, accepted.Session.Tokens["mmeAuthToken"]))
		// Apple's current macOS FamilyKit client binds Basic and both GS app
		// tokens to the numeric iCloud DSID. The AltDSID belongs only in the
		// separate X-Apple-ADSID header.
		request.Header.Set("X-Apple-GS-Token", tokenHeader(accepted.Session.DSID, icloudToken))
		request.Header.Set("X-Apple-Family-GS-Token", tokenHeader(accepted.Session.DSID, familyToken))
		request.Header.Set("X-Apple-ADSID", saved.DSID)
		request.Header.Set("X-MMe-LoggedIn-AppleID", saved.AppleID)
		request.Header.Set("X-MMe-Device-Id", strings.ToUpper(uuid.UUID(device.Identifier).String()))
		request.Header.Set("X-MMe-FMFAllowed", "false")
		if value := headerValue(headers, "X-Apple-I-MD"); value != "" {
			request.Header.Set("X-Apple-MD", value)
		}
		if value := headerValue(headers, "X-Apple-I-MD-M"); value != "" {
			request.Header.Set("X-Apple-MD-M", value)
		}
		return request, nil
	}
	acceptedInvitation := false
	if *acceptInvite != "" {
		inviteData, readErr := os.ReadFile(*acceptInvite)
		var checkpoint familyInviteCheckpoint
		if readErr != nil || len(inviteData) > 64<<10 || json.Unmarshal(inviteData, &checkpoint) != nil || checkpoint.InviteCode == "" || time.Since(checkpoint.CreatedAt) < 0 || time.Since(checkpoint.CreatedAt) > 15*time.Minute {
			return errors.New("private family invitation checkpoint is invalid or expired")
		}
		boundHash := accountBindingHash(saved.AppleID)
		if subtle.ConstantTimeCompare([]byte(boundHash), []byte(checkpoint.TargetHash)) != 1 {
			return errors.New("family invitation checkpoint target binding failed")
		}
		acceptURL, acceptErr := appleEndpoint(stringValue(urls["acceptFamilyInvitation"]))
		if acceptErr != nil {
			return errors.New("Apple bag did not return a valid invitation acceptance endpoint")
		}
		acceptBody, encodeErr := json.Marshal(map[string]any{
			"invitationCode":                 checkpoint.InviteCode,
			"appleId":                        saved.AppleID,
			"appleIdForPurchases":            saved.AppleID,
			"shareMyPurchasesEnabledDefault": false,
			"shareMyLocationEnabledDefault":  false,
		})
		if encodeErr != nil {
			return errors.New("cannot encode family invitation acceptance")
		}
		acceptRequest, requestErr := newFamilyRequest(acceptURL.String(), false)
		if requestErr != nil {
			return requestErr
		}
		acceptRequest.Method = http.MethodPost
		acceptRequest.Body = io.NopCloser(bytes.NewReader(acceptBody))
		acceptRequest.ContentLength = int64(len(acceptBody))
		acceptRequest.Header.Set("Content-Type", "application/json")
		acceptRequest.Header.Set("Accept", "application/json; charset=UTF-8")
		acceptResponse, requestErr := client.Do(acceptRequest)
		if requestErr != nil {
			return fmt.Errorf("family invitation acceptance transport failed: %w", requestErr)
		}
		acceptData, responseErr := io.ReadAll(io.LimitReader(acceptResponse.Body, (2<<20)+1))
		acceptResponse.Body.Close()
		if responseErr != nil || len(acceptData) > 2<<20 {
			return errors.New("family invitation acceptance response is invalid")
		}
		if acceptResponse.StatusCode != http.StatusOK {
			return fmt.Errorf("family invitation acceptance returned HTTP %d", acceptResponse.StatusCode)
		}
		accepted, decodeErr := decodeObject(acceptData)
		if decodeErr != nil {
			return errors.New("family invitation acceptance returned an unreadable response")
		}
		if familyResponseError(accepted) {
			checkpoint.InviteCode = ""
			_ = os.Remove(*acceptInvite)
			return errors.New("Apple rejected the family invitation code")
		}
		checkpoint.InviteCode = ""
		if removeErr := os.Remove(*acceptInvite); removeErr != nil {
			return errors.New("family invitation succeeded but private checkpoint cleanup failed")
		}
		acceptedInvitation = true
		fmt.Printf("Apple accepted the bound family invitation; HTTP=%d response-keys=%s purchase-sharing-enabled=false location-sharing-enabled=false\n", acceptResponse.StatusCode, strings.Join(sortedMapKeys(accepted), ","))
	}
	if *createFamily {
		createURL, createErr := appleEndpoint("https://setup.icloud.com/setup/mac/family/createFamily")
		if createErr != nil {
			return createErr
		}
		createBody, encodeErr := json.Marshal(map[string]any{
			"organizerAppleId":                        saved.AppleID,
			"organizerAppleIdForPurchases":            saved.AppleID,
			"organizerShareMyLocationEnabledDefault":  false,
			"organizerShareMyPurchasesEnabledDefault": false,
		})
		if encodeErr != nil {
			return errors.New("cannot encode family creation request")
		}
		createRequest, requestErr := newFamilyRequest(createURL.String(), false)
		if requestErr != nil {
			return requestErr
		}
		createRequest.Method = http.MethodPost
		createRequest.Body = io.NopCloser(bytes.NewReader(createBody))
		createRequest.ContentLength = int64(len(createBody))
		createRequest.Header.Set("Content-Type", "application/json")
		createRequest.Header.Set("Accept", "application/json; charset=UTF-8")
		createResponse, requestErr := client.Do(createRequest)
		if requestErr != nil {
			return fmt.Errorf("family creation transport failed: %w", requestErr)
		}
		createData, readErr := io.ReadAll(io.LimitReader(createResponse.Body, (2<<20)+1))
		createResponse.Body.Close()
		if readErr != nil || len(createData) > 2<<20 {
			return errors.New("family creation response is invalid")
		}
		if createResponse.StatusCode != http.StatusOK {
			return fmt.Errorf("family creation returned HTTP %d", createResponse.StatusCode)
		}
		created, decodeErr := decodeObject(createData)
		if decodeErr != nil {
			return errors.New("family creation returned an unreadable response")
		}
		if value, ok := created["success"].(bool); ok && !value {
			return errors.New("Apple did not confirm family creation")
		}
		fmt.Printf("Apple accepted family creation; HTTP=%d response-keys=%s; invitations-submitted=0 purchase-sharing-enabled=false\n", createResponse.StatusCode, strings.Join(sortedMapKeys(created), ","))
	}
	if *inspectSetupUI {
		rawSetup := stringValue(urls["setupFamilyUI"])
		if rawSetup == "" {
			return errors.New("Apple bag did not return setupFamilyUI")
		}
		setupRequest, requestErr := newFamilyRequest(rawSetup, false)
		if requestErr != nil {
			return requestErr
		}
		setupRequest.Method = http.MethodGet
		setupRequest.Body = nil
		setupResponse, requestErr := client.Do(setupRequest)
		if requestErr != nil {
			return fmt.Errorf("family setup UI transport failed: %w", requestErr)
		}
		setupData, readErr := io.ReadAll(io.LimitReader(setupResponse.Body, (2<<20)+1))
		setupResponse.Body.Close()
		if readErr != nil || len(setupData) > 2<<20 {
			return errors.New("family setup UI response is invalid")
		}
		if *setupUIDump != "" {
			file, openErr := os.OpenFile(*setupUIDump, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if openErr != nil {
				return errors.New("private setup UI dump must be a new file")
			}
			if _, writeErr := file.Write(setupData); writeErr != nil || file.Close() != nil {
				return errors.New("cannot preserve private setup UI response")
			}
		}
		locationHost, locationPath := "", ""
		if rawLocation := setupResponse.Header.Get("Location"); rawLocation != "" {
			if location, locationErr := appleEndpoint(setupRequest.URL.ResolveReference(&url.URL{Path: rawLocation}).String()); locationErr == nil {
				locationHost, locationPath = location.Hostname(), location.EscapedPath()
			}
		}
		forms := safeFamilyUIForms(setupRequest.URL, setupData)
		fmt.Printf("family setup UI inspected; HTTP=%d content-type=%q bytes=%d forms=%d redirect-host=%s redirect-path=%s; mutation-submitted=false\n", setupResponse.StatusCode, setupResponse.Header.Get("Content-Type"), len(setupData), len(forms), locationHost, locationPath)
		for index, form := range forms {
			fmt.Printf("family setup form=%d method=%s host=%s path=%s fields=%s\n", index+1, form.Method, form.Host, form.Path, strings.Join(form.Fields, ","))
		}
		for index, scriptURL := range safeFamilyUIScripts(setupRequest.URL, setupData) {
			scriptRequest, scriptErr := newFamilyRequest(scriptURL.String(), false)
			if scriptErr != nil {
				return scriptErr
			}
			scriptRequest.Method = http.MethodGet
			scriptRequest.Body = nil
			scriptRequest.Header.Set("Referer", setupRequest.URL.String())
			scriptResponse, scriptErr := client.Do(scriptRequest)
			if scriptErr != nil {
				return fmt.Errorf("family setup script transport failed: %w", scriptErr)
			}
			scriptData, scriptReadErr := io.ReadAll(io.LimitReader(scriptResponse.Body, (2<<20)+1))
			scriptResponse.Body.Close()
			if scriptReadErr != nil || len(scriptData) > 2<<20 {
				return errors.New("family setup script response is invalid")
			}
			identifiers := safeFamilyActionIdentifiers(scriptData)
			fmt.Printf("family setup script=%d HTTP=%d host=%s path=%s bytes=%d action-identifiers=%s; mutation-submitted=false\n", index+1, scriptResponse.StatusCode, scriptURL.Hostname(), scriptURL.EscapedPath(), len(scriptData), strings.Join(identifiers, ","))
		}
	}
	if *inspectSubscriptions {
		rawSubscriptions := stringValue(urls["getFamilySubscriptions"])
		if rawSubscriptions == "" {
			return errors.New("Apple bag did not return family subscriptions endpoint")
		}
		subscriptionRequest, requestErr := newFamilyRequest(rawSubscriptions, false)
		if requestErr != nil {
			return requestErr
		}
		subscriptionRequest.Method = http.MethodGet
		subscriptionRequest.Body = nil
		subscriptionResponse, requestErr := client.Do(subscriptionRequest)
		if requestErr != nil {
			return fmt.Errorf("family subscriptions transport failed: %w", requestErr)
		}
		subscriptionData, readErr := readLimited(subscriptionResponse)
		if readErr != nil {
			return readErr
		}
		subscriptions, decodeErr := decodeObject(subscriptionData)
		if decodeErr != nil {
			return errors.New("family subscriptions returned an unreadable response")
		}
		if *subscriptionsDump != "" {
			file, openErr := os.OpenFile(*subscriptionsDump, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if openErr != nil {
				return errors.New("private subscriptions dump must be a new file")
			}
			if _, writeErr := file.Write(subscriptionData); writeErr != nil || file.Close() != nil {
				return errors.New("cannot preserve private subscriptions response")
			}
		}
		lower := bytes.ToLower(subscriptionData)
		mentionsICloud := bytes.Contains(lower, []byte("icloud"))
		mentions50GB := bytes.Contains(lower, []byte("50 gb")) || bytes.Contains(lower, []byte("50gb")) || bytes.Contains(lower, []byte("53687091200"))
		fmt.Printf("family subscriptions accepted; response-keys=%s icloud-present=%t 50gb-present=%t; mutation-submitted=false\n", strings.Join(sortedMapKeys(subscriptions), ","), mentionsICloud, mentions50GB)
		if *inspectICloudSharingUI {
			services, _ := subscriptions["familySharedServices"].([]any)
			rawSharingURL := ""
			for _, rawService := range services {
				service, _ := rawService.(map[string]any)
				if stringValue(service["name"]) == "ICLOUD_STORAGE" {
					rawSharingURL = stringValue(service["url"])
					break
				}
			}
			sharingURL, sharingErr := appleEndpoint(rawSharingURL)
			if sharingErr != nil {
				return errors.New("family subscriptions did not return a valid iCloud+ sharing URL")
			}
			sharingRequest, requestErr := newFamilyRequest(sharingURL.String(), false)
			if requestErr != nil {
				return requestErr
			}
			sharingRequest.Method = http.MethodGet
			sharingRequest.Body = nil
			sharingResponse, requestErr := client.Do(sharingRequest)
			if requestErr != nil {
				return fmt.Errorf("iCloud+ sharing UI transport failed: %w", requestErr)
			}
			sharingData, sharingReadErr := io.ReadAll(io.LimitReader(sharingResponse.Body, (2<<20)+1))
			sharingResponse.Body.Close()
			if sharingReadErr != nil || len(sharingData) > 2<<20 {
				return errors.New("iCloud+ sharing UI response is invalid")
			}
			if *iCloudSharingUIDump != "" {
				file, openErr := os.OpenFile(*iCloudSharingUIDump, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if openErr != nil {
					return errors.New("private iCloud+ sharing UI dump must be a new file")
				}
				if _, writeErr := file.Write(sharingData); writeErr != nil || file.Close() != nil {
					return errors.New("cannot preserve private iCloud+ sharing UI response")
				}
			}
			fmt.Printf("iCloud+ sharing UI inspected; HTTP=%d host=%s path=%s content-type=%q bytes=%d; mutation-submitted=false\n", sharingResponse.StatusCode, sharingURL.Hostname(), sharingURL.EscapedPath(), sharingResponse.Header.Get("Content-Type"), len(sharingData))
			for index, scriptURL := range safeFamilyUIScripts(sharingRequest.URL, sharingData) {
				scriptRequest, scriptErr := newFamilyRequest(scriptURL.String(), false)
				if scriptErr != nil {
					return scriptErr
				}
				scriptRequest.Method = http.MethodGet
				scriptRequest.Body = nil
				scriptRequest.Header.Set("Referer", sharingRequest.URL.String())
				scriptResponse, scriptErr := client.Do(scriptRequest)
				if scriptErr != nil {
					return fmt.Errorf("iCloud+ sharing script transport failed: %w", scriptErr)
				}
				scriptData, scriptReadErr := io.ReadAll(io.LimitReader(scriptResponse.Body, (2<<20)+1))
				scriptResponse.Body.Close()
				if scriptReadErr != nil || len(scriptData) > 2<<20 {
					return errors.New("iCloud+ sharing script response is invalid")
				}
				fmt.Printf("iCloud+ sharing script=%d HTTP=%d host=%s path=%s bytes=%d action-identifiers=%s; mutation-submitted=false\n", index+1, scriptResponse.StatusCode, scriptURL.Hostname(), scriptURL.EscapedPath(), len(scriptData), strings.Join(safeFamilyActionIdentifiers(scriptData), ","))
			}
		}
	}
	if *inspectStorageUI {
		storageURL, storageErr := appleEndpoint("https://setup.icloud.com/setup/family/v2/macos/storage")
		if storageErr != nil {
			return storageErr
		}
		storageRequest, requestErr := newFamilyRequest(storageURL.String(), false)
		if requestErr != nil {
			return requestErr
		}
		storageRequest.Method = http.MethodGet
		storageRequest.Body = nil
		storageResponse, requestErr := client.Do(storageRequest)
		if requestErr != nil {
			return fmt.Errorf("family storage UI transport failed: %w", requestErr)
		}
		storageData, readErr := io.ReadAll(io.LimitReader(storageResponse.Body, (2<<20)+1))
		storageResponse.Body.Close()
		if readErr != nil || len(storageData) > 2<<20 {
			return errors.New("family storage UI response is invalid")
		}
		if *storageUIDump != "" {
			file, openErr := os.OpenFile(*storageUIDump, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if openErr != nil {
				return errors.New("private storage UI dump must be a new file")
			}
			if _, writeErr := file.Write(storageData); writeErr != nil || file.Close() != nil {
				return errors.New("cannot preserve private storage UI response")
			}
		}
		forms := safeFamilyUIForms(storageRequest.URL, storageData)
		fmt.Printf("family storage UI inspected; HTTP=%d content-type=%q bytes=%d forms=%d; mutation-submitted=false\n", storageResponse.StatusCode, storageResponse.Header.Get("Content-Type"), len(storageData), len(forms))
		for index, scriptURL := range safeFamilyUIScripts(storageRequest.URL, storageData) {
			scriptRequest, scriptErr := newFamilyRequest(scriptURL.String(), false)
			if scriptErr != nil {
				return scriptErr
			}
			scriptRequest.Method = http.MethodGet
			scriptRequest.Body = nil
			scriptRequest.Header.Set("Referer", storageRequest.URL.String())
			scriptResponse, scriptErr := client.Do(scriptRequest)
			if scriptErr != nil {
				return fmt.Errorf("family storage script transport failed: %w", scriptErr)
			}
			scriptData, scriptReadErr := io.ReadAll(io.LimitReader(scriptResponse.Body, (2<<20)+1))
			scriptResponse.Body.Close()
			if scriptReadErr != nil || len(scriptData) > 2<<20 {
				return errors.New("family storage script response is invalid")
			}
			fmt.Printf("family storage script=%d HTTP=%d host=%s path=%s bytes=%d action-identifiers=%s; mutation-submitted=false\n", index+1, scriptResponse.StatusCode, scriptURL.Hostname(), scriptURL.EscapedPath(), len(scriptData), strings.Join(safeFamilyActionIdentifiers(scriptData), ","))
		}
	}
	detailsRequest, err := newFamilyRequest(detailsURL.String(), true)
	if err != nil {
		return err
	}
	detailsResponse, err := client.Do(detailsRequest)
	if err != nil {
		return fmt.Errorf("family details transport failed: %w", err)
	}
	detailsData, err := readLimited(detailsResponse)
	if err != nil {
		return err
	}
	if *dump != "" {
		file, err := os.OpenFile(*dump, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return errors.New("private family dump must be a new file")
		}
		if _, err := file.Write(detailsData); err != nil || file.Close() != nil {
			return errors.New("cannot preserve private family response")
		}
	}
	details, err := decodeObject(detailsData)
	if err != nil {
		if *createFamily {
			return errors.New("family was created but post-action family details are unavailable")
		}
		eligibilityRaw := stringValue(urls["checkFamilyEligibility"])
		if eligibilityRaw == "" {
			return errors.New("family details unavailable and eligibility endpoint missing")
		}
		eligibilityRequest, requestErr := newFamilyRequest(eligibilityRaw, false)
		if requestErr != nil {
			return requestErr
		}
		eligibilityResponse, requestErr := client.Do(eligibilityRequest)
		if requestErr != nil {
			return fmt.Errorf("family eligibility transport failed: %w", requestErr)
		}
		eligibilityData, requestErr := readLimited(eligibilityResponse)
		if requestErr != nil {
			return requestErr
		}
		eligibility, decodeErr := decodeObject(eligibilityData)
		if decodeErr != nil {
			return errors.New("family details and eligibility returned Apple UI errors")
		}
		keys := make([]string, 0, len(eligibility))
		for key := range eligibility {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		eligible, hasEligible := eligibility["eligible"].(bool)
		message := stringValue(eligibility["status-message"])
		if len(message) > 160 {
			message = message[:160]
		}
		fmt.Printf("family details unavailable; eligibility accepted; eligible-present=%t eligible=%t status-message=%q response-keys=%s; mutation-submitted=false\n", hasEligible, eligible, message, strings.Join(keys, ","))
		return nil
	}
	eligible, eligibilityKnown := false, false
	if !boolValue(details["is-member-of-family"]) {
		eligibilityRaw := stringValue(urls["checkFamilyEligibility"])
		if eligibilityRaw == "" {
			return errors.New("account has no family and eligibility endpoint is missing")
		}
		eligibilityRequest, requestErr := newFamilyRequest(eligibilityRaw, false)
		if requestErr != nil {
			return requestErr
		}
		eligibilityResponse, requestErr := client.Do(eligibilityRequest)
		if requestErr != nil {
			return fmt.Errorf("family eligibility transport failed: %w", requestErr)
		}
		eligibilityData, requestErr := readLimited(eligibilityResponse)
		if requestErr != nil {
			return requestErr
		}
		eligibility, decodeErr := decodeObject(eligibilityData)
		if decodeErr != nil {
			return errors.New("family eligibility returned an unreadable response")
		}
		eligible, eligibilityKnown = eligibility["eligible"].(bool)
		fmt.Printf("account has no current family; eligible-present=%t eligible=%t; mutation-submitted=false\n", eligibilityKnown, eligible)
	}
	members, _ := details["family-members"].([]any)
	invites, _ := details["family-invitations"].([]any)
	pending, _ := details["pending-members"].([]any)
	organizers, children, me, selfOrganizer, selfMember := summarizeFamilyMembers(members)
	if *createFamily && (len(members) != 1 || organizers != 1 || me != 1) {
		return errors.New("family creation response was not confirmed by organizer membership")
	}
	if acceptedInvitation {
		if len(members) < 2 || organizers != 1 || me != 1 || !boolValue(details["is-member-of-family"]) {
			return errors.New("family invitation acceptance was not confirmed by current membership")
		}
		storageVerified := false
		for attempt := 0; attempt < 5; attempt++ {
			storageRequest, requestErr := newFamilyRequest("https://setup.icloud.com/setup/family/v2/macos/storage", false)
			if requestErr != nil {
				return requestErr
			}
			storageRequest.Method = http.MethodGet
			storageRequest.Body = nil
			storageResponse, requestErr := client.Do(storageRequest)
			if requestErr == nil {
				storageData, readErr := readLimited(storageResponse)
				if readErr == nil {
					storage, decodeErr := decodeObject(storageData)
					if decodeErr == nil && boolValue(storage["isParticipatingInFamilyStorage"]) && boolValue(storage["familyStorageAvailable"]) {
						storageVerified = true
						break
					}
				}
			}
			time.Sleep(time.Second)
		}
		if !storageVerified {
			return errors.New("family membership succeeded but shared iCloud+ is not active for the target")
		}
		fmt.Println("family invitation acceptance verified; target-member=true shared-icloud-storage=true purchase-sharing-enabled=false")
	}
	if *removePoolMemberDir != "" {
		if organizers != 1 || me != 1 || !selfOrganizer || !boolValue(details["is-member-of-family"]) {
			return errors.New("pool member removal requires the current self organizer")
		}
		var target appleservices.Session
		targetData, targetErr := os.ReadFile(filepath.Join(*removePoolMemberDir, "session.json"))
		if targetErr != nil || json.Unmarshal(targetData, &target) != nil || target.AppleID == "" || strings.EqualFold(target.AppleID, saved.AppleID) {
			return errors.New("target pool account session is unavailable or invalid")
		}
		targetDSID := ""
		targetMatches := 0
		for _, raw := range members {
			member, _ := raw.(map[string]any)
			if !strings.EqualFold(familyMemberAppleID(member), target.AppleID) {
				continue
			}
			targetMatches++
			if boolValue(member["is-me"]) || boolValue(member["member-is-organizer"]) {
				return errors.New("refusing to remove self or organizer")
			}
			if boolValue(member["member-is-child-account"]) {
				return errors.New("child-account removal is unsupported")
			}
			targetDSID = numericIDValue(member["member-dsid"])
		}
		if targetMatches != 1 || targetDSID == "" {
			return errors.New("target pool member is not uniquely bound to the current family")
		}
		removeURL, removeErr := appleEndpoint(stringValue(urls["removeFromFamily"]))
		if removeErr != nil {
			return errors.New("Apple bag did not return a valid family removal endpoint")
		}
		removeBody, encodeErr := plist.Marshal(map[string]any{"member-dsid": targetDSID}, plist.XMLFormat)
		targetDSID = ""
		if encodeErr != nil {
			return errors.New("cannot encode bound family removal request")
		}
		removeRequest, requestErr := newFamilyRequest(removeURL.String(), false)
		if requestErr != nil {
			return requestErr
		}
		removeRequest.Method = http.MethodPost
		removeRequest.Body = io.NopCloser(bytes.NewReader(removeBody))
		removeRequest.ContentLength = int64(len(removeBody))
		removeRequest.Header.Set("Content-Type", "application/x-plist")
		removeRequest.Header.Set("Accept", "application/json, text/plist, application/x-plist")
		removeResponse, removeRequestErr := apiClient.Do(removeRequest)
		for index := range removeBody {
			removeBody[index] = 0
		}
		if removeRequestErr == nil {
			removeData, readErr := io.ReadAll(io.LimitReader(removeResponse.Body, (2<<20)+1))
			removeResponse.Body.Close()
			if readErr != nil || len(removeData) > 2<<20 || removeResponse.StatusCode != http.StatusOK {
				return fmt.Errorf("family member removal returned HTTP %d", removeResponse.StatusCode)
			}
			if len(bytes.TrimSpace(removeData)) > 0 {
				removed, decodeErr := decodeObject(removeData)
				if decodeErr == nil && familyResponseError(removed) {
					return errors.New("Apple returned an application error for family member removal")
				}
			}
		}
		var verified map[string]any
		confirmed := false
		for attempt := 0; attempt < 4 && !confirmed; attempt++ {
			verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 12*time.Second)
			verifyRequest, verifyErr := newFamilyRequest(detailsURL.String(), true)
			if verifyErr == nil {
				verifyRequest = verifyRequest.Clone(verifyCtx)
				if verifyResponse, doErr := apiClient.Do(verifyRequest); doErr == nil {
					if verifyData, readErr := readLimited(verifyResponse); readErr == nil {
						if value, decodeErr := decodeObject(verifyData); decodeErr == nil && !familyResponseError(value) {
							present := false
							verifiedMembers, _ := value["family-members"].([]any)
							for _, rawMember := range verifiedMembers {
								member, _ := rawMember.(map[string]any)
								if strings.EqualFold(familyMemberAppleID(member), target.AppleID) {
									present = true
								}
							}
							if !present && len(verifiedMembers) == len(members)-1 {
								verified, confirmed = value, true
							}
						}
					}
				}
			}
			verifyCancel()
			if !confirmed {
				time.Sleep(400 * time.Millisecond)
			}
		}
		if !confirmed {
			if removeRequestErr != nil {
				return fmt.Errorf("family member removal transport failed and current family still includes the target: %w", removeRequestErr)
			}
			return errors.New("family member removal was not confirmed by current organizer state")
		}
		details = verified
		members, _ = details["family-members"].([]any)
		invites, _ = details["family-invitations"].([]any)
		pending, _ = details["pending-members"].([]any)
		organizers, children, me, selfOrganizer, selfMember = summarizeFamilyMembers(members)
		fmt.Printf("bound pool member removal verified; members=%d purchase-sharing-enabled=false location-sharing-enabled=false\n", len(members))
	}
	if *verifyPoolMemberDir != "" {
		var target appleservices.Session
		data, err := os.ReadFile(filepath.Join(*verifyPoolMemberDir, "session.json"))
		if err != nil || json.Unmarshal(data, &target) != nil || !selfOrganizer || !familyContainsAccount(details, target.AppleID) {
			return errors.New("exact pool member is not confirmed in this organizer family")
		}
	}
	if *addPoolMemberDir != "" {
		if organizers != 1 || me != 1 || !selfOrganizer || !boolValue(details["is-member-of-family"]) {
			return errors.New("pool member addition requires the current self organizer")
		}
		var target appleservices.Session
		targetData, targetErr := os.ReadFile(filepath.Join(*addPoolMemberDir, "session.json"))
		if targetErr != nil || json.Unmarshal(targetData, &target) != nil || target.AppleID == "" || strings.EqualFold(target.AppleID, saved.AppleID) {
			return errors.New("target pool account session is unavailable or invalid")
		}
		var targetAccepted acceptedCheckpoint
		targetAcceptedData, targetErr := os.ReadFile(filepath.Join(*addPoolMemberDir, "icloud-accepted.json"))
		if targetErr != nil || json.Unmarshal(targetAcceptedData, &targetAccepted) != nil || !strings.EqualFold(targetAccepted.Session.AppleID, target.AppleID) || targetAccepted.Session.Tokens["mmeAuthToken"] == "" {
			return errors.New("target pool account is not independently iCloud-activated")
		}
		if familyContainsAccount(details, target.AppleID) {
			return errors.New("target is already in this family; use read-only verification")
		}
		if len(members)+len(invites)+len(pending) >= 6 {
			return errors.New("family is full; maximum is organizer plus five members")
		}
		targetCredential := target.Tokens["com.apple.gs.idms.pet"]
		credentialSource := "pet"
		if *targetPasswordEnv {
			rawPassword := os.Getenv("APPLESERVICES_TARGET_PASSWORD")
			_ = os.Unsetenv("APPLESERVICES_TARGET_PASSWORD")
			if rawPassword == "" {
				return errors.New("target password environment is empty")
			}
			targetCredential, targetErr = ephemeralFamilyPET(ctx, native, hardware.Push.Token, *addPoolMemberDir, target, rawPassword)
			rawPassword = ""
			if targetErr != nil {
				return targetErr
			}
			credentialSource = "pet"
		}
		if targetCredential == "" || len(targetCredential) > 16<<10 {
			return errors.New("target pool account has no usable family credential")
		}
		checkURL, checkErr := appleEndpoint("https://setup.icloud.com/setup/family/checkiCloudMember")
		if checkErr != nil {
			return checkErr
		}
		query := checkURL.Query()
		query.Set("email-address", target.AppleID)
		checkURL.RawQuery = query.Encode()
		checkRequest, requestErr := newFamilyRequest(checkURL.String(), false)
		if requestErr != nil {
			return requestErr
		}
		checkRequest.Method = http.MethodGet
		checkRequest.Body = nil
		checkResponse, requestErr := apiClient.Do(checkRequest)
		if requestErr != nil {
			return fmt.Errorf("pool member target check transport failed: %w", requestErr)
		}
		checkData, responseErr := io.ReadAll(io.LimitReader(checkResponse.Body, (2<<20)+1))
		checkResponse.Body.Close()
		if responseErr != nil || len(checkData) > 2<<20 || checkResponse.StatusCode != http.StatusOK {
			return errors.New("Apple did not accept the pool member target check")
		}
		checked, decodeErr := decodeObject(checkData)
		if decodeErr != nil || familyResponseError(checked) {
			return errors.New("pool member target check returned an application error")
		}
		validICloud, validPresent := checked["is-valid-icloud-account"].(bool)
		termsAccepted, termsPresent := checked["terms-accepted"].(bool)
		skipPayment, paymentPresent := checked["skipPaymentVerification"].(bool)
		pvkEnabled, _ := checked["isPVKEnabled"].(bool)
		if !validPresent || !validICloud || !termsPresent || !termsAccepted || !paymentPresent || !skipPayment || pvkEnabled {
			return fmt.Errorf("pool member requires unsupported target verification; valid=%t terms=%t skip-payment=%t pvk=%t", validICloud, termsAccepted, skipPayment, pvkEnabled)
		}
		addBody, encodeErr := json.Marshal(map[string]any{
			"appleId":                        target.AppleID,
			"password":                       targetCredential,
			"credentialSource":               credentialSource,
			"shareMyLocationEnabledDefault":  false,
			"shareMyPurchasesEnabledDefault": false,
			"appleIdForPurchases":            target.AppleID,
			"preferredAppleId":               target.AppleID,
		})
		targetCredential = ""
		if encodeErr != nil {
			return errors.New("cannot encode PET-bound pool member request")
		}
		addURL, addErr := appleEndpoint("https://setup.icloud.com/setup/mac/family/addFamilyMember")
		if addErr != nil {
			return addErr
		}
		addRequest, requestErr := newFamilyRequest(addURL.String(), false)
		if requestErr != nil {
			return requestErr
		}
		addRequest.Method = http.MethodPost
		addRequest.Body = io.NopCloser(bytes.NewReader(addBody))
		addRequest.ContentLength = int64(len(addBody))
		addRequest.Header.Set("Content-Type", "application/json")
		addRequest.Header.Set("Accept", "application/json; charset=UTF-8")
		addResponse, requestErr := apiClient.Do(addRequest)
		for index := range addBody {
			addBody[index] = 0
		}
		if requestErr != nil {
			addResponse = nil
		}
		if addResponse != nil {
			addData, responseErr := io.ReadAll(io.LimitReader(addResponse.Body, (2<<20)+1))
			addResponse.Body.Close()
			if responseErr != nil || len(addData) > 2<<20 {
				return errors.New("PET-bound pool member response is invalid")
			}
			if addResponse.StatusCode != http.StatusOK {
				failed, decodeErr := decodeObject(addData)
				if decodeErr == nil {
					return fmt.Errorf("PET-bound pool member addition returned HTTP %d response-keys=%s %s", addResponse.StatusCode, strings.Join(sortedMapKeys(failed), ","), safeFamilyStatus(failed))
				}
				return fmt.Errorf("PET-bound pool member addition returned HTTP %d bytes=%d", addResponse.StatusCode, len(addData))
			}
			added, decodeErr := decodeObject(addData)
			if decodeErr != nil || familyResponseError(added) {
				return fmt.Errorf("Apple did not confirm PET-bound pool member addition; response-keys=%s %s", strings.Join(sortedMapKeys(added), ","), safeFamilyStatus(added))
			}
			addedMembers, _ := added["familyMembers"].([]any)
			if len(addedMembers) == 0 {
				addedMembers, _ = added["family-members"].([]any)
			}
			targetPresent := false
			for _, rawMember := range addedMembers {
				member, _ := rawMember.(map[string]any)
				memberAppleID := stringValue(member["appleId"])
				if memberAppleID == "" {
					memberAppleID = stringValue(member["member-apple-id"])
				}
				if strings.EqualFold(memberAppleID, target.AppleID) {
					targetPresent = true
				}
			}
			if !targetPresent {
				return errors.New("Apple response did not include the bound pool member")
			}
			fmt.Printf("PET-bound pool member addition accepted; HTTP=%d response-keys=%s purchase-sharing-enabled=false location-sharing-enabled=false\n", addResponse.StatusCode, strings.Join(sortedMapKeys(added), ","))
		}
		// Reconcile both a successful response and an indeterminate transport
		// against current details, then use that snapshot for the final summary.
		readCtx, readCancel := context.WithTimeout(context.Background(), 32*time.Second)
		verified, verifyErr := readAddedFamilyMember(readCtx, apiClient, func() (*http.Request, error) {
			return newFamilyRequest(detailsURL.String(), true)
		}, target.AppleID)
		readCancel()
		if verifyErr != nil {
			return verifyErr
		}
		details = verified
		members, _ = details["family-members"].([]any)
		invites, _ = details["family-invitations"].([]any)
		pending, _ = details["pending-members"].([]any)
		organizers, children, me, selfOrganizer, selfMember = summarizeFamilyMembers(members)

	}
	if *inviteAccountDir != "" {
		if organizers != 1 || me != 1 || !boolValue(details["is-member-of-family"]) {
			return errors.New("family invitation requires the current self organizer")
		}
		var target appleservices.Session
		targetData, targetErr := os.ReadFile(filepath.Join(*inviteAccountDir, "session.json"))
		if targetErr != nil || json.Unmarshal(targetData, &target) != nil || target.AppleID == "" || strings.EqualFold(target.AppleID, saved.AppleID) {
			return errors.New("target pool account session is unavailable or invalid")
		}
		var targetAccepted acceptedCheckpoint
		targetAcceptedData, targetErr := os.ReadFile(filepath.Join(*inviteAccountDir, "icloud-accepted.json"))
		if targetErr != nil || json.Unmarshal(targetAcceptedData, &targetAccepted) != nil || !strings.EqualFold(targetAccepted.Session.AppleID, target.AppleID) || targetAccepted.Session.Tokens["mmeAuthToken"] == "" {
			return errors.New("target pool account is not independently iCloud-activated")
		}
		checkURL, checkErr := appleEndpoint(stringValue(urls["checkFamilyMember"]))
		if checkErr != nil {
			checkURL, checkErr = appleEndpoint("https://setup.icloud.com/setup/family/checkiCloudMember")
		}
		if checkErr != nil {
			return checkErr
		}
		query := checkURL.Query()
		query.Set("email-address", target.AppleID)
		checkURL.RawQuery = query.Encode()
		checkRequest, requestErr := newFamilyRequest(checkURL.String(), false)
		if requestErr != nil {
			return requestErr
		}
		checkRequest.Method = http.MethodGet
		checkRequest.Body = nil
		checkResponse, requestErr := client.Do(checkRequest)
		if requestErr != nil {
			return fmt.Errorf("family invite target check transport failed: %w", requestErr)
		}
		checkData, responseErr := io.ReadAll(io.LimitReader(checkResponse.Body, (2<<20)+1))
		checkResponse.Body.Close()
		if responseErr != nil || len(checkData) > 2<<20 || checkResponse.StatusCode != http.StatusOK {
			return errors.New("Apple did not accept the family invite target check")
		}
		checked, decodeErr := decodeObject(checkData)
		if decodeErr != nil {
			return errors.New("family invite target check returned an unreadable response")
		}
		if eligible, ok := checked["eligible"].(bool); ok && !eligible {
			return errors.New("Apple reports the target account is not eligible for this family")
		}
		validICloud, validPresent := checked["is-valid-icloud-account"].(bool)
		termsAccepted, termsPresent := checked["terms-accepted"].(bool)
		if !validPresent || !validICloud || !termsPresent || !termsAccepted || familyResponseError(checked) {
			return fmt.Errorf("Apple reports the target account cannot be invited; valid-present=%t valid=%t terms-present=%t terms-accepted=%t %s", validPresent, validICloud, termsPresent, termsAccepted, safeFamilyStatus(checked))
		}

		buildURL, buildErr := appleEndpoint("https://setup.icloud.com/setup/familykit/macos/buildInvite")
		if buildErr != nil {
			return buildErr
		}
		// handleInvite forwards already-selected recipients to buildInvite.
		// The native share sheet uses an empty list only when it is responsible
		// for choosing recipients interactively; this pool flow has one bound
		// target before the share context is built.
		buildBody, encodeErr := json.Marshal(map[string]any{"inviteRecipients": []string{target.AppleID}})
		if encodeErr != nil {
			return errors.New("cannot encode family invitation build")
		}
		buildRequest, requestErr := newFamilyRequest(buildURL.String(), false)
		if requestErr != nil {
			return requestErr
		}
		buildRequest.Method = http.MethodPost
		buildRequest.Body = io.NopCloser(bytes.NewReader(buildBody))
		buildRequest.ContentLength = int64(len(buildBody))
		buildRequest.Header.Set("Content-Type", "application/json")
		buildRequest.Header.Set("Accept", "application/json; charset=UTF-8")
		buildResponse, requestErr := client.Do(buildRequest)
		if requestErr != nil {
			return fmt.Errorf("family invitation build transport failed: %w", requestErr)
		}
		buildData, responseErr := io.ReadAll(io.LimitReader(buildResponse.Body, (2<<20)+1))
		buildResponse.Body.Close()
		if responseErr != nil || len(buildData) > 2<<20 {
			return errors.New("family invitation build response is invalid")
		}
		if buildResponse.StatusCode != http.StatusOK {
			return fmt.Errorf("family invitation build returned HTTP %d", buildResponse.StatusCode)
		}
		built, decodeErr := decodeObject(buildData)
		if decodeErr != nil || familyResponseError(built) {
			return errors.New("Apple did not confirm the family invitation build")
		}
		inviteCode := findStringValue(built, "inviteCode")
		if inviteCode == "" || len(inviteCode) > 4096 {
			return errors.New("Apple did not return a usable family invitation code")
		}
		registerURL, registerErr := appleEndpoint("https://setup.icloud.com/setup/familykit/macos/registerInviteSent")
		if registerErr != nil {
			return registerErr
		}
		registerBody, encodeErr := json.Marshal(map[string]any{
			"inviteRecipients": []string{target.AppleID},
			"completionStatus": "success",
			"transportMode":    "messages",
			"inviteCode":       inviteCode,
		})
		if encodeErr != nil {
			return errors.New("cannot encode family invitation registration")
		}
		registerRequest, requestErr := newFamilyRequest(registerURL.String(), false)
		if requestErr != nil {
			return requestErr
		}
		registerRequest.Method = http.MethodPost
		registerRequest.Body = io.NopCloser(bytes.NewReader(registerBody))
		registerRequest.ContentLength = int64(len(registerBody))
		registerRequest.Header.Set("Content-Type", "application/json")
		registerRequest.Header.Set("Accept", "application/json; charset=UTF-8")
		registerResponse, requestErr := apiClient.Do(registerRequest)
		if requestErr != nil {
			return fmt.Errorf("family invitation registration transport failed: %w", requestErr)
		}
		registerData, responseErr := io.ReadAll(io.LimitReader(registerResponse.Body, (2<<20)+1))
		registerResponse.Body.Close()
		if responseErr != nil || len(registerData) > 2<<20 {
			return errors.New("family invitation registration response is invalid")
		}
		if registerResponse.StatusCode != http.StatusOK {
			diagnosticPath := *inviteOut + ".register-response"
			file, openErr := os.OpenFile(diagnosticPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if openErr == nil {
				_, _ = file.Write(registerData)
				_ = file.Close()
			}
			return fmt.Errorf("family invitation registration returned HTTP %d bytes=%d private-diagnostic-written=%t", registerResponse.StatusCode, len(registerData), openErr == nil)
		}
		registered, decodeErr := decodeObject(registerData)
		if decodeErr != nil || familyResponseError(registered) {
			return errors.New("Apple did not confirm family invitation registration")
		}
		checkpoint := familyInviteCheckpoint{TargetHash: accountBindingHash(target.AppleID), InviteCode: inviteCode, CreatedAt: time.Now().UTC()}
		checkpointData, _ := json.Marshal(checkpoint)
		file, openErr := os.OpenFile(*inviteOut, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if openErr != nil {
			return errors.New("private family invitation checkpoint must be a new file")
		}
		if _, writeErr := file.Write(checkpointData); writeErr != nil || file.Close() != nil {
			return errors.New("cannot preserve private family invitation checkpoint")
		}
		inviteCode = ""
		fmt.Printf("family invitation built and registered for one bound pool account; build-HTTP=%d register-HTTP=%d target-check-keys=%s build-keys=%s register-keys=%s private-checkpoint-written=true\n", buildResponse.StatusCode, registerResponse.StatusCode, strings.Join(sortedMapKeys(checked), ","), strings.Join(sortedMapKeys(built), ","), strings.Join(sortedMapKeys(registered), ","))
	}
	if *shareICloudStorage {
		familyID := stringValue(details["family-id"])
		organizerDSID := ""
		for _, raw := range members {
			member, _ := raw.(map[string]any)
			if boolValue(member["is-me"]) && boolValue(member["member-is-organizer"]) {
				organizerDSID = numericIDValue(member["member-dsid"])
			}
		}
		if familyID == "" || organizerDSID == "" || organizers != 1 || me != 1 {
			return errors.New("iCloud+ sharing requires the current self organizer and family binding")
		}
		updateURL, updateErr := appleEndpoint("https://setup.icloud.com/setup/family/v2/macos/updateStorage")
		if updateErr != nil {
			return updateErr
		}
		updateBody, encodeErr := json.Marshal(map[string]any{
			"action":        "START_FAMILY_SHARING",
			"path":          "APP_ICLOUD_STORAGE",
			"familyId":      familyID,
			"organizerDsId": organizerDSID,
		})
		if encodeErr != nil {
			return errors.New("cannot encode iCloud+ sharing request")
		}
		updateRequest, requestErr := newFamilyRequest(updateURL.String(), false)
		if requestErr != nil {
			return requestErr
		}
		updateRequest.Method = http.MethodPost
		updateRequest.Body = io.NopCloser(bytes.NewReader(updateBody))
		updateRequest.ContentLength = int64(len(updateBody))
		updateRequest.Header.Set("Content-Type", "application/json")
		updateRequest.Header.Set("Accept", "application/json; charset=UTF-8")
		updateResponse, requestErr := client.Do(updateRequest)
		if requestErr != nil {
			return fmt.Errorf("iCloud+ sharing transport failed: %w", requestErr)
		}
		updateData, readErr := io.ReadAll(io.LimitReader(updateResponse.Body, (2<<20)+1))
		updateResponse.Body.Close()
		if readErr != nil || len(updateData) > 2<<20 {
			return errors.New("iCloud+ sharing response is invalid")
		}
		if updateResponse.StatusCode != http.StatusOK {
			return fmt.Errorf("iCloud+ sharing returned HTTP %d", updateResponse.StatusCode)
		}
		updateResult, decodeErr := decodeObject(updateData)
		if decodeErr != nil {
			return errors.New("iCloud+ sharing returned an unreadable response")
		}
		if success, ok := updateResult["success"].(bool); ok && !success {
			return errors.New("Apple did not confirm iCloud+ sharing")
		}

		storageRequest, requestErr := newFamilyRequest("https://setup.icloud.com/setup/family/v2/macos/storage", false)
		if requestErr != nil {
			return requestErr
		}
		storageRequest.Method = http.MethodGet
		storageRequest.Body = nil
		storageResponse, requestErr := client.Do(storageRequest)
		if requestErr != nil {
			return fmt.Errorf("iCloud+ sharing verification transport failed: %w", requestErr)
		}
		storageData, readErr := readLimited(storageResponse)
		if readErr != nil {
			return readErr
		}
		storage, decodeErr := decodeObject(storageData)
		if decodeErr != nil || !boolValue(storage["isParticipatingInFamilyStorage"]) || !boolValue(storage["isEnabled"]) {
			return errors.New("iCloud+ sharing was not confirmed by current storage state")
		}

		subscriptionRequest, requestErr := newFamilyRequest(stringValue(urls["getFamilySubscriptions"]), false)
		if requestErr != nil {
			return requestErr
		}
		subscriptionRequest.Method = http.MethodGet
		subscriptionRequest.Body = nil
		subscriptionResponse, requestErr := client.Do(subscriptionRequest)
		if requestErr != nil {
			return fmt.Errorf("family subscription verification transport failed: %w", requestErr)
		}
		subscriptionData, readErr := readLimited(subscriptionResponse)
		if readErr != nil {
			return readErr
		}
		subscriptions, decodeErr := decodeObject(subscriptionData)
		if decodeErr != nil {
			return errors.New("family subscription verification returned an unreadable response")
		}
		storageShared := false
		services, _ := subscriptions["familySharedServices"].([]any)
		for _, rawService := range services {
			service, _ := rawService.(map[string]any)
			if stringValue(service["name"]) == "ICLOUD_STORAGE" && stringValue(service["groupID"]) == "1" {
				storageShared = true
			}
		}
		if !storageShared {
			return errors.New("iCloud+ sharing was not confirmed by family subscriptions")
		}
		fmt.Printf("iCloud+ family storage sharing verified; HTTP=%d participating=true enabled=true subscriptions-group=shared\n", updateResponse.StatusCode)
	}
	if *joinSharedStorage {
		if !boolValue(details["is-member-of-family"]) || me != 1 {
			return errors.New("shared storage join requires current family membership")
		}
		familyID := stringValue(details["family-id"])
		organizerDSID := ""
		selfOrganizer := false
		for _, raw := range members {
			member, _ := raw.(map[string]any)
			if boolValue(member["member-is-organizer"]) {
				organizerDSID = numericIDValue(member["member-dsid"])
				if boolValue(member["is-me"]) {
					selfOrganizer = true
				}
			}
		}
		if familyID == "" || organizerDSID == "" || organizers != 1 || selfOrganizer {
			return errors.New("shared storage join requires one different current organizer")
		}
		updateBody, encodeErr := json.Marshal(map[string]any{
			"action":        "JOIN_FAMILY",
			"path":          "APP_ICLOUD_STORAGE",
			"familyId":      familyID,
			"organizerDsId": organizerDSID,
		})
		if encodeErr != nil {
			return errors.New("cannot encode shared storage join")
		}
		updateRequest, requestErr := newFamilyRequest("https://setup.icloud.com/setup/family/v2/macos/updateStorage", false)
		if requestErr != nil {
			return requestErr
		}
		updateRequest.Method = http.MethodPost
		updateRequest.Body = io.NopCloser(bytes.NewReader(updateBody))
		updateRequest.ContentLength = int64(len(updateBody))
		updateRequest.Header.Set("Content-Type", "application/json")
		updateRequest.Header.Set("Accept", "application/json; charset=UTF-8")
		updateResponse, requestErr := apiClient.Do(updateRequest)
		if requestErr != nil {
			return fmt.Errorf("shared storage join transport failed: %w", requestErr)
		}
		updateData, responseErr := io.ReadAll(io.LimitReader(updateResponse.Body, (2<<20)+1))
		updateResponse.Body.Close()
		if responseErr != nil || len(updateData) > 2<<20 || updateResponse.StatusCode != http.StatusOK {
			return fmt.Errorf("shared storage join returned HTTP %d", updateResponse.StatusCode)
		}
		updated, decodeErr := decodeObject(updateData)
		if decodeErr != nil || familyResponseError(updated) {
			return errors.New("Apple did not confirm shared storage join")
		}
		verifyRequest, requestErr := newFamilyRequest("https://setup.icloud.com/setup/family/v2/macos/storage", false)
		if requestErr != nil {
			return requestErr
		}
		verifyRequest.Method = http.MethodGet
		verifyRequest.Body = nil
		verifyResponse, requestErr := apiClient.Do(verifyRequest)
		if requestErr != nil {
			return fmt.Errorf("shared storage verification transport failed: %w", requestErr)
		}
		verifyData, responseErr := readLimited(verifyResponse)
		if responseErr != nil {
			return responseErr
		}
		storage, decodeErr := decodeObject(verifyData)
		if decodeErr != nil || !boolValue(storage["isParticipatingInFamilyStorage"]) || !boolValue(storage["familyStorageAvailable"]) || !boolValue(storage["isEnabled"]) {
			return errors.New("shared iCloud+ plan was not confirmed for the current member")
		}
		fmt.Printf("shared iCloud+ member join verified; HTTP=%d participating=true available=true enabled=true\n", updateResponse.StatusCode)
	}
	if *safeJSON {
		summary := safeFamilySummary{
			Member:           boolValue(details["is-member-of-family"]),
			Eligible:         eligible,
			EligibilityKnown: eligibilityKnown,
			Members:          len(members),
			Organizers:       organizers,
			Self:             me,
			Children:         children,
			Invitations:      len(invites),
			Pending:          len(pending),
			Organizer:        selfOrganizer,
		}
		if selfMember != nil {
			summary.PurchaseSharingEnabled, summary.PurchaseSharingKnown = findFamilyBool(selfMember, func(key string) bool {
				return strings.Contains(key, "purchasesharing") || strings.Contains(key, "sharingpurchases") || strings.Contains(key, "sharemypurchases")
			})
			summary.LocationSharingEnabled, summary.LocationSharingKnown = findFamilyBool(selfMember, func(key string) bool {
				return strings.Contains(key, "locationsharing") || strings.Contains(key, "sharinglocation") || strings.Contains(key, "sharemylocation")
			})
		}
		if summary.Member {
			storageCtx, storageCancel := context.WithTimeout(context.Background(), 12*time.Second)
			storageRequest, storageErr := newFamilyRequest("https://setup.icloud.com/setup/family/v2/macos/storage", false)
			if storageErr == nil {
				storageRequest = storageRequest.Clone(storageCtx)
				storageRequest.Method = http.MethodGet
				storageRequest.Body = nil
				if storageResponse, requestErr := apiClient.Do(storageRequest); requestErr == nil {
					if storageData, readErr := readLimited(storageResponse); readErr == nil {
						if storage, decodeErr := decodeObject(storageData); decodeErr == nil && !familyResponseError(storage) {
							summary.StorageKnown = true
							summary.StorageParticipating = boolValue(storage["isParticipatingInFamilyStorage"])
							summary.StorageAvailable = boolValue(storage["familyStorageAvailable"])
							summary.StorageEnabled = boolValue(storage["isEnabled"])
							summary.StorageBytes = findFamilyInteger(storage, "storageInBytes")
						}
					}
				}
			}
			storageCancel()
		}
		encoded, encodeErr := json.Marshal(summary)
		if encodeErr != nil {
			return errors.New("cannot encode redacted family status")
		}
		fmt.Println(safeFamilyStatusPrefix + string(encoded))
	}
	fmt.Printf("family details accepted; members=%d organizers=%d self=%d children=%d invitations=%d pending=%d; mutation-submitted=%t\n", len(members), organizers, me, children, len(invites), len(pending), *createFamily || *shareICloudStorage || *joinSharedStorage || *addPoolMemberDir != "" || *removePoolMemberDir != "" || *inviteAccountDir != "" || acceptedInvitation)
	if *createFamily {
		fmt.Println("family creation verified; organizer=self members=1 invitations=0")
	}
	return nil
}

func sortedMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func accountBindingHash(appleID string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(appleID))))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func localFamilyVerificationCode(targetDir string) (string, error) {
	var device appleservices.Device
	data, err := os.ReadFile(filepath.Join(targetDir, "device.json"))
	if err != nil || json.Unmarshal(data, &device) != nil || len(device.Identifier) != 16 || device.SerialNumber == "" {
		return "", errors.New("target local device identity is unavailable")
	}
	var hardware struct {
		Push struct {
			Token []byte `plist:"token"`
		} `plist:"push"`
	}
	profile := filepath.Join(targetDir, "native-adi")
	data, err = os.ReadFile(filepath.Join(profile, "hw_info.plist"))
	if err != nil {
		return "", errors.New("target local APS identity is unavailable")
	}
	if _, err = plist.Unmarshal(data, &hardware); err != nil || len(hardware.Push.Token) != 32 {
		return "", errors.New("target local APS identity is unavailable")
	}
	library, _ := filepath.Abs(".cache/openbubbles-store/rust_lib_bluebubbles.dll")
	client, err := nativeadi.NewClient(nativeadi.Config{LibraryPath: library, ProfileDir: profile, Python: "python", Identifier: device.Identifier, Serial: device.SerialNumber, PushToken: hardware.Push.Token})
	if err != nil {
		return "", err
	}
	codeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Start(codeCtx); err != nil {
		return "", err
	}
	defer client.Close()
	code, err := client.Code(codeCtx)
	if err != nil || len(code) != 6 || strings.IndexFunc(code, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return "", errors.New("target local verification code is unavailable")
	}
	return code, nil
}

func ephemeralFamilyPET(ctx context.Context, native *nativeadi.Client, pushToken []byte, targetDir string, target appleservices.Session, password string) (string, error) {
	cookies := map[string][]gsa.CookieKV{}
	for _, cookie := range target.Cookies {
		cookies[cookie.URL] = append(cookies[cookie.URL], gsa.CookieKV{Name: cookie.Name, Value: cookie.Value})
	}
	client := gsa.NewClientFromSession(native, gsa.Session{DSID: target.DSID, Cookies: cookies, Tokens: target.Tokens})
	client.EnableNativeDeviceAuthentication()
	if err := client.SetPushToken(pushToken); err != nil {
		return "", err
	}
	result, err := client.Login(target.AppleID, password)
	password = ""
	if err != nil {
		return "", errors.New("ephemeral FamilyKit authentication failed")
	}
	if result == nil || result.SessionPayload == nil {
		return "", errors.New("ephemeral FamilyKit authentication returned no session")
	}
	if result.NeedsTwoFactor {
		adsid, _ := result.SessionPayload["adsid"].(string)
		idms, _ := result.SessionPayload["GsIdmsToken"].(string)
		if adsid == "" || idms == "" {
			return "", errors.New("ephemeral FamilyKit challenge is missing account binding")
		}
		requestPath := filepath.Join(targetDir, "family-code.request")
		codePath := filepath.Join(targetDir, "family-code.handoff")
		_ = os.Remove(requestPath)
		_ = os.Remove(codePath)
		defer os.Remove(requestPath)
		defer os.Remove(codePath)
		if err := client.RequestTrustedDeviceCode(adsid, idms); err != nil {
			return "", errors.New("ephemeral FamilyKit trusted-device request failed")
		}
		if err := os.WriteFile(requestPath, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0600); err != nil {
			return "", errors.New("cannot announce ephemeral FamilyKit challenge")
		}
		challengeAt := time.Now()
		code, localCodeErr := localFamilyVerificationCode(targetDir)
		if localCodeErr == nil {
			fmt.Println("ephemeral FamilyKit challenge received a fresh code from the target's bound local Mac identity")
		}
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for code == "" {
			select {
			case <-ctx.Done():
				return "", errors.New("ephemeral FamilyKit verification code wait expired")
			case <-ticker.C:
				info, statErr := os.Lstat(codePath)
				if errors.Is(statErr, os.ErrNotExist) {
					continue
				}
				if statErr != nil || !info.Mode().IsRegular() || info.ModTime().Before(challengeAt.Add(-time.Second)) || time.Since(info.ModTime()) > time.Minute {
					_ = os.Remove(codePath)
					continue
				}
				data, readErr := os.ReadFile(codePath)
				_ = os.Remove(codePath)
				candidate := strings.TrimSpace(string(data))
				if readErr != nil || len(candidate) != 6 || strings.IndexFunc(candidate, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
					return "", errors.New("ephemeral FamilyKit verification handoff is invalid")
				}
				code = candidate
			}
		}
		if err := client.SubmitTrustedDeviceCode(adsid, idms, code); err != nil {
			code = ""
			return "", errors.New("ephemeral FamilyKit verification code was rejected")
		}
		code = ""
		result = client.VerifiedLoginResult()
		if result == nil || result.NeedsTwoFactor || result.SessionPayload == nil {
			return "", errors.New("ephemeral FamilyKit verification did not complete")
		}
	}
	pet, err := icloud.PETFromSPD(result.SessionPayload)
	if err != nil || pet == "" {
		return "", errors.New("ephemeral FamilyKit authentication returned no PET")
	}
	return pet, nil
}

func findStringValue(value any, key string) string {
	switch current := value.(type) {
	case map[string]any:
		if found := stringValue(current[key]); found != "" {
			return found
		}
		for _, child := range current {
			if found := findStringValue(child, key); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range current {
			if found := findStringValue(child, key); found != "" {
				return found
			}
		}
	}
	return ""
}

func familyResponseError(values map[string]any) bool {
	for key, value := range values {
		lowerKey := strings.ToLower(key)
		if strings.Contains(lowerKey, "error") || strings.HasPrefix(lowerKey, "invalid-") {
			return true
		}
		if lowerKey == "success" {
			if success, ok := value.(bool); ok && !success {
				return true
			}
		}
		if lowerKey == "status-message" {
			message := strings.ToLower(stringValue(value))
			if strings.Contains(message, "invalid") || strings.Contains(message, "error") || strings.Contains(message, "fail") {
				return true
			}
		}
	}
	return false
}

func safeFamilyStatus(values map[string]any) string {
	parts := make([]string, 0, 2)
	for _, key := range []string{"status", "status-message"} {
		value, ok := values[key]
		if !ok {
			continue
		}
		text := strings.TrimSpace(fmt.Sprint(value))
		if len(text) > 160 || strings.Contains(text, "@") {
			continue
		}
		parts = append(parts, key+"="+text)
	}
	return strings.Join(parts, ",")
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
