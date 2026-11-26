package dashboard

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed resource_probe.cjs
var resourceProbe string

type resourceSettings struct {
	LocalBrowserProxy       string `json:"localBrowserProxy"`
	MailProvider            string `json:"mailProvider"`
	CredentialFile          string `json:"credentialFile"`
	OrderFile               string `json:"orderFile"`
	SMSProvider             string `json:"smsProvider"`
	PhoneCountry            string `json:"phoneCountry"`
	DurationHours           int    `json:"durationHours"`
	AcceptRentalPriceChange bool   `json:"acceptRentalPriceChange"`
	ProxyProfile            string `json:"proxyProfile"`
	ProxyPort               int    `json:"proxyPort"`
}

type otpGmailOrder struct {
	Provider       string `json:"provider"`
	OrderID        string `json:"orderId"`
	Email          string `json:"email"`
	Warehouse      int    `json:"warehouse"`
	CredentialFile string `json:"credentialFile"`
}

func (s *Server) registerConfigFile() string {
	if file := os.Getenv("APPLESERVICES_REGISTER_CONFIG"); file != "" {
		return file
	}
	return filepath.Join(s.config.Dir, "registration-config.json")
}

func (s *Server) resourceDefaults(settings *registerSettings) {
	r := &settings.Resources
	if r.MailProvider == "" {
		r.MailProvider = "otpgmail"
	}
	if r.CredentialFile == "" {
		r.CredentialFile = filepath.Join(s.config.RepoRoot, ".cache", "otpgmail", "api-key.dpapi")
	}
	if r.SMSProvider == "" {
		r.SMSProvider = "hero"
	}
	if r.PhoneCountry == "" {
		r.PhoneCountry = "PL"
	}
	if r.ProxyProfile == "" {
		r.ProxyProfile = "current"
	}
}

func smsKey(provider string) string {
	names := []string{"SMS_API_KEY_HERO", "HERO_API_KEY"}
	if provider == "grizzly" {
		names = []string{"SMS_API_KEY", "GRIZZLY_API_KEY"}
	}
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func loadOTPGmailOrder(r resourceSettings) (otpGmailOrder, error) {
	var order otpGmailOrder
	if !filepath.IsAbs(r.OrderFile) || readRegisterJSON(r.OrderFile, &order) != nil {
		return order, errors.New("请绑定 OTPGmail 私有订单文件；面板不会自动购买邮箱")
	}
	if order.Provider != "otpgmail" || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(order.OrderID) || !registerEmailPattern.MatchString(order.Email) ||
		!strings.HasSuffix(strings.ToLower(order.Email), "@gmail.com") || (order.Warehouse != 1 && order.Warehouse != 2 && order.Warehouse != 4) ||
		!filepath.IsAbs(order.CredentialFile) || !strings.EqualFold(filepath.Clean(order.CredentialFile), filepath.Clean(r.CredentialFile)) {
		return order, errors.New("OTPGmail 订单、Gmail 地址或凭据文件不匹配")
	}
	if info, err := os.Stat(order.CredentialFile); err != nil || !info.Mode().IsRegular() {
		return order, errors.New("OTPGmail 凭据文件不可读")
	}
	return order, nil
}

func (s *Server) probeMail(ctx context.Context, r resourceSettings, onlyOrder bool) (map[string]any, error) {
	input := map[string]any{"repo": s.config.RepoRoot, "credentialFile": r.CredentialFile, "kind": "mail"}
	if r.OrderFile != "" || onlyOrder {
		order, err := loadOTPGmailOrder(r)
		if err != nil {
			return nil, err
		}
		input["order"] = order
	}
	if onlyOrder {
		input["kind"] = "order"
	}
	data, _ := json.Marshal(input)
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "-e", resourceProbe+"\nrunCLI();")
	cmd.Stdin = bytes.NewReader(data)
	configureProcess(cmd)
	output, err := cmd.Output()
	var result map[string]any
	if json.Unmarshal(output, &result) != nil {
		return nil, errors.New("邮箱检查未取得有效回执")
	}
	if code, ok := result["error"].(string); ok && regexp.MustCompile(`^[a-z_]{1,80}$`).MatchString(code) {
		return nil, errors.New("邮箱检查失败：" + code)
	}
	if err != nil {
		return nil, errors.New("邮箱检查未完成")
	}
	return result, nil
}

func resourceProxy(settings registerSettings, country string, port int) (map[string]any, error) {
	source, _ := settings.Roxy["proxyInfo"].(map[string]any)
	if name := settings.Resources.ProxyProfile; name != "" && name != "current" {
		source = settings.ProxyProfiles[name]
	}
	if source == nil {
		return nil, errors.New("所选代理配置不存在")
	}
	result := map[string]any{}
	for k, v := range source {
		result[k] = v
	}
	if port > 0 {
		result["port"] = port
	} else if settings.Resources.ProxyPort > 0 {
		result["port"] = settings.Resources.ProxyPort
	}
	user, _ := result["proxyUserName"].(string)
	replacement := strings.ToLower(country)
	if strings.Contains(user, "-zone-custom-region-") {
		replacement = strings.ToUpper(country)
	}
	result["proxyUserName"] = strings.ReplaceAll(user, "{country}", replacement)
	result["protocol"] = "SOCKS5"
	if _, err := proxyURL(result); err != nil {
		return nil, err
	}
	return result, nil
}

func proxyURL(p map[string]any) (*url.URL, error) {
	host, _ := p["host"].(string)
	user, _ := p["proxyUserName"].(string)
	password, _ := p["proxyPassword"].(string)
	port := 0
	switch n := p["port"].(type) {
	case float64:
		if n == float64(int(n)) {
			port = int(n)
		}
	case int:
		port = n
	case json.Number:
		port, _ = strconv.Atoi(string(n))
	}
	if host == "" || strings.ContainsAny(host, "/@?# \\:") || port < 1 || port > 65535 || user == "" || password == "" || strings.ContainsAny(user, "{}") {
		return nil, errors.New("代理地址、端口或凭据配置不完整")
	}
	return &url.URL{Scheme: "socks5h", Host: net.JoinHostPort(host, strconv.Itoa(port)), User: url.UserPassword(user, password)}, nil
}

func proxyProvider(p map[string]any) string {
	host, _ := p["host"].(string)
	user, _ := p["proxyUserName"].(string)
	if strings.Contains(host, "dataimpulse.com") || strings.Contains(user, "__cr.") {
		return "DataImpulse"
	}
	if strings.Contains(user, "-zone-custom-region-") {
		return "711"
	}
	return "已配置代理"
}

func (s *Server) resourceView(settings registerSettings, configErr error) map[string]any {
	r := settings.Resources
	result := map[string]any{"mailProvider": r.MailProvider, "settings": r, "purchase": settings.Purchase, "purchaseEnabled": s.config.AllowPurchase,
		"smsCredentialLoaded": smsKey(r.SMSProvider) != "", "mailCredentialConfigured": false, "orderConfigured": false, "proxyConfigured": false,
		"configReady": configErr == nil, "checked": false}
	if configErr != nil {
		result["configError"] = configErr.Error()
	}
	if info, err := os.Stat(r.CredentialFile); err == nil && info.Mode().IsRegular() {
		result["mailCredentialConfigured"] = true
	}
	if order, err := loadOTPGmailOrder(r); err == nil {
		result["orderConfigured"] = true
		result["mailbox"] = "***@gmail.com"
		result["warehouse"] = order.Warehouse
	} else {
		result["orderMessage"] = err.Error()
	}
	profiles := []map[string]string{}
	if p, _ := settings.Roxy["proxyInfo"].(map[string]any); p != nil {
		profiles = append(profiles, map[string]string{"id": "current", "name": proxyProvider(p) + " · 现有配置"})
	}
	for _, name := range []string{"711", "dataimpulse"} {
		if p := settings.ProxyProfiles[name]; p != nil {
			profiles = append(profiles, map[string]string{"id": name, "name": proxyProvider(p)})
		}
	}
	result["proxyProfiles"] = profiles
	if p, err := resourceProxy(settings, "US", 0); err == nil {
		result["proxyConfigured"] = true
		result["proxyProvider"] = proxyProvider(p)
		result["proxyPort"] = p["port"]
	} else {
		result["proxyMessage"] = err.Error()
	}
	return result
}

func (s *Server) serveResources(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/register/resources" && r.Method == "POST" {
		s.saveResources(w, r)
		return
	}
	s.registerMu.Lock()
	settings, err := s.registerSettings()
	s.registerMu.Unlock()
	if r.URL.Path == "/api/register/resources" && r.Method == "GET" {
		json.NewEncoder(w).Encode(s.resourceView(settings, err))
		return
	}
	if r.URL.Path != "/api/register/resources/check" || r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	var request struct {
		Resource string `json:"resource"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	var result map[string]any
	switch request.Resource {
	case "mail":
		result, err = s.probeMail(r.Context(), settings.Resources, false)
	case "sms":
		result, err = checkHero(r.Context(), smsKey("hero"), settings.Resources.PhoneCountry, settings.Resources.DurationHours, nil)
	case "proxy":
		result, err = checkResourceProxy(r.Context(), settings)
	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"message": "未知资源"})
		return
	}
	answer := map[string]any{"resource": request.Resource, "checkedAt": time.Now().UTC(), "ok": err == nil, "readOnly": true}
	if err != nil {
		answer["message"] = err.Error()
	} else {
		answer["result"] = result
	}
	json.NewEncoder(w).Encode(answer)
}

func (s *Server) saveResources(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Settings resourceSettings `json:"settings"`
		Purchase purchasePolicy   `json:"purchase"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	v := body.Settings
	if v.LocalBrowserProxy != "" && !validLocalBrowserProxy(v.LocalBrowserProxy) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"message": "本机浏览器代理须为带端口的本机 HTTP、HTTPS 或 SOCKS5 地址，不可包含凭据"})
		return
	}
	if v.MailProvider != "otpgmail" || v.SMSProvider != "hero" || len(v.PhoneCountry) != 2 || !strings.Contains("|PL|CO|US|PT|", "|"+v.PhoneCountry+"|") ||
		(v.DurationHours != 0 && v.DurationHours != 24) || (v.ProxyProfile != "current" && v.ProxyProfile != "711" && v.ProxyProfile != "dataimpulse") ||
		v.ProxyPort < 0 || v.ProxyPort > 65535 || !filepath.IsAbs(v.CredentialFile) || (v.OrderFile != "" && !filepath.IsAbs(v.OrderFile)) ||
		!(body.Purchase.valid() || (body.Purchase.MaxPrice == "" && body.Purchase.MaxTotal == "" && body.Purchase.Currency == "USD")) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"message": "请核对资源路径、国家、租期与 USD 预算"})
		return
	}
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	// Preserve unrelated private settings and unknown fields owned by other launchers.
	raw := map[string]json.RawMessage{}
	if readRegisterJSON(s.registerConfigFile(), &raw) != nil || raw == nil {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(map[string]string{"message": "原配置不可读，未覆盖；请先配置本机注册服务"})
		return
	}
	raw["resources"], _ = json.Marshal(v)
	raw["purchase"], _ = json.Marshal(body.Purchase)
	if writeRegisterJSON(s.registerConfigFile(), raw) != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"message": "资源设置未保存"})
		return
	}
	settings, err := s.registerSettings()
	json.NewEncoder(w).Encode(s.resourceView(settings, err))
}

func readProvider(ctx context.Context, client *http.Client, endpoint string) ([]byte, error) {
	request, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("供应商连接失败或超时")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, errors.New("供应商返回 HTTP " + strconv.Itoa(response.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, errors.New("供应商响应读取失败")
	}
	return data, nil
}

func checkHero(ctx context.Context, key, country string, duration int, client *http.Client) (map[string]any, error) {
	if key == "" {
		return nil, errors.New("Hero 密钥未加载到当前面板进程")
	}
	countries := map[string]string{"PL": "15", "CO": "33", "US": "187", "PT": "117"}
	if countries[country] == "" || (duration != 0 && duration != 24) {
		return nil, errors.New("Hero 国家或租期配置无效")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	call := func(action string, values url.Values) ([]byte, error) {
		if values == nil {
			values = url.Values{}
		}
		values.Set("api_key", key)
		values.Set("action", action)
		return readProvider(ctx, client, "https://hero-sms.com/stubs/handler_api.php?"+values.Encode())
	}
	balance, err := call("getBalance", nil)
	if err != nil {
		return nil, err
	}
	amount := strings.TrimPrefix(strings.TrimSpace(string(balance)), "ACCESS_BALANCE:")
	if !strings.HasPrefix(string(balance), "ACCESS_BALANCE:") || !regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`).MatchString(amount) {
		return nil, errors.New("Hero 余额查询被拒绝或响应无效")
	}
	active, err := call("getActiveActivations", nil)
	if err != nil {
		return nil, err
	}
	count, err := heroActivationCount(active)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"balance": amount, "balanceCurrency": "供应商账户币种", "activeOrders": count, "country": country, "durationHours": duration}
	action := "getPrices"
	params := url.Values{"country": {countries[country]}, "service": {"wx"}}
	if duration == 24 {
		action = "serviceCountRent"
		params.Set("currency", "840")
	}
	prices, err := call(action, params)
	if err != nil {
		return nil, err
	}
	// Raw prices and orders never reach the UI; unknown schemas are not usable inventory.
	quote, err := heroQuote(prices, countries[country], duration)
	if err != nil {
		return nil, err
	}
	result["quote"] = quote
	return result, nil
}

func heroActivationCount(data []byte) (int, error) {
	var value map[string]json.RawMessage
	bad := errors.New("Hero 活跃订单响应无效，未确认订单数量")
	if json.Unmarshal(data, &value) != nil {
		return 0, bad
	}
	var status string
	if json.Unmarshal(value["status"], &status) != nil || status != "success" {
		return 0, bad
	}
	count := -1
	for _, key := range []string{"activeActivations", "data"} {
		raw, ok := value[key]
		if !ok {
			continue
		}
		var rows []json.RawMessage
		if json.Unmarshal(raw, &rows) != nil {
			var object struct {
				Rows []json.RawMessage `json:"rows"`
			}
			if json.Unmarshal(raw, &object) != nil || object.Rows == nil {
				return 0, bad
			}
			rows = object.Rows
		}
		if rows == nil || (count >= 0 && count != len(rows)) {
			return 0, bad
		}
		count = len(rows)
	}
	if count < 0 {
		return 0, bad
	}
	return count, nil
}

func heroQuote(data []byte, country string, duration int) (map[string]any, error) {
	var value map[string]any
	bad := errors.New("Hero 报价结构未确认；不能据此购买")
	if json.Unmarshal(data, &value) != nil {
		return nil, bad
	}
	if duration == 0 {
		countries, _ := value[country].(map[string]any)
		row, _ := countries["wx"].(map[string]any)
		price, ok := row["cost"].(float64)
		count, cok := row["count"].(float64)
		if !ok || !cok || price <= 0 || count < 0 || count != float64(int(count)) {
			return nil, bad
		}
		return map[string]any{"price": price, "stock": int(count), "currency": "供应商未声明"}, nil
	}
	countries, _ := value[country].(map[string]any)
	row, _ := countries["24"].(map[string]any)
	price, ok := row["retail_price"].(float64)
	count, cok := row["count"].(float64)
	if !ok || !cok || price <= 0 || count < 0 || count != float64(int(count)) {
		return nil, bad
	}
	return map[string]any{"price": price, "stock": int(count), "currency": "USD", "durationHours": 24, "priceCapGuaranteed": false}, nil
}

func checkResourceProxy(ctx context.Context, settings registerSettings) (map[string]any, error) {
	p, err := resourceProxy(settings, "US", 0)
	if err != nil {
		return nil, err
	}
	endpoint, _ := proxyURL(p)
	transport := &http.Transport{Proxy: http.ProxyURL(endpoint)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	data, err := readProvider(ctx, client, "https://api.ipify.org")
	if err != nil {
		return nil, errors.New("固定代理连通检查失败；未更换出口")
	}
	if net.ParseIP(strings.TrimSpace(string(data))) == nil {
		return nil, errors.New("代理探测响应无效")
	}
	return map[string]any{"connected": true, "provider": proxyProvider(p), "countryRequested": "US", "countryVerified": false}, nil
}
