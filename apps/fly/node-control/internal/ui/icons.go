package ui

// iconPaths are 24x24 line-icon outlines (stroke 1.75, round caps). hl.js carries the same table
// for icons built in the browser; TestIconPathsMatchScript keeps the two in step.
var iconPaths = map[string]string{
	"overview": "M3 3h7v9H3zM14 3h7v5h-7zM14 12h7v9h-7zM3 16h7v5H3z",
	"nodes":    "M4 3h16a1 1 0 0 1 1 1v5a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1zM4 14h16a1 1 0 0 1 1 1v5a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-5a1 1 0 0 1 1-1zM7 6.5h.01M7 17.5h.01",
	"devices":  "M7 2h10a2 2 0 0 1 2 2v16a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2zM12 18h.01",
	"funnel":   "M2 12a10 10 0 1 0 20 0a10 10 0 1 0 -20 0M2 12h20M12 2a15 15 0 0 1 0 20M12 2a15 15 0 0 0 0 20",
	"tasks":    "m3 17 2 2 4-4M3 7l2 2 4-4M13 6h8M13 12h8M13 18h8",
	"sessions": "M3 4h18a1 1 0 0 1 1 1v12a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1zM8 22h8M12 18v4M10 8l5 3-5 3z",
	"policy":   "M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z",
	"key":      "M2 15.5a5.5 5.5 0 1 0 11 0a5.5 5.5 0 1 0 -11 0M21 2l-9.6 9.6M15.5 7.5l3 3L22 7l-3-3",
	"activity": "M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8M3 3v5h5M12 7v5l4 2",
	"search":   "M3 11a8 8 0 1 0 16 0a8 8 0 1 0 -16 0m21 21-4.3-4.3",
	"plus":     "M5 12h14M12 5v14",
	"system":   "M3 3h18a1 1 0 0 1 1 1v12a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1zM8 21h8M12 17v4",
	"sun":      "M8 12a4 4 0 1 0 8 0a4 4 0 1 0 -8 0M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41",
	"moon":     "M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9z",
	"play":     "M6 3l14 9-14 9z",
	"stop":     "M6 6h12v12H6z",
	"ban":      "M2 12a10 10 0 1 0 20 0a10 10 0 1 0 -20 0M4.93 4.93l14.14 14.14",
	"trash":    "M3 6h18M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2",
	"extend":   "M8 2v4M16 2v4M21 11V6a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h7M3 10h18M16 19h6M19 16v6",
	"link":     "M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71",
	"copy":     "M10 8h10a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H10a2 2 0 0 1-2-2V10a2 2 0 0 1 2-2zM4 16a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2",
	"close":    "M18 6 6 18M6 6l12 12",
	"external": "M15 3h6v6M10 14 21 3M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6",
	"menu":     "M4 6h16M4 12h16M4 18h16",
	"check":    "M20 6 9 17l-5-5",
	"undo":     "M3 7v6h6M21 17a9 9 0 0 0-9-9 9 9 0 0 0-6 2.3L3 13",
	"edit":     "M12 20h9M16.5 3.5a2.12 2.12 0 0 1 3 3L7 19l-4 1 1-4z",
}

// Icon returns the 16 px line icon called name as trusted inline SVG, or "" for an unknown name.
// It is decorative (aria-hidden) and takes the colour of the text beside it. The markup is
// built only from the fixed table above, so it is safe to place in a page unescaped.
func Icon(name string) string {
	d, ok := iconPaths[name]
	if !ok {
		return ""
	}
	return `<svg class="ico" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="` + d + `"></path></svg>`
}
