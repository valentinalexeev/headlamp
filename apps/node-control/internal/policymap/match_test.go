package policymap

import (
	"reflect"
	"testing"
)

const matchPolicy = `{
  // HuJSON, as headscale accepts it
  "groups": {
    "group:ops": ["alice@"],
    "group:empty": [],
    "group:outer": ["group:ops"],
  },
  "tagOwners": {"tag:web": ["group:ops"], "tag:spare": ["admin@"]},
  "acls": [
    {"action": "accept", "src": ["group:ops", "group:empty", "bob@", "carol@example.com"], "dst": ["tag:web:443"]},
  ],
}`

func unmatchedIDs(t *testing.T, devices []Device) []string {
	t.Helper()
	g, err := Build(matchPolicy)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, n := range Unmatched(g, devices) {
		ids = append(ids, n.ID)
	}
	return ids
}

func TestUnmatched(t *testing.T) {
	for _, tc := range []struct {
		name    string
		devices []Device
		want    []string
	}{
		{"no devices: every tag, group and rule user", nil,
			[]string{"bob@", "carol@example.com", "group:empty", "group:ops", "group:outer", "tag:spare", "tag:web"}},
		{"a tag on a device clears that tag only", []Device{{Tags: []string{"tag:web"}}},
			[]string{"bob@", "carol@example.com", "group:empty", "group:ops", "group:outer", "tag:spare"}},
		{"a member's device clears the group and the group that holds it", []Device{{Owner: []string{"alice@", "alice"}}},
			[]string{"bob@", "carol@example.com", "group:empty", "tag:spare", "tag:web"}},
		{"a user matches by name or by e-mail", []Device{{Owner: []string{"bob@"}}, {Owner: []string{"carol", "carol@example.com"}}},
			[]string{"group:empty", "group:ops", "group:outer", "tag:spare", "tag:web"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// admin@ is only a tag owner: it needs no device, so it is never in the list.
			if got := unmatchedIDs(t, tc.devices); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnmatchedGroupLoopEnds(t *testing.T) {
	g := Graph{Nodes: []Node{
		{ID: "group:a", Kind: KindGroup, Members: []string{"group:b"}},
		{ID: "group:b", Kind: KindGroup, Members: []string{"group:a"}},
	}}
	if got := Unmatched(g, nil); len(got) != 2 {
		t.Fatalf("%+v", got)
	}
}
