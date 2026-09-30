package main

// credentialKind is one upstream credential family this plugin serves.
//
// All three talk to the same Tencent gateway, but their credentials and
// identity headers differ, so each carries its own chat base, catalog user
// agent, and auth-record kind marker. The host binds this whole plugin to the
// single provider key "tencent"; the kind inside each auth record selects the
// upstream dialect per credential.
type credentialKind struct {
	// id is the auth-record "type"/"kind" value in the storage blob.
	id string
	// displayName is the label the management UI shows for this credential.
	displayName string
	// region is "cn" or "global" and selects the endpoint set.
	region string
	// chatBase is the upstream root for chat and catalog calls.
	chatBase string
	// billingURL is the personal credit endpoint.
	billingURL string
	// authFilename is the desktop credential file inside the shared auth dir.
	authFilename string
	// origin and productToken build the request identity headers.
	origin       string
	productToken string
	// catalogUA selects the CLI roster on the CN gateway; the international
	// gateway rejects a user agent containing a space, so it builds its own.
	catalogUA string
	// electronPath locates the app binary used to unwrap the at-rest key.
	electronPath string
	// envElectron overrides electronPath.
	envElectron string
	// fallbackAppVersion is used when the bundle version cannot be read.
	fallbackAppVersion string
	// desktop is true when the credential is read from the desktop app rather
	// than from this plugin's own OAuth document or an API key.
	desktop bool
}

// kinds is every credential family this plugin serves. The values mirror the
// verified standalone plugins: the WorkBuddy rows come from the WorkBuddy
// variant table and the CodeBuddy row keeps the CLI identity the CodeBuddy
// plugin used.
var kinds = map[string]*credentialKind{
	"workbuddy": {
		id:                 "workbuddy",
		displayName:        "WorkBuddy",
		region:             "cn",
		chatBase:           "https://copilot.tencent.com",
		billingURL:         "https://www.codebuddy.cn/v2/billing/meter/get-user-resource",
		authFilename:       "workbuddy-desktop.info",
		origin:             "https://www.codebuddy.cn",
		productToken:       "WorkBuddy",
		catalogUA:          "CLI/2.63.2 CodeBuddy/2.63.2",
		electronPath:       "/Applications/WorkBuddy.app/Contents/MacOS/Electron",
		envElectron:        "WORKBUDDY_ELECTRON_BIN",
		fallbackAppVersion: "5.5.6",
		desktop:            true,
	},
	"workbuddy-ai": {
		id:                 "workbuddy-ai",
		displayName:        "WorkBuddy AI",
		region:             "global",
		chatBase:           "https://www.workbuddy.ai",
		billingURL:         "https://www.workbuddy.ai/v2/billing/meter/get-user-resource",
		authFilename:       "workbuddy-desktop-ai.info",
		origin:             "https://www.workbuddy.ai",
		productToken:       "WorkBuddy AI",
		catalogUA:          "CLI/2.63.2 CodeBuddy/2.63.2",
		electronPath:       "/Applications/WorkBuddy AI.app/Contents/MacOS/Electron",
		envElectron:        "WORKBUDDY_AI_ELECTRON_BIN",
		fallbackAppVersion: "5.5.2",
		desktop:            true,
	},
	"codebuddy": {
		id:                 "codebuddy",
		displayName:        "CodeBuddy",
		region:             "cn",
		chatBase:           "https://copilot.tencent.com",
		billingURL:         "https://www.codebuddy.cn/v2/billing/meter/get-user-resource",
		origin:             "https://www.codebuddy.cn",
		productToken:       "CodeBuddy",
		catalogUA:          "CLI/unknown CodeBuddy/2.136.0",
		fallbackAppVersion: "2.133.1",
		desktop:            false,
	},
}

// Credential kind ids as they appear in auth records.
const (
	kindWorkbuddyID   = "workbuddy"
	kindWorkbuddyAIID = "workbuddy-ai"
	kindCodebuddyID   = "codebuddy"
)

// kindOrder fixes the credential order in listings and diagnostics.
var kindOrder = []string{"workbuddy", "workbuddy-ai", "codebuddy"}

// kindFor resolves a kind id. An empty or unknown value defaults to the
// desktop CN credential, the long-standing default, so a malformed storage
// blob can never nil-deref downstream. Callers that must distinguish an
// unknown kind (auth.parse) check the kinds map directly.
func kindFor(id string) *credentialKind {
	if k, ok := kinds[id]; ok {
		return k
	}
	return kinds["workbuddy"]
}
