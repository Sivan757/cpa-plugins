// Command cpa-provider-opencode serves the OpenCode Zen endpoints behind one
// CLIProxyAPI provider key.
//
// Two credential tiers share the key: the paid Go plan (an oc_sk_... API key)
// and the anonymous free tier, which authenticates as the OpenCode CLI itself.
// Both speak the OpenAI chat-completions shape, so one executor serves them.
package main

import "github.com/sivan/cpa-plugins/internal/cpakit"

func init() {
	cpakit.RegisterPlugin(newGateway())
}
