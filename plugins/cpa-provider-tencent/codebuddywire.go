package main

import "net/http"

// The CodeBuddy header group. The gateway validates the CLI identity shape on
// /v3/config (error 12403) and routes and bills OAuth requests on the account
// headers, so the merged plugin must send exactly what the standalone
// CodeBuddy plugin sent; the WorkBuddy desktop header group is not accepted
// for this credential kind. Copied from the verified CodeBuddy upstream.go.

// clientHeaders renders the CodeBuddy CLI identity.
func clientHeaders() http.Header {
	headers := http.Header{}
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("X-Requested-With", "XMLHttpRequest")
	headers.Set("User-Agent", userAgent)
	headers.Set("X-IDE-Type", "CLI")
	headers.Set("X-IDE-Name", "CLI")
	headers.Set("X-IDE-Version", ideVersion)
	headers.Set("X-Product-Version", productVersion)
	headers.Set("X-Private-Data", "false")
	return headers
}

// credentialHeaders adds the credential and, in OAuth mode, the account
// identity the gateway routes and bills on. ideVersion, productVersion, and
// userAgent come from the CodeBuddy identity block.
func credentialHeaders(cred *credentials) http.Header {
	headers := clientHeaders()
	headers.Set("Authorization", "Bearer "+cred.bearer())
	if cred.Mode == string(modeOAuth) {
		if cred.Domain != "" {
			headers.Set("X-Domain", cred.Domain)
		}
		if cred.UID != "" {
			headers.Set("X-User-Id", cred.UID)
		}
		if cred.isEnterprise() {
			headers.Set("X-Enterprise-Id", cred.EnterpriseID)
		}
	}
	return headers
}

// apiHeaders adds the SaaS product marker the catalog and billing endpoints
// expect. In api-key mode the raw key is repeated in x-api-key, which is how
// those endpoints resolve the active key without rotating.
func apiHeaders(cred *credentials) http.Header {
	headers := credentialHeaders(cred)
	headers.Set("X-Product", "SaaS")
	if cred.Mode == string(modeAPIKey) && cred.APIKey != "" {
		headers.Set("x-api-key", cred.APIKey)
	}
	return headers
}

// rotationKey identifies one candidate in the rotation state. It is derived
// from the key material so no log line ever needs to name it.
func (c *credentials) rotationKey() string {
	return c.Mode + ":" + c.bearer()
}
