package config

// Clone returns a deep copy of Account. Account values contain slices/maps, so
// a plain struct copy is not an immutable request snapshot: it would still share
// nested backing storage with the config/pool-owned record.
func (a Account) Clone() Account {
	out := a
	if a.Tags != nil {
		out.Tags = append([]string(nil), a.Tags...)
	}
	if a.BedrockRegions != nil {
		out.BedrockRegions = append([]string(nil), a.BedrockRegions...)
	}
	if a.BedrockModelMap != nil {
		out.BedrockModelMap = make(map[string]string, len(a.BedrockModelMap))
		for key, value := range a.BedrockModelMap {
			out.BedrockModelMap[key] = value
		}
	}
	return out
}

// Clone returns a deep copy of Config suitable for copy-on-write persistence.
// Every reference-backed field is detached so staged mutations cannot alter the
// authoritative live config before the new file image is published.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := *c
	if c.ApiKeys != nil {
		out.ApiKeys = append([]ApiKeyEntry(nil), c.ApiKeys...)
	}
	if c.Accounts != nil {
		out.Accounts = make([]Account, len(c.Accounts))
		for i := range c.Accounts {
			out.Accounts[i] = c.Accounts[i].Clone()
		}
	}
	if c.EndpointFallback != nil {
		value := *c.EndpointFallback
		out.EndpointFallback = &value
	}
	if c.AutoRecoverEnabled != nil {
		value := *c.AutoRecoverEnabled
		out.AutoRecoverEnabled = &value
	}
	if c.ProxyURLs != nil {
		out.ProxyURLs = append([]string(nil), c.ProxyURLs...)
	}
	if c.PromptFilterRules != nil {
		out.PromptFilterRules = append([]PromptFilterRule(nil), c.PromptFilterRules...)
	}
	return &out
}
