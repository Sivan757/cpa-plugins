// Command cpa-provider-tencent serves every Tencent subscription credential
// behind one CLIProxyAPI provider key.
//
// The WorkBuddy desktop products, WorkBuddy AI, and a CodeBuddy OAuth or API
// key credential all reach the same gateway host, so they share one plugin and
// stop competing over overlapping model identifiers.
package main

import "github.com/sivan/cpa-plugins/internal/pluginkit"

func init() {
	pluginkit.RegisterPlugin(newGateway())
}
