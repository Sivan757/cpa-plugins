// Command cpa-provider-qoder is a CLIProxyAPI native plugin that serves the
// Qoder (Alibaba) subscription: PAT credentials, model catalog, chat
// execution, and account quota.
//
// One binary serves both Qoder deployments. The region is a credential-level
// property rather than a build-level one, so the plugin owns the single
// provider key "qoder" and resolves the deployment from the auth record.
package main

import "github.com/sivan/cpa-plugins/internal/pluginkit"

func init() {
	pluginkit.RegisterPlugin(newGateway())
}
