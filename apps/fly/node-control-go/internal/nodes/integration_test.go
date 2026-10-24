package nodes

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"example.com/node-control/internal/headscale"
)

func discoverFrom(t *testing.T, js string, ns []headscale.Node) []Target {
	t.Helper()
	return Discover(mustParse(t, js), ns)
}

// The example file and a Selectel variant go through ParseConfig -> Discover
// -> provider, so a block shape mismatch between the layers cannot hide.
func TestExampleConfigBlocksReachProviderRequests(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/nodes.example.json")
	if err != nil {
		t.Fatal(err)
	}
	ns := []headscale.Node{
		hs("node-a", true, "tag:exit", "tag:cloud-yandex"),
		hs("node-b", true, "tag:exit", "tag:cloud-oci"),
		hs("node-c", true, "tag:exit", "tag:cloud-fly"),
		hs("node-d", true, "tag:exit", "tag:cloud-selectel"),
	}
	// the shipped placeholder region is deliberately not a valid identifier
	ts := discoverFrom(t, strings.ReplaceAll(string(raw), "<oci-region>", "region-1"), ns)
	vmA, ok := byID(ts, "vm-a")
	if !ok {
		t.Fatalf("targets %v", ids(ts))
	}
	vmB, _ := byID(ts, "vm-b")
	ctx := context.Background()

	// Yandex: pinned instance_id, no folder in env -> must not be not_configured.
	fy := &fakeYandex{instances: []yxInstance{{"<yandex-instance-id>", "other", "RUNNING"}}}
	env := yxEnv(t)
	delete(env, "YANDEX_FOLDER_ID")
	py, _ := newYandex(t, fy, env)
	st, err := py.Status(ctx, vmA)
	if err != nil || st["status"] != "RUNNING" || len(fy.lists) != 0 {
		t.Fatalf("yandex pinned id ignored: %v %v lists=%v", st, err, fy.lists)
	}

	// OCI: block compartment and instance_name reach the list request.
	fo := &fakeOCI{instances: []map[string]string{ociInst("o1", "vm-b", "RUNNING")}}
	po := newOCI(t, fo, ociEnv(t))
	st, err = po.Status(ctx, vmB)
	if err != nil || st["status"] != "RUNNING" || len(fo.lists) != 1 {
		t.Fatalf("oci: %v %v %v", st, err, fo.lists)
	}
	q, _ := url.ParseQuery(fo.lists[0])
	if q.Get("compartmentId") != "<oci-compartment-ocid>" || q.Get("displayName") != "vm-b" {
		t.Fatalf("oci block not applied: %v", q)
	}

	// Selectel: pinned id and region from the block.
	sel := discoverFrom(t, `{"nodes":{"s1":{"provider":"selectel","tailnetName":"node-d","selectel":{"instance_id":"pinned","region":"region-2"}}}}`, ns)
	s1, _ := byID(sel, "s1")
	fn := &fakeNova{region: "region-2", status: map[string]string{"pinned": "ACTIVE"}}
	ps, _ := newSelectel(t, fn, selEnv)
	st, err = ps.Status(ctx, s1)
	if err != nil || st["status"] != "RUNNING" || len(fn.queries) != 0 {
		t.Fatalf("selectel block ignored: %v %v %v", st, err, fn.queries)
	}
}
