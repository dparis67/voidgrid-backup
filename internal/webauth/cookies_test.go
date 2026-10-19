package webauth

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func cookieFor(t *testing.T, r *http.Request, clear bool) *http.Cookie {
	t.Helper()
	a := &Authenticator{}
	w := httptest.NewRecorder()
	if clear {
		a.clearCookie(w, r, "c")
	} else {
		a.setCookie(w, r, "c", "v", time.Hour)
	}
	cs := w.Result().Cookies()
	if len(cs) != 1 {
		t.Fatalf("%d cookies set", len(cs))
	}
	return cs[0]
}

func TestCookiesAreAlwaysHttpOnlyAndLaxAndSecureOverHTTPS(t *testing.T) {
	plain := httptest.NewRequest("GET", "http://example.test/", nil)
	direct := httptest.NewRequest("GET", "https://example.test/", nil)
	direct.TLS = &tls.ConnectionState{}
	proxied := httptest.NewRequest("GET", "http://example.test/", nil)
	proxied.Header.Set("X-Forwarded-Proto", "HTTPS") // any case
	proxiedHTTP := httptest.NewRequest("GET", "http://example.test/", nil)
	proxiedHTTP.Header.Set("X-Forwarded-Proto", "http")

	for name, tc := range map[string]struct {
		r      *http.Request
		secure bool
	}{
		"plain HTTP":             {plain, false},
		"TLS directly":           {direct, true},
		"HTTPS through a proxy":  {proxied, true},
		"a proxy that says http": {proxiedHTTP, false},
	} {
		for _, clear := range []bool{false, true} {
			c := cookieFor(t, tc.r, clear)
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
				t.Errorf("%s (clear=%v): cookie is not HttpOnly, SameSite=Lax and path /: %+v", name, clear, c)
			}
			if c.Secure != tc.secure {
				t.Errorf("%s (clear=%v): Secure = %v, want %v", name, clear, c.Secure, tc.secure)
			}
			if clear && (c.MaxAge >= 0 || c.Value != "") {
				t.Errorf("%s: a cleared cookie must expire and be empty: %+v", name, c)
			}
			if !clear && (c.MaxAge != 3600 || c.Value != "v") {
				t.Errorf("%s: a set cookie keeps its value and lifetime: %+v", name, c)
			}
		}
	}
}
