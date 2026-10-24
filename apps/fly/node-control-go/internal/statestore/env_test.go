package statestore

import "example.com/node-control/internal/core"

func coreEnv(m map[string]string) core.Env { return core.MapEnv(m) }
