// Package signatures embeds the list of apps Nestward can recognise in
// running containers. The YAML files here are MIT-licensed (see
// LICENSE) so other projects can reuse them.
package signatures

import "embed"

// FS holds every *.yaml signature file.
//
//go:embed *.yaml
var FS embed.FS
