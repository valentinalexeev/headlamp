package statestore

import "github.com/valentinalexeev/headlamp/apps/node-control/internal/core"

func coreEnv(m map[string]string) core.Env { return core.MapEnv(m) }
