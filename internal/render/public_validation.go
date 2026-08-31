package render

func ValidateHarness(namespace string, harness Harness) (SupportLevel, error) {
	manifest, err := Render(ResolvedWorkstation{
		Namespace: namespace,
		Name:      "validation",
		UID:       "validation",
		Harnesses: []Harness{harness},
	})
	if err != nil {
		return "", err
	}
	return manifest.ProviderInstances[harness.InstanceID].SupportLevel, nil
}

func ValidateExtension(namespace string, extension Extension) error {
	return validateExtensions(namespace, []Extension{extension})
}

func ValidateMCPServer(namespace string, server MCPServer) error {
	_, err := normalizeMCPServers(namespace, Harness{
		InstanceID: "validation",
		MCPServers: []MCPServer{server},
	})
	return err
}

func ProgramsMCPServers(driver string) bool {
	adapter, err := adapterFor(driver)
	return err == nil && adapter.programsMCPServers()
}

func ProgramsExtensionSource(driver string, sourceType ExtensionSourceType) bool {
	adapter, err := adapterFor(driver)
	if err != nil {
		return false
	}
	dialect, supported := adapter.extensionDialect("capability")
	return supported && dialect.programsSource(sourceType)
}
