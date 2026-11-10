package agent

import (
	"context"
	"regexp"
	"strings"
)

// xrayVersionRe is the version word of the first line of `xray version`, e.g. "Xray 25.10.15 (Xray, ...)".
var xrayVersionRe = regexp.MustCompile(`^Xray\s+v?([0-9][0-9A-Za-z.+-]{0,40})\b`)

// XrayVersion asks the xray binary for its version (#195). It returns "" when xray cannot be run or its
// answer has no version: the fleet view then shows the xray version as unknown.
func (r *Runner) XrayVersion(ctx context.Context) string {
	code, out, err := r.run(ctx, []string{r.Bin, "version"})
	if err != nil || code != 0 {
		return ""
	}
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	m := xrayVersionRe.FindStringSubmatch(strings.TrimSpace(first))
	if m == nil {
		return ""
	}
	return m[1]
}
