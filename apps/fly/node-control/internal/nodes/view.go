package nodes

// unmanagedStatus is what statusOf reports for a node without a provider (uppercased by the UI).
const unmanagedStatus = "UNMANAGED"

// UIView is the /nodes.json body: labels, status vocabulary and tailnet names,
// never connection details.
func UIView(cfg *Config, ts []Target) map[string]any {
	title, subtitle := cfg.Title, cfg.Subtitle
	if title == "" {
		title = defaultTitle
	}
	if subtitle == "" {
		subtitle = defaultSubtitle
	}
	out := map[string]any{}
	for _, t := range ts {
		entry := cfg.Nodes[t.ID]
		view := map[string]any{"tailnetName": t.TailnetName}
		if t.Provider == "" {
			view["provider"] = firstNonEmpty(entry.Label, statusOnlyLabel)
			// The status of a status-only node is "unmanaged"; index.html disables a
			// button whose target status equals the current one, so both go dark.
			view["startStatus"], view["stopStatus"] = unmanagedStatus, unmanagedStatus
			out[t.ID] = view
			continue
		}
		merged := map[string]any{}
		for k, v := range ProviderDefaults[t.Provider] {
			merged[k] = v
		}
		for _, k := range uiKeys {
			if v, ok := entry.raw[k]; ok {
				merged[k] = v
			}
		}
		for _, k := range uiKeys {
			if v, ok := merged[k]; ok {
				view[k] = v
			}
		}
		label, _ := merged["label"].(string)
		view["provider"] = firstNonEmpty(entry.Label, label)
		out[t.ID] = view
	}
	return map[string]any{"title": title, "subtitle": subtitle, "nodes": out}
}

// TailnetNames is the set of headscale names to report: every target's
// tailnet name plus the configured extras.
func TailnetNames(cfg *Config, ts []Target) map[string]bool {
	names := map[string]bool{}
	for _, t := range ts {
		names[t.TailnetName] = true
	}
	for _, n := range cfg.TailnetTrack {
		names[n] = true
	}
	return names
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
