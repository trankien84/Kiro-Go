package config

// GetAgentToolSteering reports whether extra tool-execution steering prompts
// are enabled. The zero-value/default is deliberately false.
func GetAgentToolSteering() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg != nil && cfg.AgentToolSteering
}

// UpdateAgentToolSteering changes the setting transactionally so memory is not
// published ahead of the visible configuration file.
func UpdateAgentToolSteering(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	_, err := publishConfigFieldsLocked(func(next *Config) error {
		next.AgentToolSteering = enabled
		return nil
	})
	return err
}
