package proxy

import (
	"fmt"
	"strings"
)

type completionGuardToolKind string

type completionGuardToolState string

const (
	completionGuardToolKindClient completionGuardToolKind = "client"
	completionGuardToolKindServer completionGuardToolKind = "server"
	completionGuardToolKindMCP    completionGuardToolKind = "mcp"

	completionGuardToolStatePending  completionGuardToolState = "pending"
	completionGuardToolStateResolved completionGuardToolState = "resolved"
)

type completionGuardToolNode struct {
	ID              string
	Kind            completionGuardToolKind
	Name            string
	ServerName      string
	ResultBlockType string
	ParentID        string
	CallerType      string
	State           completionGuardToolState
	CanCallTools    bool
	Inherited       bool
	DirectDeferred  bool
	MessageIndex    int
}

type completionGuardAssistantTurn struct {
	MessageIndex       int
	ClientCalls        map[string]*completionGuardToolNode
	PendingServerCalls map[string]*completionGuardToolNode
	PendingMCPCalls    map[string]*completionGuardToolNode
}

type completionGuardToolGraph struct {
	Nodes      map[string]*completionGuardToolNode
	Children   map[string]map[string]struct{}
	Turns      map[int]*completionGuardAssistantTurn
	References []completionGuardToolReference
}

func newCompletionGuardToolGraph() *completionGuardToolGraph {
	return &completionGuardToolGraph{
		Nodes:    make(map[string]*completionGuardToolNode),
		Children: make(map[string]map[string]struct{}),
		Turns:    make(map[int]*completionGuardAssistantTurn),
	}
}

func (g *completionGuardToolGraph) clone() *completionGuardToolGraph {
	if g == nil {
		return newCompletionGuardToolGraph()
	}
	out := newCompletionGuardToolGraph()
	for id, node := range g.Nodes {
		if node == nil {
			continue
		}
		copyNode := *node
		out.Nodes[id] = &copyNode
	}
	for parentID, children := range g.Children {
		copyChildren := make(map[string]struct{}, len(children))
		for childID := range children {
			copyChildren[childID] = struct{}{}
		}
		out.Children[parentID] = copyChildren
	}
	for index, turn := range g.Turns {
		if turn == nil {
			continue
		}
		copyTurn := &completionGuardAssistantTurn{
			MessageIndex:       turn.MessageIndex,
			ClientCalls:        make(map[string]*completionGuardToolNode),
			PendingServerCalls: make(map[string]*completionGuardToolNode),
			PendingMCPCalls:    make(map[string]*completionGuardToolNode),
		}
		for id := range turn.ClientCalls {
			copyTurn.ClientCalls[id] = out.Nodes[id]
		}
		for id := range turn.PendingServerCalls {
			copyTurn.PendingServerCalls[id] = out.Nodes[id]
		}
		for id := range turn.PendingMCPCalls {
			copyTurn.PendingMCPCalls[id] = out.Nodes[id]
		}
		out.Turns[index] = copyTurn
	}
	out.References = make([]completionGuardToolReference, len(g.References))
	for index, reference := range g.References {
		out.References[index] = reference
		out.References[index].Resolution.CandidateMCPs = append(
			[]string(nil),
			reference.Resolution.CandidateMCPs...,
		)
	}
	return out
}

func (g *completionGuardToolGraph) add(node completionGuardToolNode) string {
	if g == nil {
		return "tool_graph_unavailable"
	}
	node.ID = strings.TrimSpace(node.ID)
	node.Name = strings.TrimSpace(node.Name)
	node.ServerName = strings.TrimSpace(node.ServerName)
	node.ParentID = strings.TrimSpace(node.ParentID)
	node.ResultBlockType = strings.TrimSpace(node.ResultBlockType)
	if node.ID == "" {
		return "tool_without_id"
	}
	if _, exists := g.Nodes[node.ID]; exists {
		return "duplicate_tool_id"
	}
	if node.State == "" {
		node.State = completionGuardToolStatePending
	}
	copyNode := node
	g.Nodes[node.ID] = &copyNode
	if node.ParentID != "" {
		if g.Children[node.ParentID] == nil {
			g.Children[node.ParentID] = make(map[string]struct{})
		}
		g.Children[node.ParentID][node.ID] = struct{}{}
		// A parent that produces a caller-linked child is no longer a simple
		// direct deferred server call. Its result may appear only after the child
		// dependency closes, not necessarily as the first block of the resumed
		// response.
		if parent, exists := g.activeNode(node.ParentID); exists && parent.CanCallTools {
			parent.DirectDeferred = false
		}
	}
	return ""
}

func (g *completionGuardToolGraph) activeNode(id string) (*completionGuardToolNode, bool) {
	if g == nil {
		return nil, false
	}
	node, exists := g.Nodes[strings.TrimSpace(id)]
	return node, exists && node != nil && node.State == completionGuardToolStatePending
}

func (g *completionGuardToolGraph) activeCodeExecution(id string) bool {
	node, exists := g.activeNode(id)
	return exists && node.Kind == completionGuardToolKindServer &&
		node.Name == "code_execution" && node.CanCallTools
}

func (g *completionGuardToolGraph) hasChildren(parentID string) bool {
	if g == nil {
		return false
	}
	return len(g.Children[strings.TrimSpace(parentID)]) > 0
}

func (g *completionGuardToolGraph) hasPendingChildren(parentID string) bool {
	if g == nil {
		return false
	}
	for childID := range g.Children[strings.TrimSpace(parentID)] {
		if child, exists := g.activeNode(childID); exists && child != nil {
			return true
		}
	}
	return false
}

func (g *completionGuardToolGraph) hasPendingClientChild(parentID string) bool {
	if g == nil {
		return false
	}
	for childID := range g.Children[strings.TrimSpace(parentID)] {
		if child, exists := g.activeNode(childID); exists &&
			child.Kind == completionGuardToolKindClient {
			return true
		}
	}
	return false
}

func (g *completionGuardToolGraph) resolve(id string) string {
	node, exists := g.activeNode(id)
	if !exists {
		return "unknown_or_completed_tool_result"
	}
	if g.hasPendingChildren(node.ID) {
		return "tool_parent_closed_with_pending_children"
	}
	node.State = completionGuardToolStateResolved
	return ""
}

func (g *completionGuardToolGraph) pendingServerCount() int {
	count := 0
	if g == nil {
		return count
	}
	for _, node := range g.Nodes {
		if node != nil && node.State == completionGuardToolStatePending &&
			(node.Kind == completionGuardToolKindServer || node.Kind == completionGuardToolKindMCP) {
			count++
		}
	}
	return count
}

func (g *completionGuardToolGraph) pendingServerValues() map[string]string {
	pending := make(map[string]string)
	if g == nil {
		return pending
	}
	for id, node := range g.Nodes {
		if node == nil || node.State != completionGuardToolStatePending {
			continue
		}
		switch node.Kind {
		case completionGuardToolKindMCP:
			pending[id] = completionGuardMCPServerToolPrefix + node.Name
		case completionGuardToolKindServer:
			pending[id] = node.Name
		}
	}
	return pending
}

func (g *completionGuardToolGraph) clientCallsFromMessage(
	messageIndex int,
) map[string]*completionGuardToolNode {
	calls := make(map[string]*completionGuardToolNode)
	if g == nil {
		return calls
	}
	for id, node := range g.Nodes {
		if node == nil || node.Kind != completionGuardToolKindClient ||
			node.MessageIndex != messageIndex {
			continue
		}
		calls[id] = node
	}
	return calls
}

func (g *completionGuardToolGraph) snapshotAssistantTurn(messageIndex int) {
	if g == nil {
		return
	}
	turn := &completionGuardAssistantTurn{
		MessageIndex:       messageIndex,
		ClientCalls:        make(map[string]*completionGuardToolNode),
		PendingServerCalls: make(map[string]*completionGuardToolNode),
		PendingMCPCalls:    make(map[string]*completionGuardToolNode),
	}
	for id, node := range g.Nodes {
		if node == nil || node.State != completionGuardToolStatePending {
			continue
		}
		switch node.Kind {
		case completionGuardToolKindClient:
			if node.MessageIndex == messageIndex {
				turn.ClientCalls[id] = node
			}
		case completionGuardToolKindServer:
			turn.PendingServerCalls[id] = node
		case completionGuardToolKindMCP:
			turn.PendingMCPCalls[id] = node
		}
	}
	g.Turns[messageIndex] = turn
}

func completionGuardValidateAndResolveUserToolResults(
	graph *completionGuardToolGraph,
	messageIndex int,
	assistantIndex int,
	blocks []map[string]interface{},
	plan agentCompletionGuardPlan,
) string {
	if graph == nil {
		return "tool graph is unavailable"
	}

	turn := graph.Turns[assistantIndex]
	expected := graph.clientCallsFromMessage(assistantIndex)
	pendingServer := 0
	pendingMCP := 0
	if turn != nil {
		expected = turn.ClientCalls
		pendingServer = len(turn.PendingServerCalls)
		pendingMCP = len(turn.PendingMCPCalls)
	}
	hasProgrammatic := false
	for _, node := range expected {
		if node != nil && node.ParentID != "" {
			hasProgrammatic = true
			break
		}
	}
	requiresResultOnly := hasProgrammatic || pendingServer > 0 || pendingMCP > 0
	if len(expected) == 0 && requiresResultOnly {
		return fmt.Sprintf(
			"messages[%d] cannot be a user continuation while the preceding assistant turn has pending server or MCP calls",
			messageIndex,
		)
	}

	seen := make(map[string]bool)
	seenNonResult := false
	for _, block := range blocks {
		blockType := completionGuardMapString(block, "type")
		if blockType != "tool_result" {
			seenNonResult = true
			if requiresResultOnly {
				reason := "mixed server/client continuation"
				if hasProgrammatic {
					reason = "programmatic continuation"
				}
				return fmt.Sprintf(
					"messages[%d] %s must contain only tool_result blocks",
					messageIndex,
					reason,
				)
			}
			continue
		}
		if seenNonResult {
			return fmt.Sprintf(
				"messages[%d] tool_result blocks must come before other user content",
				messageIndex,
			)
		}

		id := completionGuardMapString(block, "tool_use_id")
		node, exists := expected[id]
		if id == "" || !exists || node == nil ||
			node.State != completionGuardToolStatePending || seen[id] {
			return fmt.Sprintf(
				"messages[%d] has unmatched or duplicate tool_result",
				messageIndex,
			)
		}
		if node.ParentID != "" &&
			!completionGuardProgrammaticResultContentValid(block["content"]) {
			return fmt.Sprintf(
				"messages[%d] programmatic tool_result content must be a string or text blocks",
				messageIndex,
			)
		}
		references, referenceReason := completionGuardParseCustomToolSearchReferences(
			block["content"],
			id,
			messageIndex,
			plan.Catalog,
		)
		if referenceReason != "" {
			return fmt.Sprintf("messages[%d] %s", messageIndex, referenceReason)
		}
		graph.References = append(graph.References, references...)
		seen[id] = true
	}

	if len(expected) == 0 {
		if len(seen) != 0 {
			return fmt.Sprintf("messages[%d] has unmatched tool_result", messageIndex)
		}
		return ""
	}
	if len(seen) != len(expected) {
		return fmt.Sprintf(
			"messages[%d] must return every pending tool result from messages[%d]",
			messageIndex,
			assistantIndex,
		)
	}

	for id := range seen {
		node := expected[id]
		if node.ParentID != "" && !graph.activeCodeExecution(node.ParentID) {
			return fmt.Sprintf(
				"messages[%d] programmatic tool_result parent is no longer active",
				messageIndex,
			)
		}
		node.State = completionGuardToolStateResolved
	}
	return ""
}

func completionGuardMapCaller(
	block map[string]interface{},
) (callerType, callerID string, valid bool) {
	raw, exists := block["caller"]
	if !exists {
		return "direct", "", true
	}
	caller, ok := raw.(map[string]interface{})
	if !ok || caller == nil {
		return "", "", false
	}
	callerType = strings.ToLower(completionGuardMapString(caller, "type"))
	callerID = completionGuardMapString(caller, "tool_id")
	if callerType == "" {
		return "", "", false
	}
	switch callerType {
	case "direct":
		return callerType, callerID, callerID == ""
	case completionGuardCodeCallerType:
		return callerType, callerID, callerID != ""
	default:
		return callerType, callerID, false
	}
}

func completionGuardHistoricalServerDefinition(
	name string,
	blockType string,
	plan agentCompletionGuardPlan,
) (completionGuardToolKind, string, bool, bool) {
	name = strings.TrimSpace(name)
	if blockType == "mcp_tool_use" {
		return completionGuardToolKindMCP, "mcp_tool_result", false, name != ""
	}
	if capability, exists := completionGuardServerToolCapabilityForName(plan, name); exists {
		return completionGuardToolKindServer,
			capability.ResultBlockType,
			capability.CanCallTools,
			capability.ResultBlockType != ""
	}
	resultType, exists := completionGuardKnownServerToolResultType(name)
	return completionGuardToolKindServer,
		resultType,
		name == "code_execution" && plan.Catalog != nil && plan.Catalog.ProgrammaticToolCalling,
		exists
}

func completionGuardBuildHistoricalToolGraph(
	req *ClaudeRequest,
	plan agentCompletionGuardPlan,
) (*completionGuardToolGraph, string) {
	graph := newCompletionGuardToolGraph()
	if req == nil {
		return graph, ""
	}

	for messageIndex, message := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		blocks := claudeContentBlocks(message.Content)
		substantiveBeforeInheritedResult := false
		introducedServerIDs := make([]string, 0)

		switch role {
		case "assistant":
			for _, block := range blocks {
				blockType := completionGuardMapString(block, "type")
				switch {
				case completionGuardServerToolBlockType(blockType):
					id := completionGuardMapString(block, "id")
					name := completionGuardMapString(block, "name")
					kind, resultType, canCallTools, known := completionGuardHistoricalServerDefinition(
						name,
						blockType,
						plan,
					)
					if id == "" || name == "" {
						return graph, fmt.Sprintf("messages[%d] has malformed server tool use", messageIndex)
					}
					if !known {
						return graph, fmt.Sprintf("messages[%d] contains unknown server tool %q", messageIndex, name)
					}
					serverName := ""
					if blockType == "mcp_tool_use" {
						serverName = completionGuardMapString(block, "server_name")
						if serverName == "" {
							return graph, fmt.Sprintf("messages[%d] mcp_tool_use.server_name must be a non-empty string", messageIndex)
						}
					}
					callerType, callerID, callerValid := completionGuardMapCaller(block)
					if !callerValid {
						return graph, fmt.Sprintf("messages[%d] has invalid server tool caller", messageIndex)
					}
					if callerType == completionGuardCodeCallerType {
						if !graph.activeCodeExecution(callerID) {
							return graph, fmt.Sprintf("messages[%d] server tool caller does not reference active code execution", messageIndex)
						}
						capability, declared := completionGuardServerToolCapabilityForName(plan, name)
						if !declared || !capability.AllowCodeCaller {
							return graph, fmt.Sprintf("messages[%d] server tool %q is not code-callable in the current tool catalog", messageIndex, name)
						}
					}
					if reason := graph.add(completionGuardToolNode{
						ID:              id,
						Kind:            kind,
						Name:            name,
						ServerName:      serverName,
						ResultBlockType: resultType,
						ParentID:        callerID,
						CallerType:      callerType,
						CanCallTools:    canCallTools,
						Inherited:       true,
						MessageIndex:    messageIndex,
					}); reason != "" {
						return graph, fmt.Sprintf("messages[%d] %s", messageIndex, reason)
					}
					introducedServerIDs = append(introducedServerIDs, id)
					substantiveBeforeInheritedResult = true

				case blockType == "tool_use":
					id := completionGuardMapString(block, "id")
					name := completionGuardMapString(block, "name")
					callerType, callerID, callerValid := completionGuardMapCaller(block)
					if id == "" || name == "" {
						return graph, fmt.Sprintf("messages[%d] has malformed client tool use", messageIndex)
					}
					if !callerValid {
						return graph, fmt.Sprintf("messages[%d] has invalid client tool caller", messageIndex)
					}
					if callerType == completionGuardCodeCallerType {
						if !graph.activeCodeExecution(callerID) {
							return graph, fmt.Sprintf("messages[%d] programmatic caller does not reference active code execution", messageIndex)
						}
						entry, declared := plan.Catalog.ExplicitEntry(name)
						if !declared || !entry.CodeCallable {
							return graph, fmt.Sprintf("messages[%d] programmatic tool %q is not code-callable in the current tool catalog", messageIndex, name)
						}
					}
					if reason := graph.add(completionGuardToolNode{
						ID:           id,
						Kind:         completionGuardToolKindClient,
						Name:         name,
						ParentID:     callerID,
						CallerType:   callerType,
						Inherited:    true,
						MessageIndex: messageIndex,
					}); reason != "" {
						return graph, fmt.Sprintf("messages[%d] %s", messageIndex, reason)
					}
					substantiveBeforeInheritedResult = true

				case completionGuardServerToolResultBlockType(blockType):
					id := completionGuardMapString(block, "tool_use_id")
					node, exists := graph.activeNode(id)
					if !exists || (node.Kind != completionGuardToolKindServer && node.Kind != completionGuardToolKindMCP) {
						return graph, fmt.Sprintf("messages[%d] has unmatched server tool result", messageIndex)
					}
					if blockType != node.ResultBlockType {
						return graph, fmt.Sprintf("messages[%d] has mismatched server tool result", messageIndex)
					}
					if node.DirectDeferred && substantiveBeforeInheritedResult {
						return graph, fmt.Sprintf("messages[%d] deferred server tool result must be first", messageIndex)
					}
					if blockType == "tool_search_tool_result" {
						references, referenceReason := completionGuardParseBuiltinToolSearchReferences(
							block["content"],
							id,
							messageIndex,
							plan.Catalog,
						)
						if referenceReason != "" {
							return graph, fmt.Sprintf("messages[%d] %s", messageIndex, referenceReason)
						}
						graph.References = append(graph.References, references...)
					}
					if reason := graph.resolve(id); reason != "" {
						return graph, fmt.Sprintf("messages[%d] %s", messageIndex, reason)
					}

				default:
					if blockType != "" && blockType != "compaction" {
						substantiveBeforeInheritedResult = true
					}
				}
			}

			for _, id := range introducedServerIDs {
				node, exists := graph.activeNode(id)
				if !exists || node.ParentID != "" {
					continue
				}
				if !graph.hasChildren(id) {
					node.DirectDeferred = true
				}
			}
			graph.snapshotAssistantTurn(messageIndex)

		case "user":
			assistantIndex := messageIndex - 1
			if assistantIndex >= 0 &&
				strings.EqualFold(strings.TrimSpace(req.Messages[assistantIndex].Role), "assistant") {
				if reason := completionGuardValidateAndResolveUserToolResults(
					graph,
					messageIndex,
					assistantIndex,
					blocks,
					plan,
				); reason != "" {
					return graph, reason
				}
			} else {
				for _, block := range blocks {
					if completionGuardMapString(block, "type") == "tool_result" {
						return graph, fmt.Sprintf(
							"messages[%d] has tool_result without an immediately preceding assistant tool_use turn",
							messageIndex,
						)
					}
				}
			}
		}
	}

	if plan.Catalog != nil {
		for _, node := range graph.Nodes {
			if node == nil || node.Kind != completionGuardToolKindServer {
				continue
			}
			_, declared := completionGuardServerToolCapabilityForName(plan, node.Name)
			if node.State == completionGuardToolStatePending && !declared {
				return graph, fmt.Sprintf(
					"pending server tool %q is missing from the current tool catalog",
					node.Name,
				)
			}
			if completionGuardHistoryRequiresCurrentDeclaration(node.Name) && !declared {
				return graph, fmt.Sprintf(
					"server tool %q from message history is missing from the current tool catalog",
					node.Name,
				)
			}
		}
	}
	for _, node := range graph.Nodes {
		if node == nil || node.Kind != completionGuardToolKindMCP ||
			node.State != completionGuardToolStatePending {
			continue
		}
		if plan.Catalog != nil &&
			!completionGuardMCPToolAllowed(plan.Catalog, node.ServerName, node.Name) {
			return graph, fmt.Sprintf(
				"pending MCP tool %q on server %q is missing or disabled in the current tool catalog",
				node.Name,
				node.ServerName,
			)
		}
	}
	return graph, ""
}

func completionGuardPendingServerTools(req ClaudeRequest) map[string]string {
	graph, _ := completionGuardBuildHistoricalToolGraph(
		&req,
		agentCompletionGuardPlan{Catalog: &completionGuardNormalizedCatalog{ProgrammaticToolCalling: true}},
	)
	// Compatibility helper for legacy unit tests and diagnostics. The strict
	// production paths consume the returned validation reason directly, while
	// this view intentionally exposes whatever exact pending state the graph had
	// reconstructed before an invalid result stopped the scan.
	return graph.pendingServerValues()
}

func (g *completionGuardToolGraph) activeCodeCallers() map[string]bool {
	callers := make(map[string]bool)
	if g == nil {
		return callers
	}
	for id, node := range g.Nodes {
		if node != nil && node.State == completionGuardToolStatePending &&
			node.Kind == completionGuardToolKindServer &&
			node.Name == "code_execution" && node.CanCallTools {
			callers[id] = true
		}
	}
	return callers
}

func completionGuardGraphFromExpectedServerTools(
	expected map[string]string,
	plan agentCompletionGuardPlan,
) *completionGuardToolGraph {
	graph := newCompletionGuardToolGraph()
	for id, value := range expected {
		name, isMCP := completionGuardPendingServerToolDetails(value)
		kind := completionGuardToolKindServer
		resultType := ""
		canCallTools := false
		if isMCP {
			kind = completionGuardToolKindMCP
			resultType = "mcp_tool_result"
		} else if capability, exists := completionGuardServerToolCapabilityForName(plan, name); exists {
			resultType = capability.ResultBlockType
			canCallTools = capability.CanCallTools
		} else {
			resultType, _ = completionGuardKnownServerToolResultType(name)
			canCallTools = name == "code_execution" && plan.Catalog != nil && plan.Catalog.ProgrammaticToolCalling
		}
		_ = graph.add(completionGuardToolNode{
			ID:              id,
			Kind:            kind,
			Name:            name,
			ResultBlockType: resultType,
			CanCallTools:    canCallTools,
			Inherited:       true,
			DirectDeferred:  !canCallTools,
			MessageIndex:    -1,
		})
	}
	return graph
}

func classifyCompletionGuardResponseWithExpectedServerTools(
	resp *ClaudeResponse,
	plan agentCompletionGuardPlan,
	expectedServerTools map[string]string,
) completionGuardOutcome {
	return classifyCompletionGuardResponseWithToolGraph(
		resp,
		plan,
		completionGuardGraphFromExpectedServerTools(expectedServerTools, plan),
	)
}

func classifyCompletionGuardResponseWithRequestHistory(
	resp *ClaudeResponse,
	plan agentCompletionGuardPlan,
	req ClaudeRequest,
) completionGuardOutcome {
	graph, reason := completionGuardBuildHistoricalToolGraph(&req, plan)
	if reason != "" {
		return completionGuardOutcome{
			Kind:   "violation",
			Reason: "invalid_tool_history: " + reason,
		}
	}
	return classifyCompletionGuardResponseWithToolGraph(resp, plan, graph)
}

func classifyCompletionGuardResponseWithToolGraph(
	resp *ClaudeResponse,
	plan agentCompletionGuardPlan,
	historical *completionGuardToolGraph,
) completionGuardOutcome {
	out := completionGuardOutcome{Kind: "violation"}
	if resp == nil {
		out.Reason = "nil_response"
		return out
	}
	if reason := completionGuardValidateResponseEnvelope(resp); reason != "" {
		out.Reason = reason
		return out
	}
	if reason := completionGuardValidateStopDetails(resp); reason != "" {
		out.Reason = reason
		return out
	}
	transition := completionGuardValidateModelTransitionForRequest(
		plan.ResolvedModel,
		resp,
	)
	if !transition.Valid {
		out.Reason = transition.Reason
		return out
	}

	completionGuardDiscardRefusalPartialOutput(resp)
	out.NaturalText = completionGuardNaturalText(resp)
	graph := historical.clone()
	seenServerToolResultIDs := make(map[string]bool)
	seenNonCompaction := false
	seenSubstantive := false

	for _, block := range resp.Content {
		blockType := strings.TrimSpace(block.Type)
		if blockType == "" {
			out.Reason = "missing_content_block_type"
			return out
		}
		if blockType != "compaction" {
			seenNonCompaction = true
		}

		switch {
		case blockType == "compaction":
			if seenNonCompaction {
				out.Reason = "compaction_not_at_start"
				return out
			}
			content, ok := block.Content.(string)
			if !ok || strings.TrimSpace(content) == "" {
				out.Reason = "invalid_compaction_block"
				return out
			}
			out.Compactions++

		case blockType == "fallback":
			if reason := completionGuardValidateFallbackBlock(block); reason != "" {
				out.Reason = reason
				return out
			}
			out.Fallbacks++

		case blockType == "text" || blockType == "connector_text":
			if block.presentKnownFields != nil && !block.hasKnownJSONField("text") {
				out.Reason = blockType + "_missing_content_field"
				return out
			}

		case blockType == "thinking" || blockType == "redacted_thinking":
			if reason := completionGuardValidateThinkingBlock(block); reason != "" {
				out.Reason = reason
				return out
			}

		case blockType == "tool_result":
			out.Reason = "client_tool_result_in_assistant_response"
			return out

		case blockType == "tool_use":
			id := strings.TrimSpace(block.ID)
			if existing, exists := graph.Nodes[id]; exists {
				if existing != nil && existing.Kind != completionGuardToolKindClient {
					out.Reason = "tool_id_kind_collision"
				} else {
					out.Reason = "duplicate_client_tool_id"
				}
				return out
			}
			if reason := completionGuardValidateClientTool(block, plan); reason != "" {
				out.Reason = reason
				return out
			}
			callerID, callerReason := completionGuardValidateClientToolCaller(
				block,
				plan,
				graph.activeCodeCallers(),
			)
			if callerReason != "" {
				out.Reason = callerReason
				return out
			}
			caller, _, _ := completionGuardParseToolCaller(block)
			if reason := graph.add(completionGuardToolNode{
				ID:           id,
				Kind:         completionGuardToolKindClient,
				Name:         strings.TrimSpace(block.Name),
				ParentID:     callerID,
				CallerType:   caller.Type,
				MessageIndex: -1,
			}); reason != "" {
				out.Reason = "tool_id_kind_collision"
				return out
			}
			out.RealTools++
			if plan.RequiredToolName == "" || block.Name == plan.RequiredToolName {
				out.RequiredMatch = true
			}

		case completionGuardServerToolBlockType(blockType):
			out.ServerTools++
			id := strings.TrimSpace(block.ID)
			desiredKind := completionGuardToolKindServer
			if blockType == "mcp_tool_use" {
				desiredKind = completionGuardToolKindMCP
			}
			if existing, exists := graph.Nodes[id]; exists {
				if existing != nil && existing.Kind != desiredKind {
					out.Reason = "tool_id_kind_collision"
				} else {
					out.Reason = "duplicate_server_tool_id"
				}
				return out
			}
			if reason := completionGuardValidateServerTool(block, plan); reason != "" {
				out.Reason = reason
				return out
			}
			capability, exists := completionGuardServerToolCapabilityForName(plan, block.Name)
			kind := completionGuardToolKindServer
			resultType := capability.ResultBlockType
			canCallTools := capability.CanCallTools
			if blockType == "mcp_tool_use" {
				kind = completionGuardToolKindMCP
				resultType = "mcp_tool_result"
			}
			if !exists && blockType != "mcp_tool_use" {
				out.Reason = "unknown_server_tool"
				return out
			}
			if reason := completionGuardValidateServerToolCaller(
				block,
				capability,
				graph.activeCodeCallers(),
			); reason != "" {
				out.Reason = reason
				return out
			}
			caller, _, _ := completionGuardParseToolCaller(block)
			parentID := ""
			if caller.Type == completionGuardCodeCallerType {
				parentID = caller.ToolID
			}
			serverName := ""
			if blockType == "mcp_tool_use" {
				serverName = strings.TrimSpace(
					completionGuardContentBlockExtraString(block, "server_name"),
				)
			}
			if reason := graph.add(completionGuardToolNode{
				ID:              id,
				Kind:            kind,
				Name:            strings.TrimSpace(block.Name),
				ServerName:      serverName,
				ResultBlockType: resultType,
				ParentID:        parentID,
				CallerType:      caller.Type,
				CanCallTools:    canCallTools,
				MessageIndex:    -1,
			}); reason != "" {
				out.Reason = "duplicate_server_tool_id"
				return out
			}
			if completionGuardServerToolSatisfiesChoice(block.Name, plan) {
				out.RequiredMatch = true
			}

		case completionGuardServerToolResultBlockType(blockType):
			out.ServerToolResults++
			id := strings.TrimSpace(block.ToolUseID)
			if id != "" && seenServerToolResultIDs[id] {
				out.Reason = "duplicate_server_tool_result"
				return out
			}
			node, pending := graph.activeNode(id)
			if id == "" || !pending ||
				(node.Kind != completionGuardToolKindServer && node.Kind != completionGuardToolKindMCP) {
				out.UnmatchedServerToolResults++
				continue
			}
			if blockType != node.ResultBlockType {
				out.Reason = "server_tool_result_type_mismatch"
				return out
			}
			if node.Inherited && node.DirectDeferred && seenSubstantive {
				out.Reason = "inherited_server_tool_result_not_first"
				return out
			}
			if graph.hasPendingChildren(id) {
				out.Reason = "server_tool_parent_closed_with_pending_children"
				return out
			}
			if blockType == "tool_search_tool_result" {
				references, referenceReason := completionGuardParseBuiltinToolSearchReferences(
					block.Content,
					id,
					-1,
					plan.Catalog,
				)
				if referenceReason != "" {
					out.Reason = "invalid_tool_reference"
					return out
				}
				graph.References = append(graph.References, references...)
			}
			if reason := graph.resolve(id); reason != "" {
				out.Reason = "server_tool_parent_closed_with_pending_children"
				return out
			}
			seenServerToolResultIDs[id] = true

		default:
			out.Reason = "unsupported_content_block"
			return out
		}

		if !(completionGuardServerToolResultBlockType(blockType) &&
			func() bool {
				node := graph.Nodes[strings.TrimSpace(block.ToolUseID)]
				return node != nil && node.Inherited && node.DirectDeferred
			}()) {
			seenSubstantive = true
		}
	}

	out.PendingServerTools = graph.pendingServerCount()
	stopReason := strings.ToLower(strings.TrimSpace(resp.StopReason))
	if stopReason == "stop_sequence" {
		if resp.StopSequence == nil || strings.TrimSpace(*resp.StopSequence) == "" {
			out.Reason = "missing_stop_sequence"
			return out
		}
	} else if resp.StopSequence != nil {
		out.Reason = "unexpected_stop_sequence"
		return out
	}

	invalidPendingServerTools := false
	if out.PendingServerTools > 0 {
		validMixedTurn := stopReason == "tool_use" && out.RealTools > 0
		if validMixedTurn {
			for _, node := range graph.Nodes {
				if node == nil || node.State != completionGuardToolStatePending || !node.Inherited ||
					(node.Kind != completionGuardToolKindServer && node.Kind != completionGuardToolKindMCP) {
					continue
				}
				allowedByProgrammaticPause := graph.hasPendingClientChild(node.ID)
				if !allowedByProgrammaticPause && node.ParentID != "" {
					allowedByProgrammaticPause = graph.hasPendingClientChild(node.ParentID)
				}
				if !allowedByProgrammaticPause {
					validMixedTurn = false
					break
				}
			}
		}
		invalidPendingServerTools = !validMixedTurn
	}

	switch {
	case (stopReason == "max_tokens" || stopReason == "model_context_window_exceeded") && out.RealTools > 0:
		// A token/context limit while constructing a client tool call can leave
		// arguments incomplete, so this remains unsafe to expose as a valid tool
		// invocation. Text-only terminal-limit responses, however, are successful
		// Claude responses and must preserve their upstream stop reason.
		out.Reason = "truncated_response"
	case out.UnmatchedServerToolResults > 0:
		out.Reason = "unmatched_server_tool_result"
	case stopReason == "compaction":
		if out.Compactions == 0 {
			out.Reason = "compaction_stop_without_block"
		} else if out.Compactions != len(resp.Content) {
			out.Reason = "compaction_stop_with_other_content"
		} else {
			out.Reason = "compaction_requires_continuation"
		}
	case stopReason == "pause_turn":
		out.Reason = "pause_turn_requires_continuation"
	case invalidPendingServerTools:
		out.Reason = "unresolved_server_tool_use"
	case out.RealTools > 0 && plan.ToolChoiceKind == "none":
		out.Reason = "client_tool_not_allowed"
	case out.RealTools > 0 && stopReason != "tool_use":
		out.Reason = "client_tool_with_invalid_stop_reason"
	case out.RealTools > 0 && plan.ToolChoiceKind == "specific" && !out.RequiredMatch:
		out.Reason = "required_tool_not_called"
	case out.RealTools > 0:
		out.Accepted = true
		out.Kind = "real_tool"
	case stopReason == "tool_use":
		out.Reason = "tool_use_without_client_tool"
	case stopReason == "refusal":
		out.Accepted = true
		out.Kind = "refusal"
	case stopReason == "max_tokens" || stopReason == "model_context_window_exceeded":
		if strings.TrimSpace(out.NaturalText) != "" {
			out.Accepted = true
			out.Kind = "prose"
		} else if out.ServerTools > 0 || out.ServerToolResults > 0 {
			out.Accepted = true
			out.Kind = "server_tool"
		} else {
			out.Reason = "empty_response_without_tool"
		}
	case (plan.ToolChoiceKind == "any" || plan.ToolChoiceKind == "specific") && !out.RequiredMatch:
		out.Reason = "required_tool_not_called"
	case stopReason == "end_turn" || stopReason == "stop_sequence":
		if strings.TrimSpace(out.NaturalText) != "" {
			out.Accepted = true
			out.Kind = "prose"
		} else if out.ServerTools > 0 || out.ServerToolResults > 0 {
			out.Accepted = true
			out.Kind = "server_tool"
		} else if out.Compactions > 0 {
			out.Reason = "compaction_without_completion"
		} else {
			out.Reason = "empty_response_without_tool"
		}
	case stopReason == "":
		out.Reason = "missing_stop_reason"
	default:
		out.Reason = "unexpected_stop_reason"
	}

	return out
}

func completionGuardValidateProgrammaticContinuation(
	req *ClaudeRequest,
	plan agentCompletionGuardPlan,
	graph *completionGuardToolGraph,
) string {
	if req == nil || len(req.Messages) < 2 || graph == nil {
		return ""
	}
	userIndex := len(req.Messages) - 1
	if !strings.EqualFold(strings.TrimSpace(req.Messages[userIndex].Role), "user") {
		return ""
	}
	assistantIndex := userIndex - 1
	if !strings.EqualFold(strings.TrimSpace(req.Messages[assistantIndex].Role), "assistant") {
		return ""
	}

	pendingCalls := make(map[string]*completionGuardToolNode)
	hasProgrammatic := false
	for id, node := range graph.Nodes {
		if node == nil || node.Kind != completionGuardToolKindClient ||
			node.MessageIndex != assistantIndex {
			continue
		}
		pendingCalls[id] = node
		if node.ParentID != "" {
			hasProgrammatic = true
		}
	}
	if !hasProgrammatic {
		return ""
	}

	blocks := claudeContentBlocks(req.Messages[userIndex].Content)
	if len(blocks) == 0 {
		return fmt.Sprintf(
			"messages[%d] programmatic continuation must contain only tool_result blocks",
			userIndex,
		)
	}
	seen := make(map[string]bool)
	for _, block := range blocks {
		if completionGuardMapString(block, "type") != "tool_result" {
			return fmt.Sprintf(
				"messages[%d] programmatic continuation must contain only tool_result blocks",
				userIndex,
			)
		}
		id := completionGuardMapString(block, "tool_use_id")
		call, exists := pendingCalls[id]
		if id == "" || !exists || seen[id] {
			return fmt.Sprintf(
				"messages[%d] has unmatched or duplicate programmatic tool_result",
				userIndex,
			)
		}
		if call.ParentID != "" &&
			!completionGuardProgrammaticResultContentValid(block["content"]) {
			return fmt.Sprintf(
				"messages[%d] programmatic tool_result content must be a string or text blocks",
				userIndex,
			)
		}
		if call.ParentID != "" && !graph.activeCodeExecution(call.ParentID) {
			return fmt.Sprintf(
				"messages[%d] programmatic tool_result parent is no longer active",
				userIndex,
			)
		}
		seen[id] = true
	}
	if len(seen) != len(pendingCalls) {
		return fmt.Sprintf(
			"messages[%d] must return every pending programmatic tool result",
			userIndex,
		)
	}

	for _, call := range pendingCalls {
		if call.ParentID == "" {
			continue
		}
		entry, exists := plan.Catalog.ExplicitEntry(call.Name)
		if !exists {
			return fmt.Sprintf(
				"programmatic continuation is missing tool definition %q",
				call.Name,
			)
		}
		if !entry.CodeCallable {
			return fmt.Sprintf(
				"programmatic continuation tool %q is no longer code-callable",
				call.Name,
			)
		}
	}
	if completionGuardRequestContainerID(req) == "" {
		return "programmatic continuation requires a non-empty container id"
	}
	if plan.Catalog == nil || !plan.Catalog.ProgrammaticToolCalling {
		return "programmatic continuation requires code_execution_20260120 or later"
	}
	return ""
}
