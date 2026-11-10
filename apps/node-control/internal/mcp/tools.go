package mcp

import (
	"strings"
)

// tool is one MCP tool. It runs one admin route of node-control in this process, as the caller, so
// the route's own permission, validation and audit apply unchanged. Route is the exact ServeMux
// pattern ("POST /nodes/{id}/start"); its permission comes from authz.Routes.
//
// No tool returns a secret: routes that hand out keys, links, invitations or task secrets are left
// out on purpose, because a tool result goes to the model of the client. Roles and grants also stay
// out: they change only in the UI or in ADMIN_CONFIG.
type tool struct {
	name, title, description string
	route                    string
	query                    []queryParam // optional query parameters
	body                     string       // "" for no body; else what the "body" object holds
	readOnly, destructive    bool
	idempotent               bool
}

type queryParam struct {
	name, description string
	integer           bool
}

// pathParams returns the {name} wildcards of the route, in order.
func (t tool) pathParams() []string {
	var out []string
	_, path, _ := strings.Cut(t.route, " ")
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}"))
		}
	}
	return out
}

func (t tool) method() string {
	m, _, _ := strings.Cut(t.route, " ")
	return m
}

// read builds a read-only tool.
func read(name, title, route, description string, query ...queryParam) tool {
	return tool{name: name, title: title, route: route, description: description, query: query,
		readOnly: true, idempotent: true}
}

// change builds a tool that changes state. body says what its "body" argument holds ("" for none).
func change(name, title, route, description, body string, destructive, idempotent bool) tool {
	return tool{name: name, title: title, route: route, description: description, body: body,
		destructive: destructive, idempotent: idempotent}
}

var limitParam = queryParam{name: "limit", description: "maximum number of entries", integer: true}

// tools is the catalogue, in the order of the console's features. A tool whose route is not
// registered (its module is off) is not listed.
var tools = []tool{
	// overview and the console core
	read("tailnet_status", "Tailnet status", "GET /tailnet/status",
		"The nodes headscale knows, whether each is online, and which are tracked exit nodes."),
	read("attention_list", "What needs attention", "GET /_attention.json",
		"The items that need an operator now, and a one-line health of each module. Only what the "+
			"caller may see."),
	read("activity_list", "Activity log", "GET /_activity.json",
		"Recent operator actions of every module, newest first.", limitParam,
		queryParam{name: "source", description: "only this module, for example nodes or policy"}),
	change("attention_acknowledge", "Acknowledge an attention item", "POST /_attention/acknowledge",
		"Mark an attention item as seen, so it stops asking. Take id and ack_token from "+
			"attention_list.",
		`{"id": item id, "ack_token": the item's ack_token, "expires_in_seconds": optional}`,
		false, true),

	// nodes
	read("nodes_list", "List exit nodes", "GET /nodes.json",
		"The exit nodes node-control can start and stop, with their provider and last "+
			"known state."),
	read("node_status", "Exit node status", "GET /nodes/{id}/status",
		"Ask the cloud provider for the current state of one node. id is from nodes_list."),
	change("node_start", "Start an exit node", "POST /nodes/{id}/start",
		"Start the VM or app of one exit node. id is from nodes_list.", "", false, true),
	change("node_stop", "Stop an exit node", "POST /nodes/{id}/stop",
		"Stop the VM or app of one exit node. Clients that use it lose their exit until it "+
			"starts again.", "", true, true),

	// enrolment
	read("devices_list", "List enrolled devices", "GET /enroll/devices",
		"The devices enrolled through bridges, with expiry and state. No links or secrets."),
	change("device_revoke", "Revoke a device", "POST /enroll/devices/{id}/revoke",
		"Revoke one enrolled device: it cannot connect any more. id is from devices_list.",
		"", true, true),
	change("device_extend", "Extend a device", "POST /enroll/devices/{id}/extend",
		"Move the expiry of one enrolled device.",
		`{"expires_at": Unix time in seconds, in the future}`, false, true),
	read("ingresses_list", "List bridges", "GET /enroll/ingresses",
		"The bridges (ingresses) that reported in, and whether each is approved."),
	read("people_list", "List people", "GET /enroll/people",
		"The people who enrolled themselves with OIDC, and whether each is locked."),
	change("person_lock", "Lock a person", "POST /enroll/people/{sub}/lock",
		"Lock one self-enrolled person: their devices stop working. sub is from people_list.",
		`{"reason": optional text, at most 200 characters}`, true, true),
	change("person_unlock", "Unlock a person", "POST /enroll/people/{sub}/unlock",
		"Unlock one self-enrolled person.", "", false, true),
	read("invites_list", "List invitations", "GET /enroll/invites",
		"The open invitations, without their links."),
	change("invite_revoke", "Revoke an invitation", "POST /enroll/invites/{id}/revoke",
		"Revoke one invitation, so its link stops working.", "", true, true),

	// tasks
	read("tasks_list", "List tasks", "GET /tasks.json",
		"The scheduled browser-script tasks and the state of their last run."),
	read("task_get", "Get a task", "GET /tasks/{id}", "One task, with its schedule and state."),
	read("task_runs", "Runs of a task", "GET /tasks/{id}/runs", "The recent runs of one task."),
	read("run_get", "Get a run", "GET /runs/{id}", "One run, with its attempts and result."),
	change("task_run", "Run a task now", "POST /tasks/{id}/run",
		"Start one run of a task now, outside its schedule.", "", false, false),
	change("run_cancel", "Cancel a run", "DELETE /runs/{id}",
		"Cancel a queued or running run.", "", true, true),
	read("tasks_config_get", "Get the task configuration", "GET /tasks/config",
		"The task configuration as stored. Secret values are not in it."),
	read("sessions_list", "List browser sessions", "GET /sessions.json",
		"The browser sessions on the session nodes."),
	change("session_start", "Start a browser session", "POST /sessions/{node}/start",
		"Start a browser session on one node.",
		`{"ttl_seconds": optional, "idle_seconds": optional}`, false, false),
	change("session_stop", "Stop a browser session", "POST /sessions/{node}/stop",
		"Stop the browser session on one node.", "", true, true),

	// headscale policy
	read("policy_get", "Get the headscale policy", "GET /policy/state",
		"The current headscale policy, its updated_at, the pending change if any, and history."),
	{name: "policy_preview", title: "Preview a policy", route: "POST /policy/preview",
		description: "Check a new policy without applying it: is it valid, the diff and " +
			"the warnings.",
		body:     `{"policy": the whole new policy, as a string}`,
		readOnly: true, idempotent: true},
	change("policy_apply", "Apply a policy", "POST /policy/apply",
		"Apply a new headscale policy on probation. It is reverted at the deadline unless "+
			"policy_confirm runs first. Preview it first.",
		`{"policy": the whole new policy, as a string, `+
			`"base_updated_at": updated_at from policy_get}`,
		true, false),
	change("policy_confirm", "Confirm a policy change", "POST /policy/confirm",
		"Keep the pending policy change.", `{"change_id": the id from policy_apply}`, false, true),
	change("policy_revert_pending", "Revert the pending policy change", "POST /policy/revert-now",
		"Undo the pending policy change now.", `{"change_id": the pending change id}`, true, true),
	change("policy_revert_to", "Revert to an earlier policy", "POST /policy/revert/{id}",
		"Apply an earlier policy from the history again, on probation. id is a history id.",
		"", true, false),

	// funnel
	read("funnel_state", "Funnel state", "GET /funnel/state",
		"The public names of the funnel: namespaces, routes, blocks and the ingress state."),
	read("funnel_audit", "Funnel audit", "GET /funnel/audit", "The funnel's own audit log."),
	change("funnel_route_put", "Add or change a funnel route", "POST /funnel/routes",
		"Publish a tailnet service under a public name.",
		`{"ns", "service", "target": tailnet host, "port", "scheme": "http" or "https", `+
			`"insecure_skip_verify": optional}`, false, true),
	change("funnel_route_delete", "Remove a funnel route", "DELETE /funnel/routes/{ns}/{service}",
		"Remove one public route.", "", true, true),
	change("funnel_block_put", "Block a funnel name", "POST /funnel/blocks",
		"Refuse one public name.", `{"ns", "service", "reason": optional}`, true, true),
	change("funnel_block_delete", "Unblock a funnel name", "DELETE /funnel/blocks/{ns}/{service}",
		"Remove one block.", "", false, true),

	// serve and access (read only)
	read("serve_state", "Serve state", "GET /serve/state",
		"The tailnet services of the serve module and their keys (ids and dates, no key values)."),
	read("access_state", "Roles and grants", "GET /admin/state",
		"The roles and who holds them. Read only: roles change only in the console or "+
			"ADMIN_CONFIG."),
	read("admin_audit", "Admin audit log", "GET /admin/audit", "Changes to roles and grants."),
}
