package dashboard

import (
	"errors"
	"net/url"
	"strconv"
)

func validLocalBrowserProxy(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port > 0 && port <= 65535 &&
		(u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "socks5") &&
		(u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1") &&
		u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
}

func validateBrowserSelection(body registerRequest) error {
	if body.BrowserMode != "" && body.BrowserMode != "forge" && body.BrowserMode != "local" {
		return errors.New("请选择本机 Chrome 或 Forge 指纹浏览器")
	}
	if body.BrowserMode == "local" {
		if body.RegistrationMethod == methodProtocol || (body.Egress.Mode != "local" && body.Egress.Mode != "direct") {
			return errors.New("本机 Chrome 只使用本机代理或直连")
		}
		if body.CaptchaProvider != "manual" {
			return errors.New("本机 Chrome 的图形校验需在场人工完成")
		}
	} else if body.Egress.Mode != "direct" && body.Egress.Mode != "roxy" {
		return errors.New("Forge 只使用已保存的固定代理或直连")
	}
	return nil
}
