package controllers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateWebhookURL(t *testing.T) {
	for _, ok := range []string{"http://alerts.svc:8080/x", "https://example.com/hook"} {
		if err := validateWebhookURL(ok); err != nil {
			t.Errorf("%s: unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"file:///etc/passwd", "gopher://x/", "ftp://x/", "http://", "/relative", ""} {
		if err := validateWebhookURL(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestWebhookClientBlocksLoopbackAndLinkLocal(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	c := newWebhookClient(2 * time.Second)
	for _, u := range []string{srv.URL, "http://169.254.169.254/latest/meta-data/"} {
		resp, err := c.Post(u, "application/json", strings.NewReader("{}"))
		if err == nil {
			resp.Body.Close()
			t.Errorf("%s: expected dial to be refused", u)
		}
	}
	if hit {
		t.Error("loopback server was reached")
	}
}

func TestWebhookClientDoesNotFollowRedirects(t *testing.T) {
	c := newWebhookClient(time.Second)
	req, _ := http.NewRequest("POST", "http://example.invalid/", nil)
	if err := c.CheckRedirect(req, nil); err != http.ErrUseLastResponse {
		t.Errorf("CheckRedirect = %v, want ErrUseLastResponse", err)
	}
}
