package statestore

func coreEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
