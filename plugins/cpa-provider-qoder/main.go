// Command cpa-provider-qoder serves Qoder subscription credentials behind one
// CLIProxyAPI provider key.
package main

import "github.com/sivan/cpa-plugins/internal/cpakit"

func init() { cpakit.RegisterPlugin(newGateway()) }
