package proxy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type completionGuardSchemaNode struct {
	Value      interface{}
	Edges      []string
	BaseURL    *url.URL
	CanResolve bool
}

type completionGuardSchemaResource struct {
	Pointer   string
	Ambiguous bool
}

type completionGuardSchemaChild struct {
	Pointer string
	Value   interface{}
	Applies bool
}

func completionGuardJSONPointerEscape(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	return strings.ReplaceAll(value, "/", "~1")
}

func completionGuardJSONPointerUnescape(value string) (string, error) {
	var out strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '~' {
			out.WriteByte(value[index])
			continue
		}
		if index+1 >= len(value) {
			return "", fmt.Errorf("contains malformed local $ref JSON Pointer")
		}
		switch value[index+1] {
		case '0':
			out.WriteByte('~')
		case '1':
			out.WriteByte('/')
		default:
			return "", fmt.Errorf("contains malformed local $ref JSON Pointer")
		}
		index++
	}
	return out.String(), nil
}

func completionGuardSchemaURIWithoutFragment(value *url.URL) string {
	if value == nil {
		return ""
	}
	copyValue := *value
	copyValue.Fragment = ""
	copyValue.RawFragment = ""
	return copyValue.String()
}

func completionGuardSchemaPointerJoin(base string, child string) string {
	if base == "" {
		return child
	}
	if child == "" {
		return base
	}
	return base + child
}

func completionGuardSchemaValue(value interface{}) bool {
	switch value.(type) {
	case bool, map[string]interface{}:
		return true
	default:
		return false
	}
}

func completionGuardSchemaObjectChildren(
	value map[string]interface{},
	pointer string,
) []completionGuardSchemaChild {
	children := make([]completionGuardSchemaChild, 0)
	addSingle := func(keyword string, applies bool) {
		child, exists := value[keyword]
		if !exists || !completionGuardSchemaValue(child) {
			return
		}
		children = append(children, completionGuardSchemaChild{
			Pointer: pointer + "/" + completionGuardJSONPointerEscape(keyword),
			Value:   child,
			Applies: applies,
		})
	}
	addArray := func(keyword string, applies bool) {
		values, exists := value[keyword].([]interface{})
		if !exists {
			return
		}
		for index, child := range values {
			if !completionGuardSchemaValue(child) {
				continue
			}
			children = append(children, completionGuardSchemaChild{
				Pointer: pointer + "/" + completionGuardJSONPointerEscape(keyword) + "/" + strconv.Itoa(index),
				Value:   child,
				Applies: applies,
			})
		}
	}
	addMap := func(keyword string, applies bool) {
		values, exists := value[keyword].(map[string]interface{})
		if !exists {
			return
		}
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := values[key]
			if !completionGuardSchemaValue(child) {
				continue
			}
			children = append(children, completionGuardSchemaChild{
				Pointer: pointer + "/" + completionGuardJSONPointerEscape(keyword) + "/" + completionGuardJSONPointerEscape(key),
				Value:   child,
				Applies: applies,
			})
		}
	}

	for _, keyword := range []string{
		"additionalItems",
		"additionalProperties",
		"contains",
		"if",
		"not",
		"propertyNames",
		"unevaluatedItems",
		"unevaluatedProperties",
	} {
		addSingle(keyword, true)
	}
	_, hasIf := value["if"]
	hasIf = hasIf && completionGuardSchemaValue(value["if"])
	addSingle("then", hasIf)
	addSingle("else", hasIf)
	// contentSchema is a schema location, but the content vocabulary treats it
	// as annotation unless an implementation opts into separate content validation.
	addSingle("contentSchema", false)

	if items, exists := value["items"]; exists {
		switch typed := items.(type) {
		case []interface{}:
			for index, child := range typed {
				if !completionGuardSchemaValue(child) {
					continue
				}
				children = append(children, completionGuardSchemaChild{
					Pointer: pointer + "/items/" + strconv.Itoa(index),
					Value:   child,
					Applies: true,
				})
			}
		default:
			if completionGuardSchemaValue(items) {
				children = append(children, completionGuardSchemaChild{
					Pointer: pointer + "/items",
					Value:   items,
					Applies: true,
				})
			}
		}
	}

	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		addArray(keyword, true)
	}
	for _, keyword := range []string{
		"dependentSchemas",
		"patternProperties",
		"properties",
	} {
		addMap(keyword, true)
	}
	// $defs/definitions reserve schema locations but do not apply them by
	// themselves. References into those locations add the evaluation edge.
	addMap("$defs", false)
	addMap("definitions", false)

	// In older drafts, dependencies may contain either a schema or a string array.
	// Only schema-valued entries participate in reference-cycle detection.
	addMap("dependencies", true)

	sort.Slice(children, func(i, j int) bool {
		return children[i].Pointer < children[j].Pointer
	})
	return children
}

func completionGuardSchemaRegisterResource(
	resources map[string]completionGuardSchemaResource,
	key string,
	pointer string,
) {
	if key == "" {
		return
	}
	current, exists := resources[key]
	if !exists {
		resources[key] = completionGuardSchemaResource{Pointer: pointer}
		return
	}
	if current.Pointer != pointer {
		current.Ambiguous = true
		resources[key] = current
	}
}

func completionGuardSchemaApplyID(
	value map[string]interface{},
	inheritedBase *url.URL,
	inheritedResourceKey string,
	inheritedResourceRoot string,
	pointer string,
) (base *url.URL, resourceKey string, resourceRoot string, canResolve bool, aliases []string) {
	base = inheritedBase
	resourceKey = inheritedResourceKey
	resourceRoot = inheritedResourceRoot
	canResolve = inheritedBase != nil && inheritedResourceKey != ""

	rawID, exists := value["$id"]
	if !exists {
		return base, resourceKey, resourceRoot, canResolve, nil
	}
	id, ok := rawID.(string)
	id = strings.TrimSpace(id)
	if !ok || id == "" || inheritedBase == nil {
		return nil, "", pointer, false, nil
	}
	parsed, err := url.Parse(id)
	if err != nil {
		return nil, "", pointer, false, nil
	}
	resolved := inheritedBase.ResolveReference(parsed)
	if resolved == nil || resolved.Scheme == "" {
		return nil, "", pointer, false, nil
	}
	if resolved.Fragment != "" {
		aliases = append(aliases, resolved.String())
	}
	resolvedCopy := *resolved
	resolvedCopy.Fragment = ""
	resolvedCopy.RawFragment = ""
	base = &resolvedCopy
	resourceKey = resolvedCopy.String()
	resourceRoot = pointer
	canResolve = resourceKey != ""
	return base, resourceKey, resourceRoot, canResolve, aliases
}

func completionGuardSchemaCanonicalFragment(fragment string) (string, error) {
	if fragment == "" {
		return "", nil
	}
	if !strings.HasPrefix(fragment, "/") {
		return "", fmt.Errorf("contains malformed local $ref JSON Pointer")
	}
	parts := strings.Split(strings.TrimPrefix(fragment, "/"), "/")
	canonical := ""
	for _, part := range parts {
		decoded, err := completionGuardJSONPointerUnescape(part)
		if err != nil {
			return "", err
		}
		canonical += "/" + completionGuardJSONPointerEscape(decoded)
	}
	return canonical, nil
}

func completionGuardSchemaResolvedFragment(value *url.URL) (string, error) {
	if value == nil {
		return "", nil
	}
	escaped := value.EscapedFragment()
	if escaped == "" {
		return value.Fragment, nil
	}
	fragment, err := url.PathUnescape(escaped)
	if err != nil {
		return "", fmt.Errorf("contains malformed local $ref")
	}
	return fragment, nil
}

func completionGuardInputSchemaHasReferenceCycle(
	schema interface{},
) (bool, string) {
	if schema == nil {
		return false, ""
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return false, "cannot be normalized as JSON"
	}
	var root interface{}
	if err := json.Unmarshal(encoded, &root); err != nil {
		return false, "cannot be normalized as JSON"
	}
	if _, ok := root.(map[string]interface{}); !ok {
		return false, "must be a JSON object"
	}

	syntheticBase, _ := url.Parse("https://completion-guard.invalid/input-schema")
	nodes := make(map[string]*completionGuardSchemaNode)
	resources := make(map[string]completionGuardSchemaResource)
	anchors := make(map[string]completionGuardSchemaResource)

	type pendingNode struct {
		Pointer      string
		Value        interface{}
		BaseURL      *url.URL
		ResourceKey  string
		ResourceRoot string
	}
	pending := []pendingNode{{
		Pointer:      "",
		Value:        root,
		BaseURL:      syntheticBase,
		ResourceKey:  syntheticBase.String(),
		ResourceRoot: "",
	}}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]

		base := current.BaseURL
		resourceKey := current.ResourceKey
		resourceRoot := current.ResourceRoot
		canResolve := base != nil && resourceKey != ""
		var aliases []string
		if object, ok := current.Value.(map[string]interface{}); ok {
			base, resourceKey, resourceRoot, canResolve, aliases = completionGuardSchemaApplyID(
				object,
				base,
				resourceKey,
				resourceRoot,
				current.Pointer,
			)
		}

		node := &completionGuardSchemaNode{
			Value:      current.Value,
			BaseURL:    base,
			CanResolve: canResolve,
		}
		nodes[current.Pointer] = node
		if canResolve {
			completionGuardSchemaRegisterResource(resources, resourceKey, resourceRoot)
			for _, alias := range aliases {
				completionGuardSchemaRegisterResource(anchors, alias, current.Pointer)
			}
		}

		if object, ok := current.Value.(map[string]interface{}); ok {
			if canResolve {
				for _, keyword := range []string{"$anchor", "$dynamicAnchor"} {
					anchor, ok := object[keyword].(string)
					anchor = strings.TrimSpace(anchor)
					if !ok || anchor == "" || strings.Contains(anchor, "#") {
						continue
					}
					key := resourceKey + "#" + anchor
					completionGuardSchemaRegisterResource(anchors, key, current.Pointer)
				}
			}

			children := completionGuardSchemaObjectChildren(object, current.Pointer)
			for index := len(children) - 1; index >= 0; index-- {
				child := children[index]
				if child.Applies {
					node.Edges = append(node.Edges, child.Pointer)
				}
				pending = append(pending, pendingNode{
					Pointer:      child.Pointer,
					Value:        child.Value,
					BaseURL:      base,
					ResourceKey:  resourceKey,
					ResourceRoot: resourceRoot,
				})
			}
		}
	}

	pointers := make([]string, 0, len(nodes))
	for pointer := range nodes {
		pointers = append(pointers, pointer)
	}
	sort.Strings(pointers)

	for _, pointer := range pointers {
		node := nodes[pointer]
		value, ok := node.Value.(map[string]interface{})
		if !ok {
			continue
		}
		refValue, exists := value["$ref"]
		if !exists {
			continue
		}
		ref, ok := refValue.(string)
		ref = strings.TrimSpace(ref)
		if !ok || ref == "" {
			return false, fmt.Sprintf("at %s contains malformed $ref", pointer)
		}
		if !node.CanResolve || node.BaseURL == nil {
			// A malformed or unsupported $id already makes this schema an upstream
			// validation concern. Avoid inventing a local target and false-rejecting it.
			continue
		}
		parsed, err := url.Parse(ref)
		if err != nil {
			return false, fmt.Sprintf("at %s contains malformed $ref", pointer)
		}
		resolved := node.BaseURL.ResolveReference(parsed)
		resourceKey := completionGuardSchemaURIWithoutFragment(resolved)
		resource, local := resources[resourceKey]
		if !local || resource.Ambiguous {
			// References outside the request-local schema document remain the
			// upstream validator's responsibility.
			continue
		}
		fragment, err := completionGuardSchemaResolvedFragment(resolved)
		if err != nil {
			return false, fmt.Sprintf("at %s %s", pointer, err)
		}

		target := ""
		targetFound := false
		switch {
		case fragment == "":
			target = resource.Pointer
			targetFound = true
		case strings.HasPrefix(fragment, "/"):
			canonical, err := completionGuardSchemaCanonicalFragment(fragment)
			if err != nil {
				return false, fmt.Sprintf("at %s %s", pointer, err)
			}
			target = completionGuardSchemaPointerJoin(resource.Pointer, canonical)
			_, targetFound = nodes[target]
		default:
			anchor, exists := anchors[resourceKey+"#"+fragment]
			if exists && !anchor.Ambiguous {
				target = anchor.Pointer
				targetFound = true
			}
		}
		if !targetFound {
			return false, fmt.Sprintf("at %s contains unresolved local $ref %q", pointer, ref)
		}
		if _, exists := nodes[target]; !exists {
			return false, fmt.Sprintf("at %s contains unresolved local $ref %q", pointer, ref)
		}
		node.Edges = append(node.Edges, target)
	}

	for _, node := range nodes {
		sort.Strings(node.Edges)
		unique := node.Edges[:0]
		for _, edge := range node.Edges {
			if len(unique) == 0 || unique[len(unique)-1] != edge {
				unique = append(unique, edge)
			}
		}
		node.Edges = unique
	}

	const (
		unvisited = uint8(0)
		visiting  = uint8(1)
		complete  = uint8(2)
	)
	state := make(map[string]uint8, len(nodes))
	type frame struct {
		Pointer string
		Next    int
	}
	for _, start := range pointers {
		if state[start] != unvisited {
			continue
		}
		state[start] = visiting
		stack := []frame{{Pointer: start}}
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			node := nodes[top.Pointer]
			if top.Next >= len(node.Edges) {
				state[top.Pointer] = complete
				stack = stack[:len(stack)-1]
				continue
			}
			target := node.Edges[top.Next]
			top.Next++
			switch state[target] {
			case visiting:
				return true, ""
			case complete:
				continue
			default:
				state[target] = visiting
				stack = append(stack, frame{Pointer: target})
			}
		}
	}
	return false, ""
}
