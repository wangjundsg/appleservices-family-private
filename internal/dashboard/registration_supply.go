package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type purchasePolicy struct {
	MaxPrice string `json:"maxPrice"`
	MaxTotal string `json:"maxTotal"`
	Currency string `json:"currency"`
}

type purchaseSummary struct {
	Purchases     int    `json:"purchases"`
	Currency      string `json:"currency"`
	Gross         string `json:"gross"`
	Refunded      string `json:"refunded"`
	OTPReceived   bool   `json:"otp_received"`
	RefundPending bool   `json:"refund_pending"`
}

func (p purchasePolicy) valid() bool {
	pattern := regexp.MustCompile(`^[0-9]{1,6}(\.[0-9]{1,6})?$`)
	if p.Currency != "USD" || !pattern.MatchString(p.MaxPrice) || !pattern.MatchString(p.MaxTotal) {
		return false
	}
	maximum, _ := new(big.Rat).SetString(p.MaxPrice)
	total, _ := new(big.Rat).SetString(p.MaxTotal)
	return maximum.Sign() > 0 && total.Cmp(maximum) >= 0
}

func normalizedRegistrationEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func generateRegistrationPassword(entropy io.Reader) (string, error) {
	// Keep 18 random bytes while avoiding long hex strings and invalid runs.
	for attempt := 0; attempt < 16; attempt++ {
		random := make([]byte, 18)
		if _, err := io.ReadFull(entropy, random); err != nil {
			return "", errors.New("无法生成账号密码")
		}
		password := "Aa1!" + base64.RawURLEncoding.EncodeToString(random)
		if strongRegistrationPassword(password) {
			return password, nil
		}
	}
	return "", errors.New("无法生成符合注册规则的密码")
}

// Called under registerMu: request.json is the durable mailbox reservation.
// The Outlook read-only API cannot reserve a mailbox for other consumers.
func (s *Server) prepareAutomaticRegistration(body *registerRequest, settings registerSettings) error {
	s.resourceDefaults(&settings)
	if settings.Resources.SMSProvider != "hero" && settings.Resources.SMSProvider != "grizzly" {
		return errors.New("不支持的购号供应商")
	}
	if smsKey(settings.Resources.SMSProvider) == "" {
		return errors.New("服务端缺少所选短信供应商密钥")
	}
	if settings.Resources.MailProvider == "otpgmail" {
		return s.prepareOTPGmailRegistration(body, settings.Resources)
	}
	if settings.Resources.MailProvider != "outlook" {
		return errors.New("不支持的邮箱供应商")
	}
	var client struct {
		BaseURL  string `json:"baseUrl"`
		APIToken string `json:"apiToken"`
	}
	if !filepath.IsAbs(settings.CodePoolConfig) || readRegisterJSON(settings.CodePoolConfig, &client) != nil {
		return errors.New("Outlook 服务客户端配置不可读")
	}
	base, err := url.Parse(client.BaseURL)
	if err != nil || base.Scheme != "http" || base.Hostname() != "127.0.0.1" || base.User != nil || client.APIToken == "" {
		return errors.New("Outlook 服务必须是配置好的本机接口")
	}
	used := map[string]bool{}
	s.accountsMu.Lock()
	for _, account := range s.accounts {
		used[normalizedRegistrationEmail(account.AppleID)] = true
	}
	s.accountsMu.Unlock()
	// Legacy runs have no safe release receipt; even an unknown result retains ownership.
	var legacyUsage map[string]string
	if err := readRegisterJSON(filepath.Join(s.config.RepoRoot, ".cache", "mailbox-usage.json"), &legacyUsage); err != nil {
		if !os.IsNotExist(err) {
			return errors.New("旧邮箱占用记录损坏，请先核实")
		}
	} else {
		if legacyUsage == nil {
			return errors.New("旧邮箱占用记录损坏，请先核实")
		}
		for email := range legacyUsage {
			used[normalizedRegistrationEmail(email)] = true
		}
	}
	files, err := filepath.Glob(filepath.Join(s.registerRoot(), "reg-*", "request.json"))
	if err != nil {
		return errors.New("无法读取邮箱占用记录")
	}
	for _, file := range files {
		var prior struct {
			Email string `json:"email"`
		}
		if readRegisterJSON(file, &prior) != nil {
			return errors.New("邮箱占用记录损坏，请先核实")
		}
		used[normalizedRegistrationEmail(prior.Email)] = true
	}
	base.Path, base.RawQuery, base.Fragment = "/api/v1/mailboxes", "", ""
	req, err := http.NewRequest("GET", base.String(), nil)
	if err != nil {
		return errors.New("Outlook 服务地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+client.APIToken)
	httpClient := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := httpClient.Do(req)
	if err != nil {
		return errors.New("Outlook 邮箱池暂时不可用")
	}
	defer response.Body.Close()
	var mailboxes []struct {
		Address       string `json:"address"`
		Reader        string `json:"reader"`
		Status        string `json:"status"`
		Authorization string `json:"authorization_status"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&mailboxes) != nil {
		return errors.New("Outlook 邮箱池响应无效，请检查服务密钥")
	}
	for _, mailbox := range mailboxes {
		email := normalizedRegistrationEmail(mailbox.Address)
		if mailbox.Reader != "graph" || mailbox.Status != "ok" || mailbox.Authorization != "ready" || used[email] || !registerEmailPattern.MatchString(email) {
			continue
		}
		// Existing registrar explicitly rejects these domains; do not spend on them.
		if strings.HasSuffix(email, "@outlook.com") || strings.HasSuffix(email, "@icloud.com") {
			continue
		}
		password, err := generateRegistrationPassword(rand.Reader)
		if err != nil {
			return err
		}
		body.Email, body.Password = email, password
		return nil
	}
	return errors.New("没有未占用且授权可用的受支持邮箱；当前建号器不支持 outlook.com 和 icloud.com")
}

func (s *Server) prepareOTPGmailRegistration(body *registerRequest, r resourceSettings) error {
	order, err := loadOTPGmailOrder(r)
	if err != nil {
		return err
	}
	email := normalizedRegistrationEmail(order.Email)
	s.accountsMu.Lock()
	occupied := false
	for _, account := range s.accounts {
		if normalizedRegistrationEmail(account.AppleID) == email {
			occupied = true
		}
	}
	s.accountsMu.Unlock()
	if occupied {
		return errors.New("邮箱已绑定账号，不能重新注册")
	}
	files, _ := filepath.Glob(filepath.Join(s.registerRoot(), "reg-*", "request.json"))
	for _, file := range files {
		var prior struct {
			Email     string `json:"email"`
			OrderFile string `json:"otpgmail_order_file"`
		}
		if readRegisterJSON(file, &prior) != nil {
			return errors.New("历史任务不可读，请先核实邮箱占用")
		}
		if normalizedRegistrationEmail(prior.Email) == email {
			return errors.New("邮箱已有任务，未知或失败结果不会自动释放订单")
		}
		if prior.OrderFile != "" {
			var bound otpGmailOrder
			if readRegisterJSON(prior.OrderFile, &bound) != nil {
				return errors.New("历史邮箱订单不可读，请先核实")
			}
			if bound.OrderID == order.OrderID {
				return errors.New("邮箱订单已被任务占用")
			}
		}
	}
	result, err := s.probeMail(context.Background(), r, true)
	if err != nil {
		return err
	}
	if result["orderReady"] != true {
		return errors.New("邮箱订单未处于等待新码状态，未购买号码")
	}
	password, err := generateRegistrationPassword(rand.Reader)
	if err != nil {
		return err
	}
	body.Email, body.Password = email, password
	body.mailOrder = &order
	return nil
}

func (s *Server) registrationInProgress(account *Account) bool {
	if account == nil {
		return false
	}
	var origin struct {
		JobID string `json:"job_id"`
	}
	if readRegisterJSON(filepath.Join(account.Dir, "registration-origin.json"), &origin) != nil {
		return false
	}
	if !registerIDPattern.MatchString(origin.JobID) {
		return true
	}
	var receipt pipelineReceipt
	// Fail closed after a crash: the account must first be reconciled by its owner.
	return readRegisterJSON(filepath.Join(s.registerFolder(origin.JobID), "result.json"), &receipt) != nil || !receipt.HandoffComplete
}
