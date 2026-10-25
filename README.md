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

* **Generated from the spec.** Every operation in `openapi.yaml` becomes one
  tool; write operations require `{"confirm": true}` in the request body.
* In stdio mode, route the library's stdout chatter away (`./hawk-mcp-server ...
  >/dev/null`) if your client cannot tolerate extra output on the protocol
  channel; the Streamable HTTP transport does not have this problem.
* `openapi.yaml` is the single source of truth. If it and the server disagree,
  the tools silently return whatever the server sends, so keep them in sync.
