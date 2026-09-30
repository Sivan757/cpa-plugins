package main

import "runtime"

// providerKey is the credential provider key this plugin owns.
//
// Both Qoder deployments share one key on purpose. The region is part of the
// credential (each auth record carries its own), while the host binds one
// executor per key, so a single library serves a qoder.sh account and a
// qoder.com.cn account at the same time. Splitting the key would force a user
// who holds both to add two providers that differ only by a field.
const providerKey = "qoder"

// ideVersion is the client version the COSY signature advertises. It is written
// both into the Cosy-Version header and inside the signed payload, so the two
// must stay identical or the upstream rejects the signature outright.
const ideVersion = "1.1.60"

// cosyProtocolVersion is the openapi protocol version. It is unrelated to
// ideVersion: the openapi face sends it, the COSY face does not.
const cosyProtocolVersion = "1.0.1"

// clientType is the value the official client sends. Any other string leaves a
// detectable third-party fingerprint in the request headers.
const clientType = "5"

// userAgent accompanies every openapi request.
const userAgent = "qoder/" + ideVersion

// region describes one Qoder deployment.
//
// apiBase carries its trailing slash because the upstream routes hang off
// "/algo" and the catalog URL is built by concatenation.
type region struct {
	id          string
	displayName string
	apiBase     string
	openAPI     string
	centerURL   string
	// portalURL is where a subscriber creates the personal access token the
	// plugin authenticates with.
	portalURL string
}

var regions = map[string]*region{
	"global": {
		id:          "global",
		displayName: "Qoder",
		apiBase:     "https://api3.qoder.sh/",
		openAPI:     "https://openapi.qoder.sh",
		centerURL:   "https://center.qoder.sh",
		portalURL:   "https://qoder.sh/account/integrations",
	},
	"china": {
		id:          "china",
		displayName: "Qoder CN",
		apiBase:     "https://gateway.qoder.com.cn/",
		openAPI:     "https://openapi.qoder.com.cn",
		centerURL:   "https://gateway.qoder.com.cn",
		portalURL:   "https://qoder.cn/account/integrations",
	},
}

// defaultRegionID is used when neither the credential nor the config names one.
const defaultRegionID = "global"

// regionFor resolves a region name. An unknown name falls back to the default
// rather than failing, so a typo in a credential degrades to "wrong region"
// instead of taking the whole provider down.
func regionFor(id string) *region {
	if resolved, ok := regions[normalizeRegionID(id)]; ok {
		return resolved
	}
	return regions[defaultRegionID]
}

// normalizeRegionID maps the spellings users and docs actually use onto the two
// canonical ids. "china" and "cn" are the same deployment.
func normalizeRegionID(id string) string {
	switch trimLower(id) {
	case "china", "cn", "qoder-cn", "qodercn", "qoder.com.cn", "mainland":
		return "china"
	case "global", "intl", "international", "qoder.sh", "overseas":
		return "global"
	default:
		return ""
	}
}

// isKnownRegion reports whether a name resolves without falling back.
func isKnownRegion(id string) bool {
	_, ok := regions[normalizeRegionID(id)]
	return ok
}

// machineOS renders the platform token the official client sends.
//
// The desktop client reports the Linux spelling on macOS too, and the upstream
// does not validate the value, so this mirrors the client instead of reporting
// the real platform.
func machineOS() string {
	arch := "x86_64"
	if runtime.GOARCH == "arm64" || runtime.GOARCH == "aarch64" {
		arch = "aarch64"
	}
	if runtime.GOOS == "windows" {
		return arch + "_windows"
	}
	return arch + "_linux"
}
