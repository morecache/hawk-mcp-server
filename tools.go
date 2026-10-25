package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file holds the hand-written tools. They exist because auto-generated
// tools are a poor fit for two things: summarising several API responses into
// one compact answer, and multi-step operations with waiting and verification.

// hawkClient is a minimal JSON client for the patched hawk-apiserver.
type hawkClient struct {
	baseURL string
	token   string
}

func (c *hawkClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.baseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

type hawkNameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type hawkResource struct {
	ID             string            `json:"id"`
	Class          string            `json:"class"`
	Provider       string            `json:"provider"`
	Type           string            `json:"type"`
	Agent          string            `json:"agent"`
	Role           string            `json:"role"`
	TargetRole     string            `json:"target_role"`
	Node           string            `json:"node"`
	Active         bool              `json:"active"`
	Managed        bool              `json:"managed"`
	Maintenance    bool              `json:"maintenance"`
	Failed         bool              `json:"failed"`
	FailureIgnored bool              `json:"failure_ignored"`
	Orphaned       bool              `json:"orphaned"`
	Blocked        bool              `json:"blocked"`
	NodesRunningOn int               `json:"nodes_running_on"`
	RunningOn      []string          `json:"running_on"`
	Clone          string            `json:"clone"`
	Params         map[string]string `json:"params"`
	Meta           map[string]string `json:"meta"`
}

type hawkResourceList struct {
	Items []hawkResource `json:"items"`
	Count int            `json:"count"`
}

type hawkNode struct {
	Name             string `json:"name"`
	ID               string `json:"id"`
	Online           bool   `json:"online"`
	Standby          bool   `json:"standby"`
	Maintenance      bool   `json:"maintenance"`
	Unclean          bool   `json:"unclean"`
	IsDC             bool   `json:"is_dc"`
	ResourcesRunning int    `json:"resources_running"`
}

type hawkNodeList struct {
	Items []hawkNode `json:"items"`
	Count int        `json:"count"`
}

type hawkCommandResult struct {
	Action   string   `json:"action"`
	Subject  string   `json:"subject"`
	Command  []string `json:"command"`
	DryRun   bool     `json:"dry_run"`
	Executed bool     `json:"executed"`
	Message  string   `json:"message"`
	Stdout   string   `json:"stdout"`
	Stderr   string   `json:"stderr"`
}

// ---------------------------------------------------------------- resource_explain

type resourceExplainArgs struct {
	ResourceID string `json:"resource_id" jsonschema:"id of the resource to explain, as printed by resources_list"`
}

// resourceExplainResult is deliberately flat: the SDK derives the output schema
// from these Go types and rejects nulls where the schema says object or array,
// so every collection is initialised to an empty value and no nested struct is
// returned.
type resourceExplainResult struct {
	Found        bool     `json:"found"`
	ID           string   `json:"id"`
	Agent        string   `json:"agent"`
	Role         string   `json:"role"`
	TargetRole   string   `json:"target_role"`
	Node         string   `json:"node"`
	Clone        string   `json:"clone"`
	Managed      bool     `json:"managed"`
	Maintenance  bool     `json:"maintenance"`
	Failed       bool     `json:"failed"`
	Orphaned     bool     `json:"orphaned"`
	Blocked      bool     `json:"blocked"`
	Summary      string   `json:"summary"`
	LastFailure  string   `json:"last_failure,omitempty"`
	FailureCount int      `json:"failure_count"`
	Hints        []string `json:"hints"`
	KnownIDs     []string `json:"known_ids"`
}

// resourceExplain answers "why is this resource in this state, and what do I do
// next" in one call, instead of making the caller join resources_list with
// crm_status_get and interpret the flags itself.
func (c *hawkClient) resourceExplain(ctx context.Context, _ *mcp.CallToolRequest, args resourceExplainArgs) (*mcp.CallToolResult, *resourceExplainResult, error) {
	var list hawkResourceList
	if err := c.do(ctx, http.MethodGet, "/api/mcp/v1/resources", nil, &list); err != nil {
		return nil, nil, err
	}

	result := &resourceExplainResult{
		Hints:    []string{},
		KnownIDs: []string{},
	}
	var found *hawkResource
	for i := range list.Items {
		if list.Items[i].ID == args.ResourceID {
			found = &list.Items[i]
			break
		}
	}
	if found == nil {
		ids := make([]string, 0, len(list.Items))
		for _, item := range list.Items {
			ids = append(ids, item.ID)
		}
		sort.Strings(ids)
		result.KnownIDs = ids
		result.Summary = fmt.Sprintf("no resource with id %q; known ids: %s", args.ResourceID, strings.Join(ids, ", "))
		return nil, result, nil
	}

	result.Found = true
	result.ID = found.ID
	result.Role = found.Role
	result.TargetRole = found.TargetRole
	result.Node = found.Node
	result.Clone = found.Clone
	result.Managed = found.Managed
	result.Maintenance = found.Maintenance
	result.Failed = found.Failed
	result.Orphaned = found.Orphaned
	result.Blocked = found.Blocked

	agent := found.Class
	if found.Provider != "" {
		agent += ":" + found.Provider
	}
	if found.Type != "" {
		agent += ":" + found.Type
	}
	result.Agent = agent

	parts := []string{fmt.Sprintf("resource %s (%s)", found.ID, agent)}
	if found.Clone != "" {
		parts = append(parts, "clone="+found.Clone)
	}
	if found.Role != "" {
		parts = append(parts, "role="+found.Role)
	}
	if found.Node != "" {
		parts = append(parts, "on="+found.Node)
	} else {
		parts = append(parts, "not running")
	}
	if found.TargetRole != "" && found.TargetRole != "Unknown" {
		parts = append(parts, "target="+found.TargetRole)
	}
	result.Summary = strings.Join(parts, " ")

	if len(found.Params) > 0 {
		result.Summary += "\nparams: " + formatFlatMap(found.Params)
	}
	if len(found.Meta) > 0 {
		result.Summary += "\nmeta: " + formatFlatMap(found.Meta)
	}

	hints := []string{}
	switch {
	case found.Failed:
		hints = append(hints, "the resource is FAILED: fix the underlying cause, then call resource_cleanup to reset its failure history")
	case found.Maintenance:
		hints = append(hints, "the resource is in MAINTENANCE mode: pacemaker will not recover it; call resource_maintenance_off to hand it back")
	case found.Orphaned:
		hints = append(hints, "the resource is ORPHANED: it runs somewhere pacemaker does not expect; check whether a manual start happened")
	case found.Blocked:
		hints = append(hints, "the resource is BLOCKED by a constraint that cannot be satisfied; check nodes_list for offline or standby nodes")
	}
	if !found.Managed {
		hints = append(hints, "the resource is UNMANAGED: pacemaker ignores it until you set is-managed=true (resource_parameter_set with meta=true)")
	}
	if found.Active && found.Role == "Started" && found.Node == "" {
		hints = append(hints, "the resource reports role=Started but no node; the cluster view is inconsistent, check crm_status_get")
	}
	// Recent operation history: the latest failed operation is usually the most
	// useful single fact when a resource misbehaves.
	history, err := c.resourceHistory(ctx, found.ID)
	if err == nil {
		result.FailureCount = history.failures
		if history.lastFailure != "" {
			result.LastFailure = history.lastFailure
			hints = append(hints, "last recorded failure: "+history.lastFailure)
		}
	}
	result.Hints = hints

	return nil, result, nil
}

// resourceHistory summarises the operation history of one resource.
type historySummary struct {
	failures    int
	lastFailure string
}

func (c *hawkClient) resourceHistory(ctx context.Context, resource string) (historySummary, error) {
	var payload struct {
		Ops []struct {
			Status string `json:"status"`
		} `json:"ops"`
		LastFailure *struct {
			Operation    string `json:"operation"`
			Node         string `json:"node"`
			RCCode       string `json:"rc_code"`
			RCText       string `json:"rc_text"`
			LastRCChange string `json:"last_rc_change"`
			Interval     string `json:"interval"`
		} `json:"last_failure"`
	}

	path := "/api/mcp/v1/resource-history?resource=" + url.QueryEscape(resource) + "&limit=500"
	if err := c.do(ctx, http.MethodGet, path, nil, &payload); err != nil {
		return historySummary{}, err
	}

	summary := historySummary{}
	for _, op := range payload.Ops {
		if op.Status == "failed" {
			summary.failures++
		}
	}
	if payload.LastFailure != nil {
		lf := payload.LastFailure
		detail := fmt.Sprintf("%s on %s rc=%s (%s)", lf.Operation, lf.Node, lf.RCCode, lf.RCText)
		if lf.Interval != "" && lf.Interval != "0" && lf.Interval != "0s" {
			detail += " interval=" + lf.Interval
		}
		if lf.LastRCChange != "" {
			detail += " at " + lf.LastRCChange
		}
		summary.lastFailure = detail
	}
	return summary, nil
}

func formatFlatMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, " ")
}

// ---------------------------------------------------------------- node_drain

type nodeDrainArgs struct {
	Node string `json:"node" jsonschema:"name of the node to drain, for example node-3"`
	// confirm is optional in the schema on purpose: when it is absent the tool
	// returns a structured explanation instead of a protocol-level error, which
	// is easier for a model to act on than "invalid params".
	Confirm        bool `json:"confirm,omitempty" jsonschema:"must be true; without it nothing is changed"`
	TimeoutSeconds int  `json:"timeout_seconds,omitempty" jsonschema:"how long to wait for resources to leave the node, default 120"`
}

type nodeDrainResult struct {
	Node               string   `json:"node"`
	Confirmed          bool     `json:"confirmed"`
	Standby            bool     `json:"standby"`
	Drained            bool     `json:"drained"`
	ResourcesRunning   int      `json:"resources_running"`
	RemainingResources []string `json:"remaining_resources"`
	WaitedSeconds      int      `json:"waited_seconds"`
	Message            string   `json:"message"`
}

// nodeDrain sets a node to standby and waits until its resources have moved
// away, reporting what is left if they do not. It is hand-written because the
// single API call only starts the operation; the interesting part is the wait
// and the verification.
func (c *hawkClient) nodeDrain(ctx context.Context, _ *mcp.CallToolRequest, args nodeDrainArgs) (*mcp.CallToolResult, *nodeDrainResult, error) {
	if args.Node == "" {
		return nil, nil, fmt.Errorf("node must be set")
	}
	timeout := args.TimeoutSeconds
	if timeout <= 0 {
		timeout = 120
	}

	result := &nodeDrainResult{Node: args.Node, Confirmed: args.Confirm, RemainingResources: []string{}}
	if !args.Confirm {
		result.Message = "confirmation required: call again with confirm=true to put " + args.Node + " into standby"
		return nil, result, nil
	}

	path := "/api/mcp/v1/nodes/" + url.PathEscape(args.Node) + "/standby-on"
	var cmd hawkCommandResult
	if err := c.do(ctx, http.MethodPost, path, map[string]any{"confirm": true}, &cmd); err != nil {
		return nil, nil, fmt.Errorf("putting %s into standby failed: %w", args.Node, err)
	}

	start := time.Now()
	deadline := start.Add(time.Duration(timeout) * time.Second)
	for {
		var nodes hawkNodeList
		if err := c.do(ctx, http.MethodGet, "/api/mcp/v1/nodes", nil, &nodes); err != nil {
			return nil, nil, fmt.Errorf("reading node state failed: %w", err)
		}
		var node *hawkNode
		for i := range nodes.Items {
			if nodes.Items[i].Name == args.Node {
				node = &nodes.Items[i]
				break
			}
		}
		if node == nil {
			return nil, nil, fmt.Errorf("node %s is not reported by the cluster", args.Node)
		}

		result.Standby = node.Standby
		result.ResourcesRunning = node.ResourcesRunning
		if node.Standby && node.ResourcesRunning == 0 {
			result.Drained = true
			result.WaitedSeconds = int(time.Since(start).Seconds())
			result.Message = fmt.Sprintf("node %s is drained: standby=true, no resources left", args.Node)
			return nil, result, nil
		}
		if time.Now().After(deadline) {
			result.WaitedSeconds = int(time.Since(start).Seconds())
			remaining, err := c.resourcesOnNode(ctx, args.Node)
			result.RemainingResources = remaining
			if err != nil {
				result.Message = fmt.Sprintf("node %s is in standby=%v but still runs %d resource(s) after %ds; listing them failed: %v",
					args.Node, node.Standby, node.ResourcesRunning, result.WaitedSeconds, err)
				return nil, result, nil
			}
			result.Message = fmt.Sprintf("node %s is in standby=%v but still runs %d resource(s) after %ds",
				args.Node, node.Standby, node.ResourcesRunning, result.WaitedSeconds)
			return nil, result, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// resourcesOnNode lists the ids of resources currently running on a node.
func (c *hawkClient) resourcesOnNode(ctx context.Context, node string) ([]string, error) {
	ids := []string{}
	var list hawkResourceList
	if err := c.do(ctx, http.MethodGet, "/api/mcp/v1/resources", nil, &list); err != nil {
		return ids, err
	}
	for _, r := range list.Items {
		if r.Node == node {
			ids = append(ids, r.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
