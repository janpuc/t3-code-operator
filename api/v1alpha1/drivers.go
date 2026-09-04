package v1alpha1

// BuiltInDriver describes one upstream t3 provider driver that the operator
// knows by a short provider name.
type BuiltInDriver struct {
	// Name is the short provider name used as a Workstation providers key or a
	// Harness name.
	Name string
	// Driver is the upstream t3 driver slug.
	Driver string
	// DisplayName is the label t3 shows when a provider sets none.
	DisplayName string
}

var builtInDrivers = []BuiltInDriver{
	{Name: "codex", Driver: "codex", DisplayName: "Codex"},
	{Name: "claude", Driver: "claudeAgent", DisplayName: "Claude"},
	{Name: "opencode", Driver: "opencode", DisplayName: "OpenCode"},
	{Name: "cursor", Driver: "cursor", DisplayName: "Cursor"},
	{Name: "grok", Driver: "grok", DisplayName: "Grok"},
	{Name: "antigravity", Driver: "antigravity", DisplayName: "Antigravity"},
}

// BuiltInDrivers returns the drivers the operator resolves from a provider
// name, in presentation order.
func BuiltInDrivers() []BuiltInDriver {
	return append([]BuiltInDriver(nil), builtInDrivers...)
}

// BuiltInDriverForName resolves a short provider name such as "claude" to its
// upstream driver.
func BuiltInDriverForName(name string) (BuiltInDriver, bool) {
	for _, driver := range builtInDrivers {
		if driver.Name == name {
			return driver, true
		}
	}
	return BuiltInDriver{}, false
}

// DisplayNameForDriver returns the default display name for an upstream driver
// slug, or an empty string for a driver the operator does not know.
func DisplayNameForDriver(driver string) string {
	for _, candidate := range builtInDrivers {
		if candidate.Driver == driver {
			return candidate.DisplayName
		}
	}
	return ""
}
