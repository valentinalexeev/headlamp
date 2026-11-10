package agent

import (
	"context"
	"errors"
	"testing"
)

func TestXrayVersion(t *testing.T) {
	for _, c := range []struct {
		out  string
		code int
		err  error
		want string
	}{
		{"Xray 25.10.15 (Xray, Penetrates Everything.) 1fc5d4b (go1.25.3 linux/amd64)\nA unified platform\n", 0, nil, "25.10.15"},
		{"Xray v26.1.2 (x)\n", 0, nil, "26.1.2"},
		{"something else\n", 0, nil, ""},
		{"Xray 25.10.15\n", 1, nil, ""},
		{"", 0, errors.New("no binary"), ""},
	} {
		var got []string
		r := &Runner{Bin: "/x/xray", Run: func(_ context.Context, args []string) (int, string, error) {
			got = args
			return c.code, c.out, c.err
		}}
		if v := r.XrayVersion(context.Background()); v != c.want {
			t.Errorf("%q: got %q, want %q", c.out, v, c.want)
		}
		if len(got) != 2 || got[0] != "/x/xray" || got[1] != "version" {
			t.Errorf("args %v", got)
		}
	}
}
