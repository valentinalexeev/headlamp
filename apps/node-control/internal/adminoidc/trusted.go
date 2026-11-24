package adminoidc

import "strings"

// ParseDomains reads ADMIN_OIDC_TRUSTED_EMAIL_DOMAINS (#447): a comma list of email domains.
// Each domain is trimmed, lower-cased and without a leading "@". Empty entries are skipped.
func ParseDomains(s string) []string {
	var out []string
	for _, d := range strings.Split(s, ",") {
		d = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(d)), "@")
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// trustedDomain tells whether the domain of email (the part after the last "@") is one of
// trusted. The match is exact and case-insensitive: a subdomain does not match.
func trustedDomain(email string, trusted []string) bool {
	i := strings.LastIndex(email, "@")
	if i < 0 {
		return false
	}
	domain := strings.ToLower(strings.TrimSpace(email[i+1:]))
	if domain == "" {
		return false
	}
	for _, d := range trusted {
		if strings.EqualFold(strings.TrimSpace(d), domain) {
			return true
		}
	}
	return false
}
