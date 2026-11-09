package workerapi

// Kind builds the name of a kind of job, "<provider>.<action>".
func Kind(provider, action string) string { return provider + "." + action }

// The providers that ship with the program. A worker package and the backend's descriptor for a
// provider agree on its name and its kinds through these.
const (
	ProviderWithings      = "withings"
	ProviderStrengthLevel = "strengthlevel"
)
