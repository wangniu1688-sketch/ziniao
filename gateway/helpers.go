package gateway

import (
	"encoding/json"
	"net/http"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/identity"
	"ziniao/shop"
)

// decodeJSON 解析请求体（限制 1MB，防止大 body 攻击）。
func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

func loginInput(username, password, totp, tenantID string, term eventbus.Terminal) identity.LoginInput {
	return identity.LoginInput{
		Username: username, Password: password, TOTPCode: totp,
		TenantID: tenantID, Terminal: term,
	}
}

func shopCreateInput(name, platform, site, status, url string, subj iam.Subject, term eventbus.Terminal) shop.CreateInput {
	return shop.CreateInput{
		Name: name, Platform: platform, Site: site, Status: status,
		Actor: subj, Terminal: term, URL: url,
	}
}
