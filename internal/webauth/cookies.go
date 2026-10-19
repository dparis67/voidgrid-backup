package webauth

import (
	"net/http"
	"strings"
	"time"
)

// Every cookie this package sets is made here, and this is the only file where
// the Secure flag is ever turned off: .golangci.yml excuses gosec's G124 for
// this file alone. A cookie is HttpOnly and SameSite=Lax always, and Secure
// whenever the request arrived over HTTPS, directly or through the operator's
// reverse proxy. It is not Secure on plain HTTP because a browser drops a Secure
// cookie there, and the UI has to work on a LAN with nothing in front of it.

// secure reports whether cookies should carry the Secure flag: the request
// arrived over TLS, directly or (trusting the operator's own reverse proxy,
// as documented) via X-Forwarded-Proto.
func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// newCookie is a cookie that is HttpOnly, SameSite=Lax and Secure. The one
// relaxation is for a request that did not arrive over HTTPS (see secure): a
// browser would drop a Secure cookie on plain HTTP, and the UI has to work on a
// LAN with nothing in front of it. Starting from the safe settings and relaxing
// them in one visible place keeps every cookie this package sets consistent.
func newCookie(r *http.Request, name, value string, maxAge int) *http.Cookie {
	c := &http.Cookie{
		Name: name, Value: value, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, MaxAge: maxAge,
	}
	if !secure(r) {
		c.Secure = false
	}
	return c
}

func (a *Authenticator) setCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge time.Duration) {
	http.SetCookie(w, newCookie(r, name, value, int(maxAge.Seconds())))
}

// clearCookie deletes a cookie. It carries the same attributes it was set with:
// a browser will not let a response without Secure overwrite a Secure cookie.
func (a *Authenticator) clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, newCookie(r, name, "", -1))
}
