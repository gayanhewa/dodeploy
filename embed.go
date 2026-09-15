// Package dodeploy carries the files that ship with the command.
//
// The only embedded asset is the agent skill under skills/, which the CLI can
// install into a project or a user's home directory so an AI coding assistant
// knows how to deploy with this tool.
package dodeploy

import "embed"

// Skills holds every skill shipped with the command, keyed by its repository
// path, e.g. "skills/dodeploy/SKILL.md".
//
//go:embed skills
var Skills embed.FS
