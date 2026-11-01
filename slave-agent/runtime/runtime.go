// Package runtime drives the inference engine on a Slave. The agent's
// reconciler talks to it through Runtime; the Ollama driver is the default,
// and the fake driver does real pulls without running inference (dev, tests).
package runtime

import (
	"context"

	"aicluster/common/registry"
)

// Model is one model as the runtime sees it.
type Model struct {
	Ref registry.Ref
	// LocalName is the name clients use on this node, e.g. "qwen2.5:7b-instruct-q4_K_M".
	LocalName string
	// RegistryHost is the agent's loopback registry shim, e.g. "127.0.0.1:7452".
	RegistryHost string
}

// Progress reports bytes downloaded so far and the total, when known.
type Progress func(done, total int64)

type Runtime interface {
	Kind() string
	// Check returns nil when the runtime is up and usable.
	Check(ctx context.Context) error
	// Installed lists models present on disk, by LocalName.
	Installed(ctx context.Context) (map[string]bool, error)
	Pull(ctx context.Context, m Model, p Progress) error
	// Load keeps a model in memory until Unload.
	Load(ctx context.Context, m Model) error
	Unload(ctx context.Context, m Model) error
	Remove(ctx context.Context, m Model) error
}
