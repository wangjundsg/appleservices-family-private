package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/net/publicsuffix"
)

// Replay SetCookies, not Cookies(): the latter drops domain/path/expiry metadata.
type cookieEvent struct {
	URL     string
	Cookies []*http.Cookie
}
type recordedJar struct {
	http.CookieJar
	Events []cookieEvent
}

func newRecordedJar(events []cookieEvent) (*recordedJar, error) {
	j, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}
	r := &recordedJar{CookieJar: j}
	for _, e := range events {
		u, err := url.Parse(e.URL)
		if err != nil {
			return nil, err
		}
		j.SetCookies(u, e.Cookies)
	}
	r.Events = events
	return r, nil
}
func (j *recordedJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	copyCookies := make([]*http.Cookie, 0, len(cookies))
	for _, c := range cookies {
		v := *c
		if v.MaxAge > 0 {
			v.Expires = time.Now().Add(time.Duration(v.MaxAge) * time.Second)
			v.MaxAge = 0
		}
		copyCookies = append(copyCookies, &v)
	}
	j.CookieJar.SetCookies(u, copyCookies)
	if len(copyCookies) > 0 {
		j.Events = append(j.Events, cookieEvent{u.String(), copyCookies})
	}
}

type sessionState struct {
	Options                                 SessionOptions
	Scnt, SessionID, HCChallenge, WidgetKey string
	HCBits                                  int
	Cookies                                 []cookieEvent
	CurlCookies                             []byte `json:",omitempty"`
}

func (s *Session) snapshot(opts SessionOptions) (*sessionState, error) {
	v := &sessionState{Options: opts, Scnt: s.scnt, SessionID: s.sessionID, HCChallenge: s.hcChallenge, HCBits: s.hcBits, WidgetKey: s.widgetKey}
	if s.UseCurl {
		if s.curlJar != "" {
			var err error
			v.CurlCookies, err = os.ReadFile(s.curlJar)
			if err != nil {
				return nil, err
			}
		}
	} else {
		jar, ok := s.client.Jar.(*recordedJar)
		if !ok {
			return nil, errors.New("session cookie journal unavailable")
		}
		v.Cookies = jar.Events
	}
	return v, nil
}
func restoreSession(v *sessionState) (*Session, error) {
	if v == nil || v.Scnt == "" || v.SessionID == "" {
		return nil, errors.New("saved session is incomplete; do not bootstrap a replacement")
	}
	s, err := newSession(v.Options)
	if err != nil {
		return nil, err
	}
	s.scnt, s.sessionID, s.hcChallenge, s.hcBits, s.widgetKey = v.Scnt, v.SessionID, v.HCChallenge, v.HCBits, v.WidgetKey
	if s.UseCurl {
		if err = os.WriteFile(s.cookieFile(), v.CurlCookies, 0600); err != nil {
			s.Close()
			return nil, err
		}
	} else {
		s.client.Jar, err = newRecordedJar(v.Cookies)
		if err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

func writeState(path string, st *state) error {
	st.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".signup-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}

// A crash leaves the lock for explicit review; never guess that a write is safe to replay.
func lockState(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("task locked; another process is running or a previous process needs review")
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(path + ".lock") }, nil
}
