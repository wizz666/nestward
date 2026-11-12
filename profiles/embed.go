// Package profiles embeds the app profile catalog. The YAML files in
// this folder are MIT-licensed (see LICENSE here) so other projects can
// reuse them freely; the Go code around them is AGPL-3.0 like the rest.
package profiles

import "embed"

// FS holds every *.yaml profile.
//
//go:embed *.yaml
var FS embed.FS
