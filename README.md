# hawk-mcp-server

MCP server that exposes the Hawk cluster API (read endpoints plus confirmed
write actions) as MCP tools.

It is a thin adapter on purpose:

* the tool surface is defined by `openapi.yaml` in this directory (embedded into
  the binary at build time) plus two hand-written tools registered in `main.go`
  (`resource_explain` and `node_drain`);
* every operation in that document becomes one MCP tool through
  [github.com/evcc-io/openapi-mcp](https://github.com/evcc-io/openapi-mcp),
  which performs the real HTTP call;
* the HTTP endpoints are served by the `mcp-api` branch of `hawk-apiserver` under
  `/api/mcp/v1`, authenticated with a static bearer token;
* the library also registers an `info` tool that returns this API's
  metadata (title, version, description); it never reads cluster state;
* the library appends `AUTHENTICATION`/`PARAMETERS`/`EXAMPLE` blocks to
  every tool description. The example request body is a placeholder and does not
  contain `confirm`; a write call needs `{"confirm": true}` (optionally
  `{"dry_run": true}`), otherwise it is answered with 409.

## Prerequisites

1. A patched `hawk-apiserver` build that serves `/api/mcp/v1` (the `mcp-api`
   branch of `/root/go/src/hawk-apiserver`). Start it on its own port so it does
   not collide with the packaged `hawk.service` on 7630:

   ```
   HAWK_MCP_TOKEN_FILE=/etc/hawk/mcp.token \
   PATH/TO/hawk-apiserver \
       -key /etc/hawk/hawk.key -cert /etc/hawk/hawk.pem -port 7630
   ```

2. A bearer token file (mode 0600), for example:

   ```
   umask 077
   head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > /etc/hawk/mcp.token
   ```

3. A running pacemaker/corosync cluster. When the cluster is stopped or still
   initializing, the endpoints answer `503 cluster_unavailable` instead of
   pretending the cluster is empty.

## Build

```
go build -o hawk-mcp-server .
```

## Run

```
# Streamable HTTP on 127.0.0.1:9000, clients connect to http://127.0.0.1:9000/mcp
./hawk-mcp-server -hawk-url https://127.0.0.1:7630 -token-file /etc/hawk/mcp.token -http 127.0.0.1:9000

# stdio transport (for clients that spawn the server themselves)
./hawk-mcp-server -hawk-url https://127.0.0.1:7630 -token-file /etc/hawk/mcp.token
```

Flags:

| flag | default | meaning |
|---|---|---|
| `-hawk-url` | *(required)* | base URL of the hawk-apiserver `mcp-api` branch |
| `-http` | *(empty)* | Streamable HTTP listen address; empty means stdio |
| `-token-file` | `/etc/hawk/mcp.token` | file with the bearer token |
| `-ca-file` | `/etc/hawk/hawk.pem` | certificate to trust for hawk's self-signed TLS; empty string keeps the system trust store |
| `-insecure` | `false` | skip TLS verification (testing only) |

## Tools

### Read-only cluster and resource overview

| tool | backed by | use it for |
|---|---|---|
| `cluster_get` | `GET /api/mcp/v1/cluster` | one-call cluster health: status, DC, quorum, counts, failed resources, offline/unclean/maintenance nodes |
| `crm_status_get` | `GET /api/mcp/v1/crm/status` | full parsed `crm status`: every node and resource instance with live state |
| `nodes_list` | `GET /api/mcp/v1/nodes` | nodes plus their CIB attributes and utilization values |
| `resources_list` | `GET /api/mcp/v1/resources` | resources with parameters, meta attributes and live state |

### Write actions

Each action is one tool generated from `openapi.yaml`. **All of them require
`{"confirm": true}` in the request body** (which the generated tool exposes as a
nested `requestBody` argument); without it the call is rejected with HTTP 409 and
nothing happens. Add `{"dry_run": true}` to get the exact `crm` command without
executing it.

| tool | runs | notes |
|---|---|---|
| `resource_start` / `resource_stop` | `crm --force resource start/stop <id>` | stopping a fencing resource disables fencing while it is stopped |
| `resource_cleanup` | `crm --force resource cleanup <id> [<node>]` | clears the failure history so the resource may run again |
| `resource_clear` | `crm --force resource clear <id>` | removes a placement constraint left by `migrate`/`move` |
| `resource_migrate` | `crm --force resource migrate <id> <destination>` | needs `destination`; the move is sticky until `resource_clear` |
| `resource_maintenance_on` / `resource_maintenance_off` | `crm --force resource maintenance <id> [off]` | pacemaker stops recovering the resource |
| `resource_delete` | `crm --force configure delete <id>` | no undo |
| `node_standby_on` / `node_standby_off` | `crm --force node standby/online <name>` | resources are moved away and not scheduled back |
| `node_maintenance_on` / `node_maintenance_off` | `crm --force node maintenance/ready <name>` | resources on the node are frozen in place |
| `node_fence` | `crm --force node fence <name>` | power-cycles the node; the most disruptive action |
| `node_clearstate` | `crm --force node clearstate <name>` | clears a stale UNCLEAN flag |

### Configuration writes

These change CIB definitions instead of runtime state, and follow the same
confirm/dry-run contract as the actions above.

| tool | runs | notes |
|---|---|---|
| `resource_create` | `crm configure primitive <id> <class>[:<provider>]:<type> [k=v ...] [meta k=v ...]` | create-only: an existing id is answered with 409 `already_exists` and nothing changes |
| `resource_parameter_set` | `crm resource param\|meta <id> set\|delete <name> [<value>]` | one parameter or meta attribute at a time; `meta: true` targets meta attributes, `delete: true` removes |
| `node_attribute_set` | `crm node attribute <node> set <name> <value>` | location rules may react, so resources can move |
| `node_attribute_delete` | `crm node attribute <node> delete <name>` | a missing attribute is not an error: the delete is idempotent (200, updated=false) |

**Changing an existing resource is done with `resource_parameter_set`, not by
re-creating it.** `crm configure primitive` refuses to touch an existing id, and
re-creating would drop the resource history and its placement.

### Groups, clones and constraints

| tool | runs | notes |
|---|---|---|
| `resource_groups_list` | `GET /api/mcp/v1/resource-groups` | lists groups, their members and meta attributes |
| `resource_group_create` | `crm configure group <id> <members...> [meta k=v ...]` | create-only; members must be top-level primitives and their order is the start order |
| `resource_group_member_add` / `resource_group_member_remove` | `crm configure modgroup <group> add\|remove <resource>` | the supported way to change membership; removing a resource leaves it configured |
| `resource_clones_list` | `GET /api/mcp/v1/resource-clones` | lists clones and the resource they wrap |
| `resource_clone_create` | `crm configure clone <id> <resource\|group> [meta k=v ...]` | create-only; the child cannot be changed in place, and deleting a clone keeps the child |
| `resource_constraints_list` | `GET /api/mcp/v1/resource-constraints` | lists constraints with their type and attributes |
| `resource_constraint_create` | `crm configure location\|colocation\|order\|rsc_ticket ...` (rule-based locations use `cibadmin --modify`) | create-only; `type` selects the form and the required fields; a location may carry a rule instead of node/score, and `rsc_ticket` makes a resource depend on a ticket |

Semantics worth knowing, all verified against crmsh on a live cluster:

* Every configuration object is **create-only**. An existing id is answered with
  409 `already_exists`; "replace" means delete and create again.
* A constraint target must be a top-level primitive, a group or a clone. A
  resource inside a group cannot carry a constraint directly (crmsh calls it
  ambiguous), so target the group instead; the API rejects the member case with
  a hint that names the container.
* `resources_list` reports the containing `group` as well as `clone`, and
  group members are included in the list instead of silently missing.
* A `rsc_ticket` constraint makes a resource depend on a ticket (`ticket`, with
  optional `role` and `loss_policy` stop/demote/fence/freeze). The resource only
  runs while the ticket is granted to this site, so creating one against an
  ungranted ticket stops the resource: grant the ticket with
  `crm_ticket -t <ticket> -g` first if the resource must keep running.
* Deleting a group or clone through `resource_delete` keeps its members; only
  the group/clone element goes away.
* Rules nest: a rule may contain `rules`, which is how compound conditions
  (a group of AND/OR sub-expressions) are expressed; nesting is limited to three
  levels. `score_attribute` takes the score from a node attribute instead of a
  fixed `score` (exactly one of the two is required per rule).
* A location constraint may carry a **rule** instead of node/score:
  `score` (a number, or `inf`/`-inf`) or `score_attribute` is required, exactly one of them; conditions are `expressions`
  (attribute comparisons such as `#uname eq node-3`, or `defined`/`not_defined`),
  `date_spec` (recurring window with `hours`, `weekdays`, `months`, ...) and
  `in_range` (`start`/`end`), combined by `boolean_op` (default `or`).
  Rules apply to location constraints only, and `node`/`score` must not be
  combined with a rule. Rule-based locations are written with
  `cibadmin --modify --xpath //constraints`, because crmsh has no grammar for
  date expressions and `cibadmin --create` rejects even schema-valid fragments
  on this pacemaker version. Unknown rule fields (for example `days` instead of
  `weekdays`) are rejected instead of silently ignored.

### Resource operations and cluster defaults

| tool | runs | notes |
|---|---|---|
| `resource_operation_set` | `cibadmin --modify\|--replace\|--delete` | one operation of a primitive; op id is `<resource>-<name>-<interval>`, and a bare interval number means seconds |
| `cluster_properties_list` | reads `crm_config`, `rsc_defaults`, `op_defaults` | see which defaults are in effect |
| `cluster_property_set` | `crm configure property\|rsc_defaults\|op_defaults <name>=<value>` | delete uses `cibadmin --delete` because crmsh rejects an empty value |

Operations are edited through `cibadmin` on purpose: `crm configure op` is an
alias for `op_defaults` and cannot change one resource's operations. Setting an
operation replaces the whole `op` element, so omitted attributes are reset;
deleting the last operation also removes the empty `operations` element, and
deleting an operation that is not configured is a no-op.

Property scopes are `cluster` (default), `rsc_defaults` and `op_defaults`.
Some values have consequences the API cannot check: `stonith-enabled=false`
disables fencing, and defaults such as `resource-stickiness` or
`migration-threshold` change how the scheduler behaves cluster-wide.

### Resource history

| tool | reads | notes |
|---|---|---|
| `resource_history_get` | CIB `<status>` `lrm_rsc_op` entries | operations newest first, with node, rc code and its meaning, exec time and the time the status last changed; `last_failure` repeats the latest failed one |

`resource_explain` reads the same data, so its answer carries `last_failure` and
`failure_count` next to the flags; it covers the most recent 500 recorded
operations. Typical exit codes:
0 ok, 2 invalid parameter, 6 not configured, 7 not running, 124/125 timeout, 190/191 degraded,
126/127 the agent could not be executed.

### Hand-written tools

These exist because auto-generated tools cannot combine several API responses
into one answer, or wait for an operation to finish.

| tool | what it does |
|---|---|
| `resource_explain` | one call that returns the resource's agent, role, node, parameters, meta attributes and actionable hints for failed/unmanaged/orphaned/blocked resources. Takes `resource_id`. |
| `node_drain` | puts a node into standby, polls until it runs nothing (default 120 s), and reports either `drained: true` or the ids of the resources still running on the node. Requires `confirm`; without it returns an explanation and changes nothing. |

## Trying it

MCP Inspector:

```
npx @modelcontextprotocol/inspector
# Transport Type: Streamable HTTP
# URL: http://127.0.0.1:9000/mcp
```

Raw JSON-RPC over the Streamable HTTP endpoint:

```
curl -sS -X POST http://127.0.0.1:9000/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}' -D /tmp/hdr.txt
# then use the Mcp-Session-Id response header for tools/list and tools/call
```

Previewing a write without touching the cluster (note the nested `requestBody`):

```
curl -sS -X POST http://127.0.0.1:9000/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -H "Mcp-Session-Id: $SID" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"resource_stop","arguments":{"id":"stonith-sbd","requestBody":{"confirm":true,"dry_run":true}}}}'
```

## Notes and limitations

* **Confirmation semantics are ours, not openapi-mcp's.** In the upstream
  library (v0.6.0) the "dangerous action confirmation" runs *after* the HTTP
  request has already been sent, so it is not a guard. The real gate is the `confirm` flag
  enforced by hawk-apiserver, which rejects the request before running anything.
* `node_fence` is the only tool that calls itself "the most disruptive action this
  API offers"; the other state-changing tools describe their effect individually
  (stopping a resource, moving resources off a node, freezing a node's resources,
  and so on). Mention the intent to the user before calling any of them.
* **Not exposed yet:** remote and bundle resources, ticket constraints, rule
  expressions (time/date-based constraints), and the textual `crm history`
  report (the API exposes the recorded operations themselves, not the narrative).
* Resource parameters are passed to crmsh as a single argv token, so values
  containing double quotes or line breaks are rejected rather than escaped.
* In stdio mode, route the library's stdout chatter away (`./hawk-mcp-server ...
  >/dev/null`) if your client cannot tolerate extra output on the protocol
  channel; the Streamable HTTP transport does not have this problem.
* `openapi.yaml` is the source of truth for the generated tools; the two
  hand-written tools live in `main.go` and `tools.go`. If they and the server
  disagree, the tools silently return whatever the server sends, so keep them in
  sync.
