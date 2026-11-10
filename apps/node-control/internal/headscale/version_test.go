package headscale

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVersion(t *testing.T) {
	for _, c := range []struct {
		code       int
		body, want string
		fail       bool
	}{
		{200, `{"version":"v0.29.7","commit":"abc","buildTime":"x","go":{},"dirty":false}`, "v0.29.7", false},
		{200, "v0.26.1\n", "v0.26.1", false},
		{200, "<html>nope</html>", "", true},
		{404, "", "", true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/version" || r.Header.Get("Authorization") != "" {
				t.Errorf("request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
			}
			w.WriteHeader(c.code)
			_, _ = w.Write([]byte(c.body))
		}))
		got, err := (&Client{BaseURL: srv.URL + "/", APIKey: "k"}).Version(context.Background())
		srv.Close()
		if got != c.want || (err != nil) != c.fail {
			t.Errorf("%q: got %q, %v", c.body, got, err)
		}
	}
}
