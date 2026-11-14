package dashboard

import (
	"crypto/rand"
	"encoding/hex"
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

// Called under registerMu: request.json is the durable mailbox reservation.
// The Outlook read-only API cannot reserve a mailbox for other consumers.
func (s *Server) prepareAutomaticRegistration(body *registerRequest, settings registerSettings) error {
	if os.Getenv("SMS_API_KEY") == "" {
		return errors.New("服务端缺少 SMS_API_KEY")
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
		random := make([]byte, 18)
		if _, err := rand.Read(random); err != nil {
			return errors.New("无法生成账号密码")
		}
		body.Email, body.Password = email, "Aa1!"+hex.EncodeToString(random)
		return nil
	}
	return errors.New("没有未占用且授权可用的受支持邮箱；当前建号器不支持 outlook.com 和 icloud.com")
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
